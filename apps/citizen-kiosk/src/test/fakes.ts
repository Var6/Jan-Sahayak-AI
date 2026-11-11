// Test doubles for the device agent, EventSource, microphone and speaker.
// They model the documented API contract, not a real dialogue engine.

import type { EventSourceLike } from "../api/events";
import type {
  Capabilities,
  DeviceStatus,
  Reply,
  SessionEvent,
  SessionSnapshot,
} from "../api/types";
import type { Player, PlayOutcome } from "../audio/player";
import { MicError, type Recorder, type RecordOptions, type RecordResult } from "../audio/recorder";
import { wavBlob } from "../audio/wav";

export class FakeEventSource implements EventSourceLike {
  static instances: FakeEventSource[] = [];
  readyState = 0;
  onopen: ((ev: Event) => unknown) | null = null;
  onerror: ((ev: Event) => unknown) | null = null;
  closed = false;
  private listeners = new Map<string, ((ev: MessageEvent) => void)[]>();

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }

  static latest(): FakeEventSource {
    const es = FakeEventSource.instances.at(-1);
    if (!es) throw new Error("no EventSource created");
    return es;
  }

  addEventListener(type: string, l: (ev: MessageEvent) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), l]);
  }

  close(): void {
    this.closed = true;
    this.readyState = 2;
  }

  open(): void {
    this.readyState = 1;
    this.onopen?.(new Event("open"));
  }

  fail(): void {
    this.onerror?.(new Event("error"));
  }

  emit(type: string, data: unknown): void {
    const ev = new MessageEvent(type, {
      data: typeof data === "string" ? data : JSON.stringify(data),
    });
    for (const l of this.listeners.get(type) ?? []) l(ev);
  }

  session(ev: SessionEvent): void {
    this.emit("session", ev);
  }
}

export const CAPS_ALL: Capabilities = {
  voice_input: true,
  voice_output: true,
  llm: true,
  sim_button: true,
  camera: false,
};

export const IDLE: SessionSnapshot = { activity: "IDLE", camera_consent: false, turns: 0 };

export function reply(over: Partial<Reply> = {}): Reply {
  return {
    kind: "answer",
    text: "reply",
    listen_next: false,
    end_session: false,
    used_llm: false,
    ...over,
  };
}

export interface Call {
  method: string;
  path: string;
  headers: Record<string, string>;
  body: unknown;
}

type Handler = (
  call: Call,
) => { status?: number; body: unknown } | Promise<{ status?: number; body: unknown }>;

/**
 * Programmable fake of the agent's HTTP API. Each route can be overridden
 * per test; every call is recorded.
 */
export class FakeAgent {
  calls: Call[] = [];
  caps: Capabilities = { ...CAPS_ALL };
  session: SessionSnapshot = { ...IDLE };
  routes = new Map<string, Handler>();

  status(): DeviceStatus {
    return {
      device_id: "test",
      version: "test",
      overall: "HEALTHY",
      capabilities: this.caps,
      session: this.session,
    };
  }

  /** Route key: "POST /session/:id/listen" etc. (`:id` replaces the session id). */
  on(key: string, h: Handler): this {
    this.routes.set(key, h);
    return this;
  }

  fetch = async (input: string, init: RequestInit = {}): Promise<Response> => {
    const method = init.method ?? "GET";
    const path = input.replace(/^\/api\/v1/, "");
    const headers = Object.fromEntries(
      Object.entries((init.headers ?? {}) as Record<string, string>).map(([k, v]) => [
        k.toLowerCase(),
        v,
      ]),
    );
    let body: unknown = init.body;
    if (typeof init.body === "string") body = JSON.parse(init.body);
    const call: Call = { method, path, headers, body };
    this.calls.push(call);
    const key = `${method} ${path.replace(/^\/session\/[^/]+\//, "/session/:id/")}`;
    const h = this.routes.get(key);
    if (h) {
      const r = await h(call);
      return new Response(JSON.stringify(r.body), { status: r.status ?? 200 });
    }
    if (key === "GET /status") return new Response(JSON.stringify(this.status()));
    if (key === "POST /session/:id/touch" || key === "POST /session/:id/language") {
      return new Response(JSON.stringify({ session: this.session }));
    }
    return new Response(
      JSON.stringify({ error: { code: "internal", message: `no route ${key}` } }),
      {
        status: 500,
      },
    );
  };

  paths(): string[] {
    return this.calls.map((c) => `${c.method} ${c.path}`);
  }
}

export class FakeRecorder implements Recorder {
  records = 0;
  aborted = 0;
  pending: { resolve: (r: RecordResult) => void; reject: (e: unknown) => void } | null = null;
  failWith: MicError | null = null;

  record(opts?: RecordOptions): Promise<RecordResult> {
    this.records++;
    if (this.failWith) return Promise.reject(this.failWith);
    opts?.onLevel?.(0.5);
    return new Promise((resolve, reject) => {
      this.pending = { resolve, reject };
    });
  }

  /** Simulate the VAD ending the utterance. */
  finish(): void {
    const p = this.pending;
    this.pending = null;
    p?.resolve({
      wav: wavBlob(new Int16Array(1600)),
      durationMs: 100,
      end: "silence",
      hadSpeech: true,
    });
  }

  get active(): boolean {
    return this.pending !== null;
  }

  stop(): void {
    this.finish();
  }

  abort(): void {
    const p = this.pending;
    this.pending = null;
    if (p) {
      this.aborted++;
      p.reject(new MicError("aborted"));
    }
  }
}

export class FakePlayer implements Player {
  played: string[] = [];
  private settle: ((o: PlayOutcome) => void) | null = null;
  /** If true, play() resolves immediately with "ended". */
  auto = true;

  get playing(): boolean {
    return this.settle !== null;
  }

  play(url: string): Promise<PlayOutcome> {
    this.played.push(url);
    if (this.auto) return Promise.resolve("ended");
    return new Promise((r) => {
      this.settle = r;
    });
  }

  end(): void {
    const s = this.settle;
    this.settle = null;
    s?.("ended");
  }

  stop(): void {
    const s = this.settle;
    this.settle = null;
    s?.("stopped");
  }
}
