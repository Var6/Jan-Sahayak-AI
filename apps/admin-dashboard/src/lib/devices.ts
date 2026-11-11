import type { DeviceListItem } from "./types";

/**
 * A device counts as online when its last heartbeat arrived within this
 * window. Provisional: align with the device agent's heartbeat interval
 * once that is fixed.
 */
export const ONLINE_WINDOW_MS = 15 * 60 * 1000;

export type Connectivity = "online" | "offline" | "never" | "revoked";

export function connectivity(
  d: Pick<DeviceListItem, "status" | "last_heartbeat">,
  now: number,
): Connectivity {
  if (d.status === "revoked") return "revoked";
  const at = d.last_heartbeat?.received_at;
  if (!at) return "never";
  const t = Date.parse(at);
  if (Number.isNaN(t)) return "never";
  return now - t <= ONLINE_WINDOW_MS ? "online" : "offline";
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** Client checks for POST /v1/admin/devices (the server re-validates). */
export function validateNewDevice(d: {
  id: string;
  public_key: string;
  name: string;
  district: string;
}): string[] {
  const errs: string[] = [];
  if (!UUID_RE.test(d.id.trim()))
    errs.push("Device id must be a lowercase UUID from the device identity file.");
  const pk = d.public_key.trim();
  let keyOk = /^[A-Za-z0-9+/]{43}=$/.test(pk);
  if (keyOk) {
    try {
      keyOk = atob(pk).length === 32;
    } catch {
      keyOk = false;
    }
  }
  if (!keyOk)
    errs.push("Public key must be a base64 Ed25519 public key (32 bytes, 44 characters).");
  const name = d.name.trim();
  if (!name || [...name].length > 100) errs.push("Name is required (at most 100 characters).");
  const district = d.district.trim();
  if (!district || [...district].length > 60)
    errs.push("District is required (at most 60 characters).");
  return errs;
}
