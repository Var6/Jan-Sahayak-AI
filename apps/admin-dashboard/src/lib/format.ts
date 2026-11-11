import type { Count } from "./types";

const dtf = new Intl.DateTimeFormat("en-IN", {
  dateStyle: "medium",
  timeStyle: "short",
  timeZone: "Asia/Kolkata",
});

/** Formats an RFC 3339 timestamp in IST, or "—". */
export function fmtTime(s: string | undefined | null): string {
  if (!s) return "—";
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? s : `${dtf.format(d)} IST`;
}

/** A suppressed count (null) is shown as "<5" (never as 0). */
export function fmtCount(c: Count | undefined, minCount = 5): string {
  if (c === undefined) return "—";
  if (c === null) return `<${minCount}`;
  return c.toLocaleString("en-IN");
}

export function fmtPct(r: number | null | undefined): string {
  if (r === null || r === undefined || Number.isNaN(r)) return "—";
  return `${(r * 100).toFixed(1)}%`;
}

export function fmtBytes(n: number | undefined | null): string {
  if (n === undefined || n === null) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

export function fmtAge(ms: number): string {
  if (ms < 0) return "in the future (clock skew)";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h} h ago`;
  return `${Math.floor(h / 24)} days ago`;
}

/** YYYY-MM-DD for a date in IST. */
export function istDate(d: Date): string {
  return new Intl.DateTimeFormat("en-CA", { timeZone: "Asia/Kolkata" }).format(d);
}
