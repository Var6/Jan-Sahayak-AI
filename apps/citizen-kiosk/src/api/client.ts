// Typed client for the device local API v1. Every call has an explicit
// timeout; mutating calls carry `X-JS-Kiosk: 1` (the agent's CSRF check).
// Requests use relative same-origin paths: the agent serves this UI at `/`.

import type {
  DeviceStatus,
  ErrorBody,
  Lang,
  ReplyResult,
  SessionResult,
  SessionSnapshot,
  TurnResult,
} from "./types";

export const API_BASE = "/api/v1";

/** Agent limits (docs/API_DOCUMENTATION.md). Checked before sending. */
export const MAX_JSON_BYTES = 16 * 1024;
export const MAX_AUDIO_BYTES = 1024 * 1024;
export const MAX_TEXT_CHARS = 500;

/** Per-call timeouts in milliseconds. */
export const TIMEOUTS = {
  status: 5_000,
  control: 10_000, // listen, speaking-done, language, touch
  reply: 30_000, // start, end (greeting/goodbye include TTS)
  turn: 60_000, // audio, text, option (STT + dialogue + TTS on a Pi)
} as const;

/**
 * Codes produced by the client itself, in addition to the agent's
 * documented error codes.
 */
export type ClientErrorCode = "network" | "timeout" | "aborted" | "bad_response";

export class ApiError extends Error {
  readonly code: string;
  readonly status: number;

  constructor(code: string, status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
  }

  /** True if the active session is gone and the UI should reset. */
  get sessionGone(): boolean {
    return this.code === "no_session" || this.code === "wrong_session";
  }
}

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface ApiClientOptions {
  fetch?: FetchLike;
  base?: string;
}

interface RequestOpts {
  method: "GET" | "POST";
  path: string;
  timeoutMs: number;
  json?: unknown;
  body?: Blob;
  contentType?: string;
  signal?: AbortSignal;
}

const sid = (id: string): string => {
  if (!id) throw new ApiError("no_session", 0, "no active session id");
  return encodeURIComponent(id);
};

export class DeviceApi {
  private readonly fetchImpl: FetchLike;
  private readonly base: string;

  constructor(opts: ApiClientOptions = {}) {
    // Bind so that window.fetch is not called with a wrong `this`.
    this.fetchImpl = opts.fetch ?? ((input, init) => globalThis.fetch(input, init));
    this.base = opts.base ?? API_BASE;
  }

  status(signal?: AbortSignal): Promise<DeviceStatus> {
    return this.request({ method: "GET", path: "/status", timeoutMs: TIMEOUTS.status, signal });
  }

  startSession(language?: Lang): Promise<ReplyResult> {
    return this.request({
      method: "POST",
      path: "/session/start",
      timeoutMs: TIMEOUTS.reply,
      json: language ? { language } : {},
    });
  }

  listen(id: string): Promise<SessionResult> {
    return this.post(`/session/${sid(id)}/listen`, TIMEOUTS.control);
  }

  sendAudio(id: string, wav: Blob): Promise<TurnResult> {
    if (wav.size > MAX_AUDIO_BYTES) {
      return Promise.reject(new ApiError("too_large", 0, "audio exceeds 1 MB"));
    }
    return this.request({
      method: "POST",
      path: `/session/${sid(id)}/audio`,
      timeoutMs: TIMEOUTS.turn,
      body: wav,
      contentType: "audio/wav",
    });
  }

  sendText(id: string, text: string): Promise<TurnResult> {
    const t = text.trim();
    if (t.length === 0) return Promise.reject(new ApiError("bad_request", 0, "empty text"));
    if ([...t].length > MAX_TEXT_CHARS) {
      return Promise.reject(new ApiError("too_large", 0, "text exceeds 500 characters"));
    }
    return this.post(`/session/${sid(id)}/text`, TIMEOUTS.turn, { text: t });
  }

  chooseOption(id: string, optionId: string): Promise<TurnResult> {
    return this.post(`/session/${sid(id)}/option`, TIMEOUTS.turn, { option_id: optionId });
  }

  speakingDone(id: string): Promise<SessionResult> {
    return this.post(`/session/${sid(id)}/speaking-done`, TIMEOUTS.control);
  }

  setLanguage(id: string, language: Lang): Promise<SessionResult> {
    return this.post(`/session/${sid(id)}/language`, TIMEOUTS.control, { language });
  }

  touch(id: string): Promise<SessionResult> {
    return this.post(`/session/${sid(id)}/touch`, TIMEOUTS.control);
  }

  end(id: string): Promise<ReplyResult> {
    return this.post(`/session/${sid(id)}/end`, TIMEOUTS.reply);
  }

  private post<T>(path: string, timeoutMs: number, json?: unknown): Promise<T> {
    return this.request<T>({ method: "POST", path, timeoutMs, json });
  }

  private async request<T>(o: RequestOpts): Promise<T> {
    const headers: Record<string, string> = { Accept: "application/json" };
    let body: BodyInit | undefined;
    if (o.method === "POST") {
      headers["X-JS-Kiosk"] = "1";
      if (o.body) {
        body = o.body;
        headers["Content-Type"] = o.contentType ?? "application/octet-stream";
      } else {
        const s = JSON.stringify(o.json ?? {});
        if (new TextEncoder().encode(s).length > MAX_JSON_BYTES) {
          throw new ApiError("too_large", 0, "request body exceeds 16 KB");
        }
        body = s;
        headers["Content-Type"] = "application/json";
      }
    }

    const ctl = new AbortController();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      ctl.abort();
    }, o.timeoutMs);
    const onCallerAbort = () => ctl.abort();
    if (o.signal) {
      if (o.signal.aborted) ctl.abort();
      else o.signal.addEventListener("abort", onCallerAbort, { once: true });
    }

    try {
      let res: Response;
      try {
        res = await this.fetchImpl(this.base + o.path, {
          method: o.method,
          headers,
          body,
          signal: ctl.signal,
          credentials: "same-origin",
          cache: "no-store",
        });
      } catch (e) {
        if (timedOut) throw new ApiError("timeout", 0, `request timed out after ${o.timeoutMs} ms`);
        if (ctl.signal.aborted) throw new ApiError("aborted", 0, "request aborted");
        throw new ApiError("network", 0, e instanceof Error ? e.message : "network error");
      }

      let text: string;
      try {
        text = await res.text();
      } catch {
        if (timedOut) throw new ApiError("timeout", res.status, "response timed out");
        throw new ApiError("network", res.status, "failed to read response");
      }

      if (!res.ok) throw errorFromBody(res.status, text);

      try {
        return JSON.parse(text) as T;
      } catch {
        throw new ApiError("bad_response", res.status, "response is not valid JSON");
      }
    } finally {
      clearTimeout(timer);
      o.signal?.removeEventListener("abort", onCallerAbort);
    }
  }
}

function errorFromBody(status: number, text: string): ApiError {
  try {
    const b = JSON.parse(text) as Partial<ErrorBody>;
    if (b && typeof b.error?.code === "string") {
      return new ApiError(b.error.code, status, String(b.error.message ?? ""));
    }
  } catch {
    // fall through
  }
  const code = status === 413 ? "too_large" : status === 503 ? "unavailable" : "internal";
  return new ApiError(code, status, `HTTP ${status}`);
}

/** True when the snapshot describes an open session. */
export function hasSession(s: SessionSnapshot | null | undefined): s is SessionSnapshot & {
  id: string;
} {
  return !!s?.id && s.activity !== "IDLE";
}

/**
 * Accept only same-origin API audio paths. The UI never fetches anything
 * outside the device agent.
 */
export function safeAudioUrl(url: string | undefined | null): string | null {
  if (!url) return null;
  if (!url.startsWith(`${API_BASE}/`) || url.includes("..") || url.includes("//")) return null;
  return url;
}
