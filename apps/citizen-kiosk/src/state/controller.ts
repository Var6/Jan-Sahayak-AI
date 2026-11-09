// Side effects of the kiosk UI: talks to the device agent, drives the mic
// and the speaker, and feeds the reducer. One instance per App.
//
// Invariants:
//   * the mic is opened only after a POST (listen or speaking-done) has
//     returned the session in LISTENING;
//   * the mic is released after every utterance and whenever the session
//     ends, the screen changes, or the citizen switches to text;
//   * after reply audio finishes (or when there is none while the agent is
//     SPEAKING) the UI posts speaking-done exactly once;
//   * results of requests that belong to an ended session are dropped.

import { ApiError, type DeviceApi, hasSession, safeAudioUrl } from "../api/client";
import { type EventSourceFactory, EventStream } from "../api/events";
import type { Lang, Reply, SessionSnapshot, TurnResult } from "../api/types";
import type { Player } from "../audio/player";
import { MicError, type Recorder } from "../audio/recorder";
import {
  type Action,
  type NoticeKey,
  type UiState,
  voiceInputAvailable,
  voiceOutputAvailable,
} from "./machine";

export interface ControllerDeps {
  api: DeviceApi;
  recorder: Recorder;
  player: Player;
  createEventSource?: EventSourceFactory;
  /** Override timings (tests). */
  timings?: Partial<typeof TIMINGS>;
}

export const TIMINGS = {
  /** Ended screen → welcome. */
  endedReturnMs: 8_000,
  /** Upper bound on the ended screen while the goodbye plays. */
  endedMaxMs: 25_000,
  /** At most one /touch per this interval for general taps. */
  touchThrottleMs: 20_000,
  /** Transient notices disappear after this. */
  noticeMs: 6_000,
};

export class KioskController {
  private readonly t: typeof TIMINGS;
  private events: EventStream | null = null;
  private epoch = 0; // bumps when a session ends; stale async results are dropped
  private lastTouch = 0;
  private endedTimer: ReturnType<typeof setTimeout> | null = null;
  private noticeTimer: ReturnType<typeof setTimeout> | null = null;
  private resyncing = false;
  private disposed = false;
  /** Mic pressed while a reply was playing: listen once playback stops. */
  private bargeIn = false;

  constructor(
    private readonly deps: ControllerDeps,
    private readonly getState: () => UiState,
    private readonly dispatch: (a: Action) => void,
  ) {
    this.t = { ...TIMINGS, ...deps.timings };
  }

  // ---- lifecycle -------------------------------------------------------

  start(): void {
    this.disposed = false;
    this.events = new EventStream(
      {
        onOpen: () => void this.resync(),
        onConn: (conn) => this.dispatch({ type: "conn", conn }),
        onHealth: (h) => this.dispatch({ type: "health", overall: h.overall, caps: h.capabilities }),
        onSession: (ev) => {
          const before = this.getState();
          this.dispatch({ type: "sessionEvent", ev });
          if (ev.type === "session.ended") {
            // Already on the ended screen (our own End / goodbye): let the
            // goodbye audio finish.
            if (before.screen !== "ended") this.onSessionGone(true);
          } else {
            this.afterScreenChange(before);
          }
        },
      },
      this.deps.createEventSource ? { create: this.deps.createEventSource } : {},
    );
    this.events.start();
    void this.resync();
  }

  dispose(): void {
    this.disposed = true;
    this.events?.stop();
    this.events = null;
    this.deps.recorder.abort();
    this.deps.player.stop();
    if (this.endedTimer) clearTimeout(this.endedTimer);
    if (this.noticeTimer) clearTimeout(this.noticeTimer);
  }

  /** Re-read the full device state (boot, SSE reconnect, after errors). */
  async resync(): Promise<void> {
    if (this.resyncing || this.disposed) return;
    this.resyncing = true;
    const before = this.getState();
    try {
      const status = await this.deps.api.status();
      if (this.disposed) return;
      this.dispatch({ type: "status", status });
      this.afterScreenChange(before);
    } catch {
      // Offline: the SSE loop retries and calls resync again on reconnect.
    } finally {
      this.resyncing = false;
    }
  }

  // ---- welcome / setup -------------------------------------------------

  /** Simulated physical button (dev PC only; the Pi uses GPIO). */
  async pressStart(): Promise<void> {
    const s = this.getState();
    if (s.busy || s.screen !== "welcome" || !s.caps?.sim_button) return;
    this.dispatch({ type: "busy", busy: true });
    try {
      const r = await this.deps.api.startSession(s.lang);
      this.dispatch({
        type: "started",
        session: r.session,
        reply: r.reply ?? null,
        audioUrl: safeAudioUrl(r.audio_url),
      });
    } catch (e) {
      // A session may have started via GPIO meanwhile; the status tells.
      this.showError(e);
      void this.resync();
    } finally {
      this.dispatch({ type: "busy", busy: false });
    }
  }

  async chooseLanguage(lang: Lang): Promise<void> {
    const s = this.getState();
    const id = s.session?.id;
    if (s.screen !== "language" || !id) return;
    const current = s.session?.language ?? "hi";
    const keepGreeting = current === lang;
    this.dispatch({ type: "languageChosen", lang, keepGreeting });
    if (current !== lang) {
      try {
        const r = await this.deps.api.setLanguage(id, lang);
        this.dispatch({ type: "session", session: r.session });
      } catch (e) {
        this.handleSessionError(e);
      }
    }
  }

  async ackPrivacy(): Promise<void> {
    const s = this.getState();
    if (s.screen !== "privacy") return;
    const greeting = s.greeting;
    this.dispatch({ type: "privacyAck" });
    const epoch = this.epoch;
    if (greeting) {
      this.dispatch({ type: "showReply", reply: greeting.reply, audioUrl: greeting.audioUrl });
      await this.speakThenContinue(greeting.reply, greeting.audioUrl, epoch);
    } else if (s.session?.activity === "SPEAKING") {
      // Greeting not shown (language changed or session started by GPIO):
      // release the agent from SPEAKING.
      await this.finishSpeaking(null, epoch);
    }
  }

  // ---- conversation ----------------------------------------------------

  /** The big mic button. Meaning depends on what is happening. */
  async micPressed(): Promise<void> {
    const s = this.getState();
    if (s.screen !== "conversation") return;
    this.touchSoon();
    switch (s.voice) {
      case "recording":
        this.deps.recorder.stop();
        return;
      case "playing":
        // Barge-in: stop the reply; the playing flow then opens the mic.
        this.bargeIn = true;
        this.deps.player.stop();
        return;
      case "idle":
        if (s.busy || !voiceInputAvailable(s)) return;
        await this.listenAndRecord();
        return;
      default:
        return;
    }
  }

  async sendText(text: string): Promise<boolean> {
    const s = this.getState();
    const id = s.session?.id;
    if (s.screen !== "conversation" || !id || s.busy) return false;
    const trimmed = text.trim();
    if (!trimmed) return false;
    if ([...trimmed].length > 500) {
      this.notice("tooLong");
      return false;
    }
    this.cancelVoice();
    return this.runTurn(() => this.deps.api.sendText(id, trimmed));
  }

  async chooseOption(optionId: string): Promise<void> {
    const s = this.getState();
    const id = s.session?.id;
    if (s.screen !== "conversation" || !id || s.busy) return;
    this.cancelVoice();
    await this.runTurn(() => this.deps.api.chooseOption(id, optionId));
  }

  /** Replay the last reply audio (no state change on the agent). */
  async repeat(): Promise<void> {
    const s = this.getState();
    const url = s.lastAudioUrl;
    if (!url || s.voice !== "idle" || s.busy || !voiceOutputAvailable(s)) return;
    this.touchSoon(true);
    const epoch = this.epoch;
    this.dispatch({ type: "voice", voice: "playing" });
    await this.deps.player.play(url);
    if (epoch !== this.epoch) return;
    this.dispatch({ type: "voice", voice: "idle" });
    if (this.bargeIn) {
      this.bargeIn = false;
      if (voiceInputAvailable(this.getState())) await this.listenAndRecord();
    }
  }

  async endSession(): Promise<void> {
    const s = this.getState();
    const id = s.session?.id;
    if (!id) {
      this.dispatch({ type: "ended" });
      this.scheduleReturn(this.t.endedReturnMs);
      return;
    }
    this.cancelVoice();
    this.deps.player.stop();
    this.epoch++;
    try {
      const r = await this.deps.api.end(id);
      this.dispatch({ type: "farewell", reply: r.reply });
      const url = safeAudioUrl(r.audio_url);
      this.scheduleReturn(this.t.endedMaxMs);
      if (url && voiceOutputAvailable(this.getState())) {
        const epoch = this.epoch;
        await this.deps.player.play(url);
        if (epoch !== this.epoch) return;
      }
      this.scheduleReturn(this.t.endedReturnMs);
    } catch {
      // Whatever happened, the citizen asked to leave: show the end screen.
      this.dispatch({ type: "ended" });
      this.scheduleReturn(this.t.endedReturnMs);
    }
  }

  /** Inactivity overlay "continue" button. */
  async keepAlive(): Promise<void> {
    const id = this.getState().session?.id;
    this.dispatch({ type: "overlayClear" });
    if (!id) return;
    try {
      this.lastTouch = Date.now();
      const r = await this.deps.api.touch(id);
      this.dispatch({ type: "session", session: r.session });
    } catch (e) {
      this.handleSessionError(e);
    }
  }

  /** Any tap on the conversation screen (throttled). */
  touchSoon(force = false): void {
    const s = this.getState();
    const id = s.session?.id;
    if (!id) return;
    const now = Date.now();
    if (!force && now - this.lastTouch < this.t.touchThrottleMs) return;
    this.lastTouch = now;
    this.deps.api.touch(id).catch((e: unknown) => this.handleSessionError(e));
  }

  dismissNotice(): void {
    this.dispatch({ type: "notice", notice: null });
  }

  // ---- internals -------------------------------------------------------

  private async listenAndRecord(): Promise<void> {
    const s = this.getState();
    const id = s.session?.id;
    if (!id) return;
    const epoch = this.epoch;
    this.dispatch({ type: "voice", voice: "opening" });
    let session: SessionSnapshot;
    try {
      session = (await this.deps.api.listen(id)).session;
    } catch (e) {
      if (epoch === this.epoch) this.dispatch({ type: "voice", voice: "idle" });
      this.handleSessionError(e);
      return;
    }
    if (epoch !== this.epoch) return;
    this.dispatch({ type: "session", session });
    if (session.activity !== "LISTENING") {
      this.dispatch({ type: "voice", voice: "idle" });
      return;
    }
    await this.recordAndSend(id, epoch);
  }

  /** Precondition: the agent has just confirmed LISTENING for session `id`. */
  private async recordAndSend(id: string, epoch: number): Promise<void> {
    this.dispatch({ type: "voice", voice: "recording" });
    let wav: Blob;
    try {
      const res = await this.deps.recorder.record({
        onLevel: (level) => this.dispatch({ type: "level", level }),
      });
      wav = res.wav;
    } catch (e) {
      if (epoch !== this.epoch) return;
      this.dispatch({ type: "voice", voice: "idle" });
      if (e instanceof MicError && e.code === "aborted") return;
      // Permission denied, no device, unsupported: switch to text input.
      this.dispatch({ type: "micBlocked" });
      return;
    }
    if (epoch !== this.epoch || this.getState().session?.id !== id) return;
    this.dispatch({ type: "voice", voice: "uploading" });
    await this.runTurn(() => this.deps.api.sendAudio(id, wav), true);
  }

  private async runTurn(call: () => Promise<TurnResult>, fromVoice = false): Promise<boolean> {
    const epoch = this.epoch;
    this.dispatch({ type: "busy", busy: true });
    let result: TurnResult;
    try {
      result = await call();
    } catch (e) {
      if (epoch === this.epoch) {
        this.dispatch({ type: "busy", busy: false });
        if (fromVoice) this.dispatch({ type: "voice", voice: "idle" });
        this.handleSessionError(e);
      }
      return false;
    }
    if (epoch !== this.epoch) return false;
    const audioUrl = safeAudioUrl(result.audio_url);
    this.dispatch({ type: "turn", result, audioUrl });
    this.dispatch({ type: "busy", busy: false });
    this.dispatch({ type: "voice", voice: "idle" });
    if (this.getState().screen === "ended") {
      await this.playFarewell(audioUrl);
      return true;
    }
    await this.speakThenContinue(result.reply, audioUrl, epoch, result.session);
    return true;
  }

  /**
   * Play reply audio (if any and voice output works), then tell the agent we
   * are done speaking, then follow listen_next / end_session.
   */
  private async speakThenContinue(
    reply: Reply,
    audioUrl: string | null,
    epoch: number,
    session?: SessionSnapshot,
  ): Promise<void> {
    const s = this.getState();
    const activity = session?.activity ?? s.session?.activity;
    if (audioUrl && voiceOutputAvailable(s)) {
      this.dispatch({ type: "voice", voice: "playing" });
      await this.deps.player.play(audioUrl);
      if (epoch !== this.epoch) return;
      this.dispatch({ type: "voice", voice: "idle" });
      await this.finishSpeaking(reply, epoch);
    } else if (activity === "SPEAKING") {
      await this.finishSpeaking(reply, epoch);
    } else {
      await this.maybeAutoListen(reply, activity, epoch);
    }
  }

  private async finishSpeaking(reply: Reply | null, epoch: number): Promise<void> {
    const id = this.getState().session?.id;
    if (!id) return;
    let session: SessionSnapshot;
    try {
      session = (await this.deps.api.speakingDone(id)).session;
    } catch (e) {
      if (epoch === this.epoch) this.handleSessionError(e, true);
      return;
    }
    if (epoch !== this.epoch) return;
    const wantListen = this.bargeIn;
    this.bargeIn = false;
    if (!hasSession(session)) {
      if (reply?.end_session) this.dispatch({ type: "farewell", reply });
      else this.dispatch({ type: "session", session });
      this.onSessionGone(true);
      return;
    }
    this.dispatch({ type: "session", session });
    if (wantListen && !reply?.end_session && voiceInputAvailable(this.getState())) {
      if (session.activity === "LISTENING") await this.recordAndSend(id, epoch);
      else await this.listenAndRecord();
      return;
    }
    if (reply) await this.maybeAutoListen(reply, session.activity, epoch);
  }

  private async maybeAutoListen(
    reply: Reply,
    activity: string | undefined,
    epoch: number,
  ): Promise<void> {
    const s = this.getState();
    const id = s.session?.id;
    if (!id || s.screen !== "conversation" || reply.end_session) return;
    // Only when the agent itself put the session in LISTENING (listen_next).
    if (reply.listen_next && activity === "LISTENING" && voiceInputAvailable(s) && s.voice === "idle") {
      await this.recordAndSend(id, epoch);
    }
  }

  private async playFarewell(audioUrl: string | null): Promise<void> {
    this.scheduleReturn(this.t.endedMaxMs);
    const epoch = this.epoch;
    if (audioUrl && voiceOutputAvailable(this.getState())) {
      await this.deps.player.play(audioUrl);
      if (epoch !== this.epoch) return;
    }
    this.scheduleReturn(this.t.endedReturnMs);
  }

  /** Stop any recording (discarding it) and any playback. */
  private cancelVoice(): void {
    const v = this.getState().voice;
    if (v === "recording" || v === "opening") this.deps.recorder.abort();
    if (v === "playing") this.deps.player.stop();
  }

  private afterScreenChange(before: UiState): void {
    const after = this.getState();
    if (before.screen !== after.screen && after.screen === "ended") this.onSessionGone(false);
  }

  private onSessionGone(wasActive: boolean): void {
    this.epoch++;
    this.bargeIn = false;
    this.deps.recorder.abort();
    this.deps.player.stop();
    if (wasActive || this.getState().screen === "ended") {
      if (this.getState().screen !== "ended") this.dispatch({ type: "ended" });
      this.scheduleReturn(this.t.endedReturnMs);
    }
  }

  private scheduleReturn(ms: number): void {
    if (this.endedTimer) clearTimeout(this.endedTimer);
    this.endedTimer = setTimeout(() => {
      this.endedTimer = null;
      if (this.disposed) return;
      if (this.getState().screen === "ended") {
        this.dispatch({ type: "reset" });
        void this.resync();
      }
    }, ms);
  }

  private handleSessionError(e: unknown, quiet = false): void {
    if (e instanceof ApiError && e.sessionGone) {
      this.onSessionGone(true);
      return;
    }
    if (e instanceof ApiError && e.code === "bad_state") {
      // Our view of the session is stale; re-read it.
      void this.resync();
      return;
    }
    if (!quiet) this.showError(e);
  }

  private showError(e: unknown): void {
    let key: NoticeKey = "error";
    if (e instanceof ApiError) {
      if (e.code === "timeout") key = "timeout";
      else if (e.code === "button_disabled") key = "buttonDisabled";
      else if (e.code === "unavailable") key = "unavailable";
      else if (e.code === "too_large") key = "tooLong";
    }
    this.notice(key);
  }

  private notice(key: NoticeKey): void {
    this.dispatch({ type: "notice", notice: key });
    if (this.noticeTimer) clearTimeout(this.noticeTimer);
    this.noticeTimer = setTimeout(() => {
      this.noticeTimer = null;
      if (this.getState().notice === key) this.dispatch({ type: "notice", notice: null });
    }, this.t.noticeMs);
  }
}
