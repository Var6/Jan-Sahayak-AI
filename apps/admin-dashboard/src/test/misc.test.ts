import { describe, expect, it } from "vitest";
import { connectivity, ONLINE_WINDOW_MS, validateNewDevice } from "@/lib/devices";
import { safeNext } from "@/lib/nav";
import { inlineScriptHashes } from "../../scripts/csp-lib.mjs";

describe("connectivity", () => {
  const now = Date.parse("2026-11-11T10:00:00Z");
  const at = (ms: number) => ({ received_at: new Date(now - ms).toISOString() }) as never;
  it("classifies by last heartbeat age", () => {
    expect(connectivity({ status: "active", last_heartbeat: at(60_000) }, now)).toBe("online");
    expect(connectivity({ status: "active", last_heartbeat: at(ONLINE_WINDOW_MS + 1) }, now)).toBe(
      "offline",
    );
    expect(connectivity({ status: "active" }, now)).toBe("never");
    expect(connectivity({ status: "revoked", last_heartbeat: at(1) }, now)).toBe("revoked");
  });
});

describe("validateNewDevice", () => {
  it("checks uuid, 32-byte key and lengths", () => {
    const key = btoa(String.fromCharCode(...new Uint8Array(32).fill(7)));
    expect(
      validateNewDevice({
        id: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
        public_key: key,
        name: "Kiosk 1",
        district: "Patna",
      }),
    ).toEqual([]);
    const errs = validateNewDevice({
      id: "ABC",
      public_key: btoa("short"),
      name: "",
      district: "x".repeat(61),
    });
    expect(errs).toHaveLength(4);
  });
});

describe("safeNext", () => {
  it("allows only in-app relative paths", () => {
    expect(safeNext("/schemes/")).toBe("/schemes/");
    for (const bad of [null, "", "https://evil.example", "//evil.example", "/\\evil", "/login/"])
      expect(safeNext(bad)).toBe("/");
  });
});

describe("inlineScriptHashes", () => {
  it("hashes inline scripts only, as CSP computes them", () => {
    const html =
      '<script src="/a.js" async=""></script><script>alert(1)</script><script type="application/json">{"a":1}</script><script id="x">self.__next_f.push([0])</script>';
    // sha256("alert(1)") in base64
    expect(inlineScriptHashes(html)).toContain(
      "sha256-bhHHL3z2vDgxUt0W3dWQOrprscmda2Y5pLsLg4GF+pI=",
    );
    expect(inlineScriptHashes(html)).toHaveLength(2);
  });
});
