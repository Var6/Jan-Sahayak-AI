"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { type Capability, can, ROLE_LABELS } from "@/lib/roles";
import { RequireAuth, useAuth, useUser } from "./Auth";

const NAV: { href: string; label: string; cap: Capability }[] = [
  { href: "/", label: "Overview", cap: "read" },
  { href: "/devices/", label: "Devices", cap: "read" },
  { href: "/schemes/", label: "Schemes", cap: "read" },
  { href: "/audit/", label: "Audit log", cap: "viewAudit" },
];

export function navFor(user: Parameters<typeof can>[0]) {
  return NAV.filter((n) => can(user, n.cap));
}

function Header() {
  const user = useUser();
  const { logout } = useAuth();
  const pathname = usePathname() ?? "/";
  const items = navFor(user);
  return (
    <header className="topbar">
      <div className="brand">
        <span lang="hi">जन सहायक</span> <span>Jan Sahayak · Admin</span>
      </div>
      <nav aria-label="Main">
        <ul className="nav-list">
          {items.map((n) => {
            const current = n.href === "/" ? pathname === "/" : pathname.startsWith(n.href);
            return (
              <li key={n.href}>
                <Link href={n.href} aria-current={current ? "page" : undefined}>
                  {n.label}
                </Link>
              </li>
            );
          })}
        </ul>
      </nav>
      <div className="whoami">
        <span title={user.username}>{user.display_name || user.username}</span>
        <span className="muted small">{user.roles.map((r) => ROLE_LABELS[r] ?? r).join(", ")}</span>
        <button type="button" className="link-button" onClick={() => void logout()}>
          Log out
        </button>
      </div>
    </header>
  );
}

export function Shell({ title, children }: { title: string; children: ReactNode }) {
  return (
    <RequireAuth>
      <a className="skip" href="#main">
        Skip to content
      </a>
      <Header />
      <main id="main" tabIndex={-1}>
        <h1>{title}</h1>
        {children}
      </main>
    </RequireAuth>
  );
}

/** Renders children only when the user has the capability. */
export function Can({ cap, children }: { cap: Capability; children: ReactNode }) {
  const user = useUser();
  return can(user, cap) ? children : null;
}
