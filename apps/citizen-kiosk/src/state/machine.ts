// Pure UI state machine. All side effects live in the controller; this file
// only decides what the screen shows. The agent is the authority on session
// state — the reducer mirrors snapshots from responses and SSE events.

import { hasSession, safeAudioUrl } from "../api/client";
import type { ConnState } from "../api/events";
import type {
  Capabilities,
  DeviceStatus,
  HealthState,
  Lang,
  Reply,
  SessionEvent,
  SessionSnapshot,
  Transcript,
  TurnResult,
} from "../api/types";

export type Screen = "boot" | "welcome" | "language" | "privacy" | "conversation" | "ended";

/** Local voice pipeline state (what the browser is doing). */
export type Voice = "idle" | "opening" | "recording" | "uploading" | "playing";

export type Overlay = "none" | "reminder" | "final_warning";

export type NoticeKey =
  | "error"
  | "timeout"
  | "micBlocked"
  | "buttonDisabled"
  | "tooLong"
  | "unavailable";

export interface UiState {
  screen: Screen;
  conn: ConnState;
  overall: HealthState | null;
  caps: Capabilities | null;
  session: SessionSnapshot | null;
  lang: Lang;
  reply: Reply | null;
  transcript: Transcript | null;
  lastAudioUrl: string | null;
  /** Greeting from POST /session/start, shown after the privacy notice. */
  greeting: { reply: Reply; audioUrl: string | null } | null;
  voice: Voice;
  level: number;
  busy: boolean;
  overlay: Overlay;
  notice: NoticeKey | null;
  /** getUserMedia failed in this session → text only until the session ends. */
  micBlocked: boolean;
  /** Increments on every new reply so the view can scroll/announce. */
  replySeq: number;
  /** Goodbye reply shown on the ended screen (from POST /end or end_session). */
  farewell: Reply | null;
}

export const initialState: UiState = {
  screen: "boot",
  conn: "connecting",
  overall: null,
  caps: null,
  session: null,
  lang: "hi",
  reply: null,
  transcript: null,
  lastAudioUrl: null,
  greeting: null,
  voice: "idle",
  level: 0,
  busy: false,
  overlay: "none",
  notice: null,
  micBlocked: false,
  replySeq: 0,
  farewell: null,
};

export type Action =
  | { type: "conn"; conn: ConnState }
  | { type: "status"; status: DeviceStatus }
  | { type: "health"; overall: HealthState; caps: Capabilities }
  | { type: "sessionEvent"; ev: SessionEvent }
  | { type: "session"; session: SessionSnapshot }
  | { type: "started"; session: SessionSnapshot; reply: Reply | null; audioUrl: string | null }
  | { type: "languageChosen"; lang: Lang; keepGreeting: boolean }
  | { type: "greeting"; sessionId: string; reply: Reply; audioUrl: string | null }
  | { type: "privacyAck" }
  | { type: "showReply"; reply: Reply; audioUrl: string | null; transcript?: Transcript | null }
  | { type: "turn"; result: TurnResult; audioUrl: string | null }
  | { type: "voice"; voice: Voice }
  | { type: "level"; level: number }
  | { type: "busy"; busy: boolean }
  | { type: "overlayClear" }
  | { type: "notice"; notice: NoticeKey | null }
  | { type: "micBlocked" }
  | { type: "farewell"; reply: Reply }
  | { type: "ended" }
  | { type: "reset" };

const inSession = (s: Screen) => s === "language" || s === "privacy" || s === "conversation";

/** Fields cleared whenever a session ends (privacy: drop all session content). */
const sessionCleared = {
  reply: null,
  transcript: null,
  lastAudioUrl: null,
  greeting: null,
  voice: "idle",
  level: 0,
  busy: false,
  overlay: "none",
  notice: null,
  micBlocked: false,
} as const satisfies Partial<UiState>;

function applySnapshot(state: UiState, session: SessionSnapshot): UiState {
  const open = hasSession(session);
  const lang = open && session.language ? session.language : state.lang;
  if (open && (state.screen === "welcome" || state.screen === "boot" || state.screen === "ended")) {
    // Session started elsewhere (GPIO button) or UI reloaded mid-session.
    return {
      ...state,
      ...sessionCleared,
      session,
      lang,
      farewell: null,
      screen: session.turns > 0 ? "conversation" : "language",
    };
  }
  if (!open && inSession(state.screen)) {
    return { ...state, ...sessionCleared, session, screen: "ended" };
  }
  if (open && state.session?.id && session.id !== state.session.id && inSession(state.screen)) {
    // A different session replaced ours: start over on its language screen.
    return { ...state, ...sessionCleared, session, lang, screen: "language" };
  }
  return { ...state, session, lang };
}

export function reducer(state: UiState, a: Action): UiState {
  switch (a.type) {
    case "conn":
      return { ...state, conn: a.conn };

    case "status": {
      const s = { ...state, overall: a.status.overall, caps: a.status.capabilities };
      const next = applySnapshot(s, a.status.session);
      if (next.screen === "boot") return { ...next, screen: "welcome" };
      return next;
    }

    case "health":
      return { ...state, overall: a.overall, caps: a.caps };

    case "sessionEvent": {
      const { ev } = a;
      if (ev.type === "session.ended") {
        if (state.screen === "ended" || state.screen === "welcome" || state.screen === "boot") {
          return { ...state, ...sessionCleared, session: ev.session };
        }
        return { ...state, ...sessionCleared, session: ev.session, screen: "ended" };
      }
      // Ignore events for a session other than ours unless we have none.
      if (state.session?.id && ev.session.id && ev.session.id !== state.session.id) {
        if (ev.type !== "session.started") return state;
      }
      let next = applySnapshot(state, ev.session);
      if (!inSession(next.screen)) return next;
      if (
        ev.type === "session.started" &&
        ev.reply &&
        !next.greeting &&
        (next.screen === "language" || next.screen === "privacy")
      ) {
        // Physical-button start: the greeting rides on the SSE event.
        next = { ...next, greeting: { reply: ev.reply, audioUrl: safeAudioUrl(ev.audio_url) } };
      }
      if (ev.type === "session.reminder") {
        return next.overlay === "final_warning" ? next : { ...next, overlay: "reminder" };
      }
      if (ev.type === "session.final_warning") return { ...next, overlay: "final_warning" };
      return next;
    }

    case "session":
      return applySnapshot(state, a.session);

    case "started":
      return {
        ...state,
        ...sessionCleared,
        session: a.session,
        lang: a.session.language ?? state.lang,
        screen: "language",
        greeting: a.reply ? { reply: a.reply, audioUrl: a.audioUrl } : null,
        farewell: null,
      };

    case "languageChosen":
      if (state.screen !== "language") return state;
      return {
        ...state,
        lang: a.lang,
        screen: "privacy",
        greeting: a.keepGreeting ? state.greeting : null,
      };

    case "greeting":
      // A fresh greeting (POST /greeting after a language change, or after
      // the UI found a session without one). Only before the conversation.
      if (state.session?.id !== a.sessionId) return state;
      if (state.screen !== "language" && state.screen !== "privacy") return state;
      return { ...state, greeting: { reply: a.reply, audioUrl: a.audioUrl } };

    case "privacyAck":
      if (state.screen !== "privacy") return state;
      return { ...state, screen: "conversation" };

    case "showReply":
      if (!inSession(state.screen)) return state;
      return {
        ...state,
        reply: a.reply,
        transcript: a.transcript ?? null,
        lastAudioUrl: a.audioUrl ?? state.lastAudioUrl,
        greeting: null,
        overlay: "none",
        replySeq: state.replySeq + 1,
      };

    case "turn": {
      if (!inSession(state.screen)) return state;
      const withSession = applySnapshot(state, a.result.session);
      if (withSession.screen === "ended") return { ...withSession, farewell: a.result.reply };
      return {
        ...withSession,
        reply: a.result.reply,
        transcript: a.result.transcript ?? null,
        lastAudioUrl: a.audioUrl,
        overlay: "none",
        notice: null,
        replySeq: state.replySeq + 1,
      };
    }

    case "voice":
      return { ...state, voice: a.voice, level: a.voice === "recording" ? state.level : 0 };

    case "level":
      return state.voice === "recording" ? { ...state, level: a.level } : state;

    case "busy":
      return { ...state, busy: a.busy };

    case "overlayClear":
      return { ...state, overlay: "none" };

    case "notice":
      return { ...state, notice: a.notice };

    case "micBlocked":
      return { ...state, micBlocked: true, voice: "idle", level: 0, notice: "micBlocked" };

    case "farewell":
      if (state.screen === "welcome" || state.screen === "boot") return state;
      return { ...state, ...sessionCleared, screen: "ended", farewell: a.reply };

    case "ended":
      if (state.screen === "welcome" || state.screen === "boot") return state;
      return { ...state, ...sessionCleared, screen: "ended" };

    case "reset":
      // Back to the attract screen; the controller re-reads /status, which
      // moves to the language screen if a new session is already open.
      return {
        ...state,
        ...sessionCleared,
        session: null,
        lang: "hi",
        screen: "welcome",
        farewell: null,
      };
  }
}

/** Derived: can the citizen use the microphone right now? */
export function voiceInputAvailable(s: UiState): boolean {
  return !!s.caps?.voice_input && !s.micBlocked;
}

/** Derived: should reply audio be played? */
export function voiceOutputAvailable(s: UiState): boolean {
  return !!s.caps?.voice_output;
}
