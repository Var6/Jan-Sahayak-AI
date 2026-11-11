// Same-origin client for the central admin API (/v1/admin/*).
//
// The session cookie (js_admin, HttpOnly, SameSite=Strict, Path=/v1/admin)
// is sent by the browser; this code never sees it. Mutations are protected
// server-side by Go's http.CrossOriginProtection, which accepts these
// same-origin fetches. Every request has a timeout; nothing is retried
// automatically (mutations are not idempotent).

import type {
  AnalyticsSummary,
  AuditPage,
  BundleMeta,
  Device,
  DeviceDetail,
  DeviceListItem,
  SchemeDetail,
  SchemeListItem,
  SchemeRecord,
  SchemeVersionResponse,
  User,
} from "./types";

export const API_TIMEOUT_MS = 20_000;

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details: string[];
  constructor(status: number, code: string, message: string, details: string[] = []) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export type Fetcher = typeof fetch;

interface RequestOptions {
  method?: "GET" | "POST" | "PUT";
  body?: unknown;
  query?: Record<string, string | number | undefined>;
  signal?: AbortSignal;
}

function buildQuery(q?: RequestOptions["query"]): string {
  if (!q) return "";
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(q)) {
    if (v !== undefined && v !== "") p.set(k, String(v));
  }
  const s = p.toString();
  return s ? `?${s}` : "";
}

const MAX_ERROR_DETAILS = 50;

async function parseError(res: Response): Promise<ApiError> {
  let code = "http_error";
  let message = `Request failed (${res.status})`;
  let details: string[] = [];
  try {
    const body = (await res.json()) as {
      error?: { code?: unknown; message?: unknown; details?: unknown };
    };
    const e = body?.error;
    if (e && typeof e.code === "string") code = e.code;
    if (e && typeof e.message === "string") message = e.message;
    if (e && Array.isArray(e.details)) {
      details = e.details
        .filter((d): d is string => typeof d === "string")
        .slice(0, MAX_ERROR_DETAILS);
    }
  } catch {
    // Not JSON (e.g. a reverse proxy error page): keep the generic message.
  }
  return new ApiError(res.status, code, message, details);
}

export class AdminApi {
  private readonly fetcher: Fetcher;
  private readonly base: string;
  private readonly timeoutMs: number;
  /** Called on any 401 so the UI can return to the login screen. */
  onUnauthorized?: () => void;

  constructor(opts: { fetcher?: Fetcher; base?: string; timeoutMs?: number } = {}) {
    this.fetcher = opts.fetcher ?? ((...a) => fetch(...a));
    this.base = opts.base ?? "/v1/admin";
    this.timeoutMs = opts.timeoutMs ?? API_TIMEOUT_MS;
  }

  async request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
    const ctrl = new AbortController();
    const timer = setTimeout(
      () => ctrl.abort(new DOMException("timeout", "TimeoutError")),
      this.timeoutMs,
    );
    const onAbort = () => ctrl.abort(opts.signal?.reason);
    opts.signal?.addEventListener("abort", onAbort, { once: true });
    const headers: Record<string, string> = { Accept: "application/json" };
    let body: string | undefined;
    if (opts.body !== undefined) {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(opts.body);
    }
    let res: Response;
    try {
      res = await this.fetcher(`${this.base}${path}${buildQuery(opts.query)}`, {
        method: opts.method ?? "GET",
        headers,
        body,
        credentials: "same-origin",
        cache: "no-store",
        redirect: "error",
        signal: ctrl.signal,
      });
    } catch (err) {
      if (ctrl.signal.aborted && !opts.signal?.aborted) {
        throw new ApiError(0, "timeout", "The server did not answer in time. Try again.");
      }
      if (opts.signal?.aborted) throw err;
      throw new ApiError(0, "network", "Cannot reach the central server. Check the connection.");
    } finally {
      clearTimeout(timer);
      opts.signal?.removeEventListener("abort", onAbort);
    }
    if (!res.ok) {
      const e = await parseError(res);
      if (res.status === 401 && path !== "/login") this.onUnauthorized?.();
      throw e;
    }
    if (res.status === 204) return undefined as T;
    return (await res.json()) as T;
  }

  // --- auth ---
  login(username: string, password: string) {
    return this.request<{ user: User; expires_in: number }>("/login", {
      method: "POST",
      body: { username, password },
    });
  }
  logout() {
    return this.request<void>("/logout", { method: "POST" });
  }
  me(signal?: AbortSignal) {
    return this.request<{ user: User }>("/me", { signal });
  }

  // --- analytics ---
  analytics(
    q: { from?: string; to?: string; district?: string; device_id?: string },
    signal?: AbortSignal,
  ) {
    return this.request<AnalyticsSummary>("/analytics/summary", { query: q, signal });
  }

  // --- devices ---
  devices(signal?: AbortSignal) {
    return this.request<{ devices: DeviceListItem[]; limit: number }>("/devices", { signal });
  }
  device(id: string, signal?: AbortSignal) {
    return this.request<DeviceDetail>(`/devices/${encodeURIComponent(id)}`, { signal });
  }
  registerDevice(d: { id: string; public_key: string; name: string; district: string }) {
    return this.request<{ device: Device }>("/devices", { method: "POST", body: d });
  }
  revokeDevice(id: string, reason: string) {
    return this.request<{ device: Device }>(`/devices/${encodeURIComponent(id)}/revoke`, {
      method: "POST",
      body: { reason },
    });
  }

  // --- schemes ---
  schemes(signal?: AbortSignal) {
    return this.request<{ schemes: SchemeListItem[] }>("/schemes", { signal });
  }
  scheme(id: string, signal?: AbortSignal) {
    return this.request<SchemeDetail>(`/schemes/${encodeURIComponent(id)}`, { signal });
  }
  schemeVersion(id: string, version: number, signal?: AbortSignal) {
    return this.request<SchemeVersionResponse>(
      `/schemes/${encodeURIComponent(id)}/versions/${encodeURIComponent(String(version))}`,
      { signal },
    );
  }
  saveDraft(id: string, baseVersion: number, record: SchemeRecord, changeNote: string) {
    return this.request<SchemeVersionResponse>(`/schemes/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: { base_version: baseVersion, change_note: changeNote, record },
    });
  }
  verify(id: string, version: number, notes: string, reviewDue: string) {
    const body: { version: number; notes?: string; review_due?: string } = { version };
    if (notes) body.notes = notes;
    if (reviewDue) body.review_due = reviewDue;
    return this.request<SchemeVersionResponse>(`/schemes/${encodeURIComponent(id)}/verify`, {
      method: "POST",
      body,
    });
  }
  withdraw(id: string, version: number, reason: string) {
    return this.request<SchemeVersionResponse & { withdrawn_version: number }>(
      `/schemes/${encodeURIComponent(id)}/withdraw`,
      { method: "POST", body: { version, reason } },
    );
  }

  // --- bundles ---
  bundles(signal?: AbortSignal) {
    return this.request<{ bundles: BundleMeta[] }>("/bundles", { signal });
  }
  publishBundle() {
    return this.request<{ bundle: BundleMeta }>("/bundles", { method: "POST", body: {} });
  }

  // --- audit ---
  audit(
    q: {
      limit?: number;
      before?: string;
      action?: string;
      target_type?: string;
      target_id?: string;
    },
    signal?: AbortSignal,
  ) {
    return this.request<AuditPage>("/audit", { query: q, signal });
  }
}

/** Human-readable message for an error thrown by AdminApi. */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "forbidden":
        return "Your role does not allow this action.";
      case "own_edit":
        return "Four-eyes rule: you edited this scheme since its last verified version, so another reviewer must verify it.";
      case "csrf":
        return "The server refused a cross-origin request. Reload the page from the dashboard address.";
      case "throttled":
      case "busy":
        return `${err.message} Wait a few seconds and try again.`;
      default:
        return err.message;
    }
  }
  if (err instanceof Error) return err.message;
  return "Unexpected error.";
}
