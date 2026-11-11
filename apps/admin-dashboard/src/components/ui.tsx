"use client";

import type { ReactNode } from "react";
import type { Connectivity } from "@/lib/devices";
import type { HealthState, RecordStatus } from "@/lib/types";

const STATUS_TEXT: Record<RecordStatus, string> = {
  VERIFIED: "Verified",
  NEEDS_REVIEW: "Needs review",
  OUTDATED: "Outdated (withdrawn)",
  UNVERIFIED: "Unverified",
};

const STATUS_ICON: Record<RecordStatus, string> = {
  VERIFIED: "✓",
  NEEDS_REVIEW: "!",
  OUTDATED: "×",
  UNVERIFIED: "?",
};

export function StatusBadge({ status, withdrawn }: { status: RecordStatus; withdrawn?: boolean }) {
  return (
    <span className={`badge badge-${status.toLowerCase()}`} data-testid="status-badge">
      <span aria-hidden="true">{STATUS_ICON[status]}</span> {STATUS_TEXT[status] ?? status}
      {withdrawn && status !== "OUTDATED" ? " · verified version withdrawn" : ""}
    </span>
  );
}

const HEALTH_CLASS: Record<HealthState, string> = {
  HEALTHY: "good",
  DEGRADED: "warn",
  RECOVERING: "warn",
  UNAVAILABLE: "bad",
  REQUIRES_ATTENTION: "bad",
};

export function HealthBadge({ state }: { state?: HealthState }) {
  if (!state) return <span className="muted">—</span>;
  return (
    <span className={`badge badge-${HEALTH_CLASS[state] ?? "warn"}`}>
      {state.replace("_", " ").toLowerCase()}
    </span>
  );
}

const CONN: Record<Connectivity, { cls: string; text: string; icon: string }> = {
  online: { cls: "good", text: "Online", icon: "●" },
  offline: { cls: "bad", text: "Offline", icon: "○" },
  never: { cls: "neutral", text: "No heartbeat yet", icon: "–" },
  revoked: { cls: "neutral", text: "Revoked", icon: "×" },
};

export function ConnectivityBadge({ c }: { c: Connectivity }) {
  const v = CONN[c];
  return (
    <span className={`badge badge-${v.cls}`}>
      <span aria-hidden="true">{v.icon}</span> {v.text}
    </span>
  );
}

export function ErrorBox({ error, details }: { error: string | null; details?: string[] }) {
  if (!error) return null;
  return (
    <div className="error" role="alert">
      <p>{error}</p>
      {details && details.length > 0 && (
        <ul>
          {details.map((d) => (
            <li key={d}>{d}</li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function Notice({ children }: { children: ReactNode }) {
  return (
    <div className="notice" role="status">
      {children}
    </div>
  );
}

export function Stat({ label, value, hint }: { label: string; value: ReactNode; hint?: string }) {
  return (
    <div className="stat">
      <div className="stat-value">{value}</div>
      <div className="stat-label">{label}</div>
      {hint && <div className="muted small">{hint}</div>}
    </div>
  );
}
