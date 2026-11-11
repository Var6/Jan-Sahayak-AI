import { describe, expect, it } from "vitest";
import type { DeviceStatus, SessionSnapshot } from "../api/types";
import { CAPS_ALL, IDLE, reply } from "../test/fakes";
import {
  type Action,
  initialState,
  reducer,
  type UiState,
  voiceInputAvailable,
  voiceOutputAvailable,
} from "./machine";

const S = (over: Partial<SessionSnapshot> = {}): SessionSnapshot => ({
  id: "s1",
  activity: "READY",
  language: "hi",
  camera_consent: false,
  turns: 0,
  ...over,
});

const status = (session: SessionSnapshot = IDLE, caps = CAPS_ALL): DeviceStatus => ({
  device_id: "d",
  version: "v",
  overall: "HEALTHY",
  capabilities: caps,
  session,
});

const run = (actions: Action[], from: UiState = initialState) => actions.reduce(reducer, from);

const welcome = run([{ type: "status", status: status() }]);
const conversation = run(
  [
    {
      type: "started",
      session: S(),
      reply: reply({ kind: "greeting", text: "नमस्ते" }),
      audioUrl: "/api/v1/a/1.wav",
    },
    { type: "languageChosen", lang: "hi", keepGreeting: true },
    { type: "privacyAck" },
  ],
  welcome,
);

describe("reducer: boot and start", () => {
  it("boot → welcome when no session is open", () => {
    expect(welcome.screen).toBe("welcome");
    expect(welcome.caps).toEqual(CAPS_ALL);
  });

  it("simulated start → language → privacy → conversation, keeping the greeting", () => {
    const s1 = run(
      [{ type: "started", session: S(), reply: reply({ kind: "greeting" }), audioUrl: "/x" }],
      welcome,
    );
    expect(s1.screen).toBe("language");
    expect(s1.greeting?.audioUrl).toBe("/x");
    const s2 = reducer(s1, { type: "languageChosen", lang: "hi", keepGreeting: true });
    expect(s2.screen).toBe("privacy");
    expect(s2.greeting).not.toBeNull();
    const s3 = reducer(s2, { type: "privacyAck" });
    expect(s3.screen).toBe("conversation");
  });

  it("choosing another language drops the greeting (it was spoken in the old language)", () => {
    const s = run(
      [
        { type: "started", session: S(), reply: reply(), audioUrl: null },
        { type: "languageChosen", lang: "en", keepGreeting: false },
      ],
      welcome,
    );
    expect(s.lang).toBe("en");
    expect(s.greeting).toBeNull();
  });

  it("GPIO start arrives as an SSE event and opens the language screen", () => {
    const s = reducer(welcome, {
      type: "sessionEvent",
      ev: { type: "session.started", session: S() },
    });
    expect(s.screen).toBe("language");
    expect(s.session?.id).toBe("s1");
  });

  it("reload mid-conversation resumes on the conversation screen", () => {
    const s = reducer(initialState, {
      type: "status",
      status: status(S({ turns: 3, language: "en" })),
    });
    expect(s.screen).toBe("conversation");
    expect(s.lang).toBe("en");
  });
});

describe("reducer: conversation", () => {
  it("a turn shows the reply, transcript and audio url and clears overlays", () => {
    const withOverlay = { ...conversation, overlay: "reminder" as const };
    const s = reducer(withOverlay, {
      type: "turn",
      result: {
        session: S({ activity: "SPEAKING", turns: 1 }),
        transcript: { text: "मुझे लोन चाहिए", confidence: 0.9, no_speech: false },
        reply: reply({ text: "उत्तर", options: [{ id: "a", label: "A" }] }),
        audio_url: "/api/v1/session/s1/audio/1.wav",
      },
      audioUrl: "/api/v1/session/s1/audio/1.wav",
    });
    expect(s.reply?.text).toBe("उत्तर");
    expect(s.transcript?.text).toBe("मुझे लोन चाहिए");
    expect(s.lastAudioUrl).toBe("/api/v1/session/s1/audio/1.wav");
    expect(s.overlay).toBe("none");
    expect(s.replySeq).toBe(conversation.replySeq + 1);
    expect(s.session?.activity).toBe("SPEAKING");
  });

  it("a turn whose session is already closed becomes the farewell on the ended screen", () => {
    const s = reducer(conversation, {
      type: "turn",
      result: {
        session: IDLE,
        reply: reply({ kind: "goodbye", text: "अलविदा", end_session: true }),
      },
      audioUrl: null,
    });
    expect(s.screen).toBe("ended");
    expect(s.farewell?.text).toBe("अलविदा");
  });

  it("level is only tracked while recording", () => {
    const rec = reducer(conversation, { type: "voice", voice: "recording" });
    expect(reducer(rec, { type: "level", level: 0.7 }).level).toBe(0.7);
    expect(reducer(conversation, { type: "level", level: 0.7 }).level).toBe(0);
    expect(reducer({ ...rec, level: 0.7 }, { type: "voice", voice: "uploading" }).level).toBe(0);
  });

  it("mic failure switches to text-only for the rest of the session", () => {
    const s = reducer(conversation, { type: "micBlocked" });
    expect(voiceInputAvailable(s)).toBe(false);
    expect(s.notice).toBe("micBlocked");
    const ended = reducer(s, {
      type: "sessionEvent",
      ev: { type: "session.ended", session: IDLE },
    });
    expect(ended.micBlocked).toBe(false);
  });

  it("derives degraded capabilities", () => {
    const s = reducer(conversation, {
      type: "health",
      overall: "DEGRADED",
      caps: { ...CAPS_ALL, voice_input: false, voice_output: false },
    });
    expect(voiceInputAvailable(s)).toBe(false);
    expect(voiceOutputAvailable(s)).toBe(false);
    expect(s.overall).toBe("DEGRADED");
  });
});

describe("reducer: inactivity and end", () => {
  it("reminder then final warning overlays; reminder never downgrades the final warning", () => {
    const r = reducer(conversation, {
      type: "sessionEvent",
      ev: { type: "session.reminder", session: S() },
    });
    expect(r.overlay).toBe("reminder");
    const f = reducer(r, {
      type: "sessionEvent",
      ev: { type: "session.final_warning", session: S() },
    });
    expect(f.overlay).toBe("final_warning");
    const again = reducer(f, {
      type: "sessionEvent",
      ev: { type: "session.reminder", session: S() },
    });
    expect(again.overlay).toBe("final_warning");
    expect(reducer(f, { type: "overlayClear" }).overlay).toBe("none");
  });

  it("no overlays outside a session", () => {
    const s = reducer(welcome, {
      type: "sessionEvent",
      ev: { type: "session.reminder", session: IDLE },
    });
    expect(s.overlay).toBe("none");
  });

  it("session.ended clears all session content and shows the ended screen", () => {
    const s = reducer(
      {
        ...conversation,
        transcript: { text: "x", confidence: 1, no_speech: false },
        lastAudioUrl: "/a",
      },
      {
        type: "sessionEvent",
        ev: { type: "session.ended", session: IDLE, end_reason: "inactivity_timeout" },
      },
    );
    expect(s.screen).toBe("ended");
    expect(s.reply).toBeNull();
    expect(s.transcript).toBeNull();
    expect(s.lastAudioUrl).toBeNull();
    expect(s.session?.id).toBeUndefined();
  });

  it("events for another session are ignored", () => {
    const s = reducer(conversation, {
      type: "sessionEvent",
      ev: { type: "session.reminder", session: S({ id: "other" }) },
    });
    expect(s).toBe(conversation);
  });

  it("farewell keeps the goodbye text through a later session.ended", () => {
    const f = reducer(conversation, {
      type: "farewell",
      reply: reply({ kind: "goodbye", text: "धन्यवाद" }),
    });
    const s = reducer(f, { type: "sessionEvent", ev: { type: "session.ended", session: IDLE } });
    expect(s.screen).toBe("ended");
    expect(s.farewell?.text).toBe("धन्यवाद");
  });

  it("reset returns to welcome in Hindi; a new session from there opens language", () => {
    const ended = reducer(conversation, { type: "ended" });
    const r = reducer({ ...ended, lang: "en" }, { type: "reset" });
    expect(r.screen).toBe("welcome");
    expect(r.lang).toBe("hi");
    expect(r.farewell).toBeNull();
    const next = reducer(ended, {
      type: "sessionEvent",
      ev: { type: "session.started", session: S({ id: "s2" }) },
    });
    expect(next.screen).toBe("language");
  });

  it("status without a session while in conversation ends it", () => {
    const s = reducer(conversation, { type: "status", status: status(IDLE) });
    expect(s.screen).toBe("ended");
  });
});

describe("reducer: greetings", () => {
  it("a GPIO session.started event carries the greeting into the setup screens", () => {
    const s = run(
      [
        {
          type: "sessionEvent",
          ev: {
            type: "session.started",
            session: S({ activity: "SPEAKING" }),
            reply: reply({ kind: "greeting", text: "नमस्ते" }),
            audio_url: "/api/v1/session/s1/audio/0.wav",
          },
        },
      ],
      welcome,
    );
    expect(s.screen).toBe("language");
    expect(s.greeting?.reply.text).toBe("नमस्ते");
    expect(s.greeting?.audioUrl).toBe("/api/v1/session/s1/audio/0.wav");
  });

  it("an unsafe audio_url on session.started is dropped", () => {
    const s = run(
      [
        {
          type: "sessionEvent",
          ev: {
            type: "session.started",
            session: S(),
            reply: reply({ kind: "greeting" }),
            audio_url: "https://evil.example/x.wav",
          },
        },
      ],
      welcome,
    );
    expect(s.greeting?.audioUrl).toBeNull();
  });

  it("a fresh greeting applies only to the same session before the conversation", () => {
    const lang = run([{ type: "started", session: S(), reply: null, audioUrl: null }], welcome);
    const g = reply({ kind: "greeting", text: "Hello" });
    expect(
      reducer(lang, { type: "greeting", sessionId: "s1", reply: g, audioUrl: null }).greeting?.reply
        .text,
    ).toBe("Hello");
    expect(reducer(lang, { type: "greeting", sessionId: "other", reply: g, audioUrl: null })).toBe(
      lang,
    );
    expect(
      reducer(conversation, { type: "greeting", sessionId: "s1", reply: g, audioUrl: null }),
    ).toBe(conversation);
  });
});
