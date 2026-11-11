import { afterEach, describe, expect, it, vi } from "vitest";
import { wavBlob } from "../audio/wav";
import { ApiError, DeviceApi, hasSession, safeAudioUrl } from "./client";

const hdr = (init?: RequestInit) => (init?.headers ?? {}) as Record<string, string>;

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

function apiWith(fetchImpl: (url: string, init?: RequestInit) => Promise<Response>) {
  const spy = vi.fn(fetchImpl);
  return { api: new DeviceApi({ fetch: spy }), spy };
}

async function caught(p: Promise<unknown>): Promise<ApiError> {
  try {
    await p;
  } catch (e) {
    expect(e).toBeInstanceOf(ApiError);
    return e as ApiError;
  }
  throw new Error("expected rejection");
}

afterEach(() => {
  vi.useRealTimers();
});

describe("DeviceApi requests", () => {
  it("GET /status without the kiosk header", async () => {
    const { api, spy } = apiWith(async () => json({ device_id: "d" }));
    await api.status();
    const [url, init] = spy.mock.calls[0] ?? [];
    expect(url).toBe("/api/v1/status");
    expect(init?.method).toBe("GET");
    expect(hdr(init)["X-JS-Kiosk"]).toBeUndefined();
  });

  it("sends X-JS-Kiosk: 1 and JSON on every mutating call", async () => {
    const { api, spy } = apiWith(async () => json({ session: {} }));
    await api.startSession("en");
    await api.listen("s1");
    await api.sendText("s1", "  नमस्ते  ");
    await api.chooseOption("s1", "opt-2");
    await api.speakingDone("s1");
    await api.setLanguage("s1", "hi");
    await api.touch("s1");
    await api.end("s1");
    const seen = spy.mock.calls.map(([u, i]) => {
      const h = i?.headers as Record<string, string>;
      expect(i?.method).toBe("POST");
      expect(h["X-JS-Kiosk"]).toBe("1");
      expect(h["Content-Type"]).toBe("application/json");
      return [u, i?.body];
    });
    expect(seen).toEqual([
      ["/api/v1/session/start", '{"language":"en"}'],
      ["/api/v1/session/s1/listen", "{}"],
      ["/api/v1/session/s1/text", '{"text":"नमस्ते"}'],
      ["/api/v1/session/s1/option", '{"option_id":"opt-2"}'],
      ["/api/v1/session/s1/speaking-done", "{}"],
      ["/api/v1/session/s1/language", '{"language":"hi"}'],
      ["/api/v1/session/s1/touch", "{}"],
      ["/api/v1/session/s1/end", "{}"],
    ]);
  });

  it("uploads audio as audio/wav", async () => {
    const { api, spy } = apiWith(async () => json({ session: {}, reply: {} }));
    const wav = wavBlob(new Int16Array(160));
    await api.sendAudio("s1", wav);
    const [url, init] = spy.mock.calls[0] ?? [];
    expect(url).toBe("/api/v1/session/s1/audio");
    expect(hdr(init)["Content-Type"]).toBe("audio/wav");
    expect(hdr(init)["X-JS-Kiosk"]).toBe("1");
    expect(init?.body).toBe(wav);
  });

  it("escapes the session id in paths", async () => {
    const { api, spy } = apiWith(async () => json({ session: {} }));
    await api.touch("a/b?c");
    expect(spy.mock.calls[0]?.[0]).toBe("/api/v1/session/a%2Fb%3Fc/touch");
  });
});

describe("DeviceApi errors", () => {
  it("maps the documented error body to ApiError", async () => {
    const { api } = apiWith(async () =>
      json({ error: { code: "bad_state", message: "not listening" } }, 409),
    );
    const e = await caught(api.listen("s1"));
    expect(e.code).toBe("bad_state");
    expect(e.status).toBe(409);
    expect(e.message).toBe("not listening");
    expect(e.sessionGone).toBe(false);
  });

  it.each(["no_session", "wrong_session"])("%s marks the session as gone", async (code) => {
    const { api } = apiWith(async () => json({ error: { code, message: "" } }, 404));
    expect((await caught(api.touch("s1"))).sessionGone).toBe(true);
  });

  it("button_disabled from the Pi", async () => {
    const { api } = apiWith(async () =>
      json({ error: { code: "button_disabled", message: "" } }, 403),
    );
    expect((await caught(api.startSession())).code).toBe("button_disabled");
  });

  it("falls back to status-derived codes for non-JSON errors", async () => {
    const { api } = apiWith(async () => new Response("<html>bad gateway</html>", { status: 503 }));
    const e = await caught(api.status());
    expect(e.code).toBe("unavailable");
    expect(e.status).toBe(503);
    const { api: api2 } = apiWith(async () => new Response("", { status: 413 }));
    expect((await caught(api2.touch("s"))).code).toBe("too_large");
    const { api: api3 } = apiWith(async () => new Response("x", { status: 500 }));
    expect((await caught(api3.touch("s"))).code).toBe("internal");
  });

  it("rejects invalid JSON on success as bad_response", async () => {
    const { api } = apiWith(async () => new Response("not json", { status: 200 }));
    expect((await caught(api.status())).code).toBe("bad_response");
  });

  it("maps fetch failures to network", async () => {
    const { api } = apiWith(async () => {
      throw new TypeError("Failed to fetch");
    });
    const e = await caught(api.status());
    expect(e.code).toBe("network");
    expect(e.status).toBe(0);
  });

  it("times out hung requests", async () => {
    vi.useFakeTimers();
    const { api } = apiWith(
      (_u, init) =>
        new Promise((_res, rej) => {
          init?.signal?.addEventListener("abort", () =>
            rej(new DOMException("aborted", "AbortError")),
          );
        }),
    );
    const p = caught(api.status());
    await vi.advanceTimersByTimeAsync(5_001);
    expect((await p).code).toBe("timeout");
  });

  it("reports caller aborts as aborted", async () => {
    const ctl = new AbortController();
    const { api } = apiWith(
      (_u, init) =>
        new Promise((_res, rej) => {
          init?.signal?.addEventListener("abort", () =>
            rej(new DOMException("aborted", "AbortError")),
          );
        }),
    );
    const p = caught(api.status(ctl.signal));
    ctl.abort();
    expect((await p).code).toBe("aborted");
  });

  it("validates limits before sending", async () => {
    const { api, spy } = apiWith(async () => json({}));
    expect((await caught(api.sendText("s", "   "))).code).toBe("bad_request");
    expect((await caught(api.sendText("s", "क".repeat(501)))).code).toBe("too_large");
    const big = new Blob([new Uint8Array(1024 * 1024 + 1)], { type: "audio/wav" });
    expect((await caught(api.sendAudio("s", big))).code).toBe("too_large");
    expect((await caught(api.touch(""))).code).toBe("no_session");
    expect(spy).not.toHaveBeenCalled();
  });

  it("counts characters, not UTF-16 units, for the 500-char limit", async () => {
    const { api, spy } = apiWith(async () => json({ session: {}, reply: {} }));
    await api.sendText("s", "क".repeat(500));
    expect(spy).toHaveBeenCalledTimes(1);
  });
});

describe("helpers", () => {
  it("hasSession", () => {
    expect(hasSession({ activity: "IDLE", camera_consent: false, turns: 0 })).toBe(false);
    expect(hasSession({ id: "x", activity: "READY", camera_consent: false, turns: 0 })).toBe(true);
    expect(hasSession(null)).toBe(false);
  });

  it("safeAudioUrl only accepts same-origin API paths", () => {
    expect(safeAudioUrl("/api/v1/session/x/audio/1.wav")).toBe("/api/v1/session/x/audio/1.wav");
    expect(safeAudioUrl("https://evil.example/a.wav")).toBeNull();
    expect(safeAudioUrl("//evil.example/api/v1/a.wav")).toBeNull();
    expect(safeAudioUrl("/api/v1/../../etc")).toBeNull();
    expect(safeAudioUrl(undefined)).toBeNull();
  });
});
