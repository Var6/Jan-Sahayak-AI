import { describe, expect, it, vi } from "vitest";
import { AdminApi, ApiError, errorMessage } from "@/lib/api";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("AdminApi", () => {
  it("sends same-origin JSON requests to /v1/admin with credentials", async () => {
    const fetcher = vi.fn(async () =>
      jsonResponse(200, {
        user: { id: "u", username: "a", display_name: "A", roles: ["viewer"] },
        expires_in: 60,
      }),
    );
    const api = new AdminApi({ fetcher });
    const r = await api.login("a", "pw");
    expect(r.user.username).toBe("a");
    const [url, init] = fetcher.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/v1/admin/login");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("same-origin");
    expect((init.headers as Record<string, string>)["Content-Type"]).toBe("application/json");
    expect(JSON.parse(init.body as string)).toEqual({ username: "a", password: "pw" });
  });

  it("builds query strings and skips empty values", async () => {
    const fetcher = vi.fn(async () => jsonResponse(200, { entries: [] }));
    const api = new AdminApi({ fetcher });
    await api.audit({ limit: 50, action: "scheme.verified", target_id: "", before: undefined });
    expect((fetcher.mock.calls[0] as unknown as [string])[0]).toBe(
      "/v1/admin/audit?limit=50&action=scheme.verified",
    );
  });

  it("encodes path segments", async () => {
    const fetcher = vi.fn(async () => jsonResponse(200, {}));
    const api = new AdminApi({ fetcher });
    await api.schemeVersion("a/../b", 3);
    expect((fetcher.mock.calls[0] as unknown as [string])[0]).toBe(
      "/v1/admin/schemes/a%2F..%2Fb/versions/3",
    );
  });

  it("maps error bodies to ApiError with details", async () => {
    const fetcher = vi.fn(async () =>
      jsonResponse(422, {
        error: {
          code: "invalid_record",
          message: "record failed validation",
          details: ['unknown category "x"', 5],
        },
      }),
    );
    const api = new AdminApi({ fetcher });
    const err = await api.saveDraft("x", 0, {} as never, "").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(422);
    expect((err as ApiError).code).toBe("invalid_record");
    expect((err as ApiError).details).toEqual(['unknown category "x"']);
  });

  it("calls onUnauthorized on 401 except for login", async () => {
    const fetcher = vi.fn(async () =>
      jsonResponse(401, { error: { code: "unauthorized", message: "login required" } }),
    );
    const api = new AdminApi({ fetcher });
    const spy = vi.fn();
    api.onUnauthorized = spy;
    await expect(api.devices()).rejects.toBeInstanceOf(ApiError);
    expect(spy).toHaveBeenCalledTimes(1);
    await expect(api.login("a", "b")).rejects.toMatchObject({ code: "unauthorized" });
    expect(spy).toHaveBeenCalledTimes(1);
  });

  it("returns undefined for 204 and tolerates non-JSON errors", async () => {
    const api = new AdminApi({ fetcher: vi.fn(async () => new Response(null, { status: 204 })) });
    await expect(api.logout()).resolves.toBeUndefined();
    const api2 = new AdminApi({
      fetcher: vi.fn(async () => new Response("<html>bad gateway</html>", { status: 502 })),
    });
    await expect(api2.devices()).rejects.toMatchObject({ status: 502, code: "http_error" });
  });

  it("times out instead of hanging", async () => {
    const fetcher = vi.fn(
      (_url: string, init?: RequestInit) =>
        new Promise<Response>((_res, rej) => {
          init?.signal?.addEventListener("abort", () =>
            rej(new DOMException("aborted", "AbortError")),
          );
        }),
    ) as unknown as typeof fetch;
    const api = new AdminApi({ fetcher, timeoutMs: 20 });
    await expect(api.devices()).rejects.toMatchObject({ code: "timeout" });
  });

  it("reports network failures", async () => {
    const api = new AdminApi({
      fetcher: vi.fn(async () => Promise.reject(new TypeError("Failed to fetch"))),
    });
    await expect(api.devices()).rejects.toMatchObject({ code: "network" });
  });

  it("omits empty optional verify fields", async () => {
    const fetcher = vi.fn(async () => jsonResponse(200, {}));
    const api = new AdminApi({ fetcher });
    await api.verify("s", 2, "", "");
    expect(
      JSON.parse((fetcher.mock.calls[0] as unknown as [string, RequestInit])[1].body as string),
    ).toEqual({ version: 2 });
  });

  it("explains four-eyes and role errors", () => {
    expect(errorMessage(new ApiError(403, "own_edit", "x"))).toMatch(/Four-eyes/);
    expect(errorMessage(new ApiError(403, "forbidden", "x"))).toMatch(/role/);
  });
});
