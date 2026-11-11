"use client";

import { useRouter, useSearchParams } from "next/navigation";
import { type FormEvent, Suspense, useEffect, useState } from "react";
import { useAuth } from "@/components/Auth";
import { ErrorBox } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { safeNext } from "@/lib/nav";

function LoginForm() {
  const { api, state, setUser } = useAuth();
  const router = useRouter();
  const params = useSearchParams();
  const next = safeNext(params.get("next"));
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (state.status === "ready") router.replace(next);
  }, [state.status, router, next]);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const r = await api.login(username, password);
      setPassword("");
      setUser(r.user);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={onSubmit} className="panel stack" aria-describedby="login-help">
      <label>
        Username
        <input
          name="username"
          autoComplete="username"
          required
          maxLength={40}
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />
      </label>
      <label>
        Password
        <input
          name="password"
          type="password"
          autoComplete="current-password"
          required
          maxLength={256}
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
      </label>
      <ErrorBox error={error} />
      <button type="submit" disabled={busy}>
        {busy ? "Signing in…" : "Sign in"}
      </button>
      <p id="login-help" className="muted small">
        Staff accounts are created by an administrator with the central-admin tool. Repeated failed
        attempts are slowed down.
      </p>
    </form>
  );
}

export default function LoginPage() {
  return (
    <main id="main" className="narrow">
      <p className="brand-large">
        <span lang="hi">जन सहायक</span> · Jan Sahayak
      </p>
      <h1>Staff sign in</h1>
      <Suspense fallback={<p className="muted">Loading…</p>}>
        <LoginForm />
      </Suspense>
    </main>
  );
}
