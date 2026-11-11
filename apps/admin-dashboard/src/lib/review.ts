// Review-state helpers mirroring the central API rules
// (services/central-api/internal/server/admin_schemes.go). The server and
// the database enforce them; the UI uses them to explain and hide actions.

import type { VersionMeta } from "./types";

export function latestVersion(history: VersionMeta[], status: VersionMeta["status"]): number {
  return history.reduce((m, v) => (v.status === status && v.version > m ? v.version : m), 0);
}

/** True when the latest VERIFIED version exists and has not been withdrawn. */
export function hasLiveVerified(history: VersionMeta[]): boolean {
  const lv = latestVersion(history, "VERIFIED");
  return lv > 0 && latestVersion(history, "OUTDATED") < lv;
}

/**
 * Four-eyes rule: a reviewer may not verify a scheme if they authored ANY
 * NEEDS_REVIEW/UNVERIFIED version since its last VERIFIED version.
 */
export function isOwnEdit(history: VersionMeta[], username: string): boolean {
  const lv = latestVersion(history, "VERIFIED");
  return history.some(
    (v) =>
      v.version > lv &&
      (v.status === "NEEDS_REVIEW" || v.status === "UNVERIFIED") &&
      v.edited_by === username,
  );
}

/** A future YYYY-MM-DD (IST) or empty, as the verify endpoint requires. */
export function validReviewDue(s: string, todayIst: string): boolean {
  return s === "" || (/^\d{4}-\d{2}-\d{2}$/.test(s) && s > todayIst);
}
