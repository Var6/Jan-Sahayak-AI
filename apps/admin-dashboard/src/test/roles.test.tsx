import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { AuthProvider } from "@/components/Auth";
import { Can, navFor } from "@/components/Shell";
import { AdminApi } from "@/lib/api";
import { can, seesUnsuppressedAnalytics } from "@/lib/roles";
import type { Role } from "@/lib/types";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
  usePathname: () => "/",
  useSearchParams: () => new URLSearchParams(),
}));

const u = (...roles: Role[]) => ({ roles });

describe("can()", () => {
  it("mirrors the server's role checks", () => {
    expect(can(u("viewer"), "read")).toBe(true);
    expect(can(u("viewer"), "editSchemes")).toBe(false);
    expect(can(u("viewer"), "viewAudit")).toBe(false);
    expect(can(u("scheme_editor"), "editSchemes")).toBe(true);
    expect(can(u("scheme_editor"), "reviewSchemes")).toBe(false);
    expect(can(u("scheme_reviewer"), "reviewSchemes")).toBe(true);
    expect(can(u("scheme_reviewer"), "editSchemes")).toBe(false);
    expect(can(u("device_admin"), "manageDevices")).toBe(true);
    expect(can(u("auditor"), "viewAudit")).toBe(true);
    expect(can(u("auditor"), "manageDevices")).toBe(false);
  });
  it("admin satisfies everything; no roles satisfies nothing", () => {
    for (const c of [
      "read",
      "manageDevices",
      "editSchemes",
      "reviewSchemes",
      "viewAudit",
    ] as const) {
      expect(can(u("admin"), c)).toBe(true);
      expect(can(u(), c)).toBe(false);
      expect(can(null, c)).toBe(false);
    }
  });
  it("only admin sees unsuppressed analytics", () => {
    expect(seesUnsuppressedAnalytics(u("admin"))).toBe(true);
    expect(seesUnsuppressedAnalytics(u("auditor", "viewer"))).toBe(false);
  });
  it("hides the audit link from non-auditors", () => {
    expect(navFor(u("viewer")).map((n) => n.label)).toEqual(["Overview", "Devices", "Schemes"]);
    expect(navFor(u("auditor")).map((n) => n.label)).toContain("Audit log");
  });
});

function apiAs(...roles: Role[]) {
  const fetcher = vi.fn(
    async () =>
      new Response(JSON.stringify({ user: { id: "1", username: "x", display_name: "X", roles } }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
  );
  return new AdminApi({ fetcher });
}

describe("<Can>", () => {
  it("renders actions only for permitted roles", async () => {
    const { RequireAuth } = await import("@/components/Auth");
    const tree = (api: AdminApi) => (
      <AuthProvider api={api}>
        <RequireAuth>
          <p>page</p>
          <Can cap="reviewSchemes">
            <button type="button">Verify</button>
          </Can>
          <Can cap="editSchemes">
            <button type="button">Edit</button>
          </Can>
        </RequireAuth>
      </AuthProvider>
    );
    const r1 = render(tree(apiAs("scheme_editor")));
    await screen.findByText("page");
    expect(screen.queryByRole("button", { name: "Verify" })).toBeNull();
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
    r1.unmount();
    render(tree(apiAs("scheme_reviewer")));
    await screen.findByText("page");
    expect(screen.getByRole("button", { name: "Verify" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  });
});
