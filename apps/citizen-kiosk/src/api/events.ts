// SSE subscription to GET /api/v1/events with our own reconnect loop.
//
// The native EventSource retry is replaced by an explicit, bounded
// exponential backoff so that every (re)connect triggers a resync from
// GET /api/v1/status (events missed while disconnected are not replayed by
// the agent). A watchdog reconnects if nothing — not even the 15 s `ping` —
// arrives for `staleAfterMs`.

import { API_BASE } from "./client";
import type { HealthEvent, SessionEvent } from "./types";

export interface EventSourceLike {
  readonly readyState: number;
  onopen: ((ev: Event) => unknown) | null;
  onerror: ((ev: Event) => unknown) | null;
  addEventListener(type: string, listener: (ev: MessageEvent) => void): void;
  close(): void;
}

export type EventSourceFactory = (url: string) => EventSourceLike;

export type ConnState = "connecting" | "online" | "offline";

export interface EventStreamHandlers {
  /** Called after every successful (re)connect; resync state here. */
  onOpen: () => void;
  onSession: (ev: SessionEvent) => void;
  onHealth: (ev: HealthEvent) => void;
  onConn: (state: ConnState) => void;
}

export interface EventStreamOptions {
  url?: string;
  create?: EventSourceFactory;
  initialBackoffMs?: number;
  maxBackoffMs?: number;
  staleAfterMs?: number;
}

const defaultFactory: EventSourceFactory = (url) =>
  new EventSource(url) as unknown as EventSourceLike;

export class EventStream {
  private es: EventSourceLike | null = null;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private staleTimer: ReturnType<typeof setTimeout> | null = null;
  private backoff: number;
  private stopped = true;
  private readonly url: string;
  private readonly create: EventSourceFactory;
  private readonly initialBackoff: number;
  private readonly maxBackoff: number;
  private readonly staleAfter: number;

  constructor(
    private readonly h: EventStreamHandlers,
    opts: EventStreamOptions = {},
  ) {
    this.url = opts.url ?? `${API_BASE}/events`;
    this.create = opts.create ?? defaultFactory;
    this.initialBackoff = opts.initialBackoffMs ?? 1_000;
    this.maxBackoff = opts.maxBackoffMs ?? 15_000;
    this.staleAfter = opts.staleAfterMs ?? 45_000;
    this.backoff = this.initialBackoff;
  }

  start(): void {
    if (!this.stopped) return;
    this.stopped = false;
    this.connect();
  }

  stop(): void {
    this.stopped = true;
    this.teardown();
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.retryTimer = null;
  }

  private connect(): void {
    this.teardown();
    this.h.onConn("connecting");
    let es: EventSourceLike;
    try {
      es = this.create(this.url);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.es = es;
    es.onopen = () => {
      if (this.es !== es) return;
      this.backoff = this.initialBackoff;
      this.h.onConn("online");
      this.armWatchdog();
      this.h.onOpen();
    };
    es.onerror = () => {
      if (this.es !== es) return;
      // Take over from the browser's built-in retry: close and back off.
      this.h.onConn("offline");
      this.scheduleReconnect();
    };
    es.addEventListener("session", (m) => {
      if (this.es !== es) return;
      this.armWatchdog();
      const v = parse<SessionEvent>(m.data);
      if (v && typeof v.type === "string" && v.session) this.h.onSession(v);
    });
    es.addEventListener("health", (m) => {
      if (this.es !== es) return;
      this.armWatchdog();
      const v = parse<HealthEvent>(m.data);
      if (v?.capabilities && typeof v.overall === "string") this.h.onHealth(v);
    });
    es.addEventListener("ping", () => {
      if (this.es === es) this.armWatchdog();
    });
  }

  private scheduleReconnect(): void {
    this.teardown();
    if (this.stopped || this.retryTimer) return;
    const delay = this.backoff;
    this.backoff = Math.min(this.backoff * 2, this.maxBackoff);
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null;
      if (!this.stopped) this.connect();
    }, delay);
  }

  private armWatchdog(): void {
    if (this.staleTimer) clearTimeout(this.staleTimer);
    this.staleTimer = setTimeout(() => {
      this.staleTimer = null;
      if (this.stopped) return;
      this.h.onConn("offline");
      this.scheduleReconnect();
    }, this.staleAfter);
  }

  private teardown(): void {
    if (this.staleTimer) clearTimeout(this.staleTimer);
    this.staleTimer = null;
    const es = this.es;
    this.es = null;
    if (es) {
      es.onopen = null;
      es.onerror = null;
      es.close();
    }
  }
}

function parse<T>(data: unknown): T | null {
  if (typeof data !== "string" || data.length > 64 * 1024) return null;
  try {
    return JSON.parse(data) as T;
  } catch {
    return null;
  }
}
