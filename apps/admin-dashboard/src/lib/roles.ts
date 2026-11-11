// Role gating mirrors services/central-api/internal/server/admin_auth.go
// (roleReq.satisfiedBy). The server is authoritative; the UI only hides
// actions a user cannot perform.

import type { Role, User } from "./types";

export type Capability =
  | "read" // analytics, devices, schemes, bundles (any role)
  | "manageDevices" // register / revoke (device_admin)
  | "editSchemes" // save drafts (scheme_editor)
  | "reviewSchemes" // verify, withdraw, publish bundles (scheme_reviewer)
  | "viewAudit"; // audit log (auditor)

const NEEDS: Record<Exclude<Capability, "read">, Role> = {
  manageDevices: "device_admin",
  editSchemes: "scheme_editor",
  reviewSchemes: "scheme_reviewer",
  viewAudit: "auditor",
};

export function can(user: Pick<User, "roles"> | null | undefined, cap: Capability): boolean {
  const roles = user?.roles ?? [];
  if (roles.includes("admin")) return true;
  if (cap === "read") return roles.length > 0;
  return roles.includes(NEEDS[cap]);
}

/** Analytics counts below 5 are suppressed for every role except admin. */
export function seesUnsuppressedAnalytics(user: Pick<User, "roles"> | null | undefined): boolean {
  return user?.roles.includes("admin") ?? false;
}

export const ROLE_LABELS: Record<Role, string> = {
  viewer: "Viewer",
  device_admin: "Device admin",
  scheme_editor: "Scheme editor",
  scheme_reviewer: "Scheme reviewer",
  auditor: "Auditor",
  admin: "Administrator",
};
