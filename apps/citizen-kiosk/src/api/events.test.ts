import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeEventSource } from "../test/fakes";
import { type ConnState, EventStream } from "./events";

function setup() {
  const h = {
    onOpen: vi.fn(),
    onSession: vi.fn(),
    onHealth: vi.fn(),
    onConn: vi.fn<(c: ConnState) => void>(),
  };
  const stream = new EventStream(h, {
    create: (url) => new FakeEventSource(url),
    initialBackoffMs: 1000,
    maxBackoffMs: 4000,
    staleAfterMs: 45_000,
  });
  return { h, stream };
}

beforeEach(() => {
  vi.useFakeTimers();
  FakeEventSource.instances = [];
});

afterEach(() => {
  vi.useRealTimers();
});

describe("EventStream", () => {
  it("connects to /api/v1/events and resyncs on open", () => {
    const { h, stream } = setup();
    stream.start();
    const es = FakeEventSource.latest();
    expect(es.url).toBe("/api/v1/events");
    expect(h.onConn).toHaveBeenLastCalledWith("connecting");
    es.open();
    expect(h.onConn).toHaveBeenLastCalledWith("online");
    expect(h.onOpen).toHaveBeenCalledTimes(1);
    stream.stop();
  });

  it("dispatches session and health events and ignores malformed data", () => {
    const { h, stream } = setup();
    stream.start();
    const es = FakeEventSource.latest();
    es.open();
    es.emit("session", { type: "session.reminder", session: { activity: "READY" } });
    es.emit("health", { overall: "DEGRADED", capabilities: { voice_input: false } });
    es.emit("session", "{not json");
    es.emit("session", { nope: 1 });
    es.emit("health", { overall: "HEALTHY" });
    expect(h.onSession).toHaveBeenCalledTimes(1);
    expect(h.onSession.mock.calls[0]?.[0].type).toBe("session.reminder");
    expect(h.onHealth).toHaveBeenCalledTimes(1);
    stream.stop();
  });

  it("reconnects with bounded exponential backoff and resyncs each time", () => {
    const { h, stream } = setup();
    stream.start();
    FakeEventSource.latest().fail();
    expect(h.onConn).toHaveBeenLastCalledWith("offline");
    expect(FakeEventSource.latest().closed).toBe(true);
    const delays: number[] = [];
    for (let i = 0; i < 4; i++) {
      const before = FakeEventSource.instances.length;
      let waited = 0;
      while (FakeEventSource.instances.length === before) {
        vi.advanceTimersByTime(250);
        waited += 250;
      }
      delays.push(waited);
      FakeEventSource.latest().fail();
    }
    expect(delays).toEqual([1000, 2000, 4000, 4000]);
    // A successful open resets the backoff and triggers a resync.
    vi.advanceTimersByTime(4000);
    FakeEventSource.latest().open();
    expect(h.onOpen).toHaveBeenCalledTimes(1);
    FakeEventSource.latest().fail();
    const n = FakeEventSource.instances.length;
    vi.advanceTimersByTime(1000);
    expect(FakeEventSource.instances.length).toBe(n + 1);
    stream.stop();
  });

  it("reconnects when the stream goes silent (no ping)", () => {
    const { h, stream } = setup();
    stream.start();
    const es = FakeEventSource.latest();
    es.open();
    vi.advanceTimersByTime(30_000);
    es.emit("ping", "{}");
    vi.advanceTimersByTime(30_000); // 30 s since last ping: still fine
    expect(es.closed).toBe(false);
    vi.advanceTimersByTime(15_001);
    expect(es.closed).toBe(true);
    expect(h.onConn).toHaveBeenLastCalledWith("offline");
    vi.advanceTimersByTime(1000);
    expect(FakeEventSource.instances.length).toBe(2);
    stream.stop();
  });

  it("stop() closes the source and cancels pending reconnects", () => {
    const { stream } = setup();
    stream.start();
    FakeEventSource.latest().fail();
    stream.stop();
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances.length).toBe(1);
  });

  it("ignores events from a source it has replaced", () => {
    const { h, stream } = setup();
    stream.start();
    const old = FakeEventSource.latest();
    old.fail();
    vi.advanceTimersByTime(1000);
    old.emit("session", { type: "session.ended", session: { activity: "IDLE" } });
    expect(h.onSession).not.toHaveBeenCalled();
    stream.stop();
  });
});
