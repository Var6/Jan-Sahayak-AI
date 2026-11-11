"use client";

import { usePathname, useRouter } from "next/navigation";
import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { AdminApi, ApiError } from "@/lib/api";
import type { User } from "@/lib/types";

type AuthState =
  | { status: "loading" }
  | { status: "anonymous" }
  | { status: "error"; message: string }
  | { status: "ready"; user: User };

interface AuthContextValue {
  state: AuthState;
  api: AdminApi;
  setUser: (u: User) => void;
  logout: () => Promise<void>;
  retry: () => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children, api: injected }: { children: ReactNode; api?: AdminApi }) {
  const api = useMemo(() => injected ?? new AdminApi(), [injected]);
  const [state, setState] = useState<AuthState>({ status: "loading" });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    api.onUnauthorized = () => setState({ status: "anonymous" });
    return () => {
      api.onUnauthorized = undefined;
    };
  }, [api]);

  // biome-ignore lint/correctness/useExhaustiveDependencies: `attempt` re-runs the check on retry
  useEffect(() => {
    const ctrl = new AbortController();
    api
      .me(ctrl.signal)
      .then((r) => setState({ status: "ready", user: r.user }))
      .catch((err: unknown) => {
        if (ctrl.signal.aborted) return;
        if (err instanceof ApiError && err.status === 401) setState({ status: "anonymous" });
        else
          setState({
            status: "error",
            message: err instanceof Error ? err.message : "Unexpected error",
          });
      });
    return () => ctrl.abort();
  }, [api, attempt]);

  const setUser = useCallback((user: User) => setState({ status: "ready", user }), []);
  const logout = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      setState({ status: "anonymous" });
    }
  }, [api]);
  const retry = useCallback(() => {
    setState({ status: "loading" });
    setAttempt((a) => a + 1);
  }, []);

  const value = useMemo(
    () => ({ state, api, setUser, logout, retry }),
    [state, api, setUser, logout, retry],
  );
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const v = useContext(AuthContext);
  if (!v) throw new Error("useAuth outside AuthProvider");
  return v;
}

/** The logged-in user; only valid inside <RequireAuth>. */
export function useUser(): User {
  const { state } = useAuth();
  if (state.status !== "ready") throw new Error("useUser outside RequireAuth");
  return state.user;
}

/** Renders children only for a logged-in user; otherwise sends them to /login/. */
export function RequireAuth({ children }: { children: ReactNode }) {
  const { state, retry } = useAuth();
  const router = useRouter();
  const pathname = usePathname();
  useEffect(() => {
    if (state.status === "anonymous") {
      const next =
        pathname && pathname !== "/login/" ? `?next=${encodeURIComponent(pathname)}` : "";
      router.replace(`/login/${next}`);
    }
  }, [state.status, router, pathname]);
  if (state.status === "ready") return <>{children}</>;
  if (state.status === "error") {
    return (
      <div className="panel" role="alert">
        <p>Could not check your session: {state.message}</p>
        <button type="button" onClick={retry}>
          Try again
        </button>
      </div>
    );
  }
  return (
    <p className="muted" aria-live="polite">
      Loading…
    </p>
  );
}
