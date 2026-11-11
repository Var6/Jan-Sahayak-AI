"use client";

import { type FormEvent, useEffect, useState } from "react";
import { useAuth, useUser } from "@/components/Auth";
import { Shell } from "@/components/Shell";
import { ErrorBox } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { fmtTime } from "@/lib/format";
import { can } from "@/lib/roles";
import type { AuditEntry } from "@/lib/types";

const ACTIONS = [
  "admin.login",
  "admin.login_failed",
  "admin.logout",
  "device.registered",
  "device.revoked",
  "scheme.draft_saved",
  "scheme.verified",
  "scheme.withdrawn",
  "bundle.published",
];

interface Filters {
  action: string;
  target_type: string;
  target_id: string;
  limit: number;
}

/** Bounded so a long session of "Load older" cannot grow without limit. */
const MAX_ROWS = 2000;

function Audit() {
  const { api } = useAuth();
  const [draft, setDraft] = useState<Filters>({
    action: "",
    target_type: "",
    target_id: "",
    limit: 50,
  });
  const [filters, setFilters] = useState<Filters>(draft);
  const [rows, setRows] = useState<AuditEntry[]>([]);
  const [next, setNext] = useState<string | undefined>();
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    const ctrl = new AbortController();
    setLoading(true);
    setError(null);
    api
      .audit({ ...filters, target_id: filters.target_id.trim() }, ctrl.signal)
      .then((p) => {
        setRows(p.entries);
        setNext(p.next_before);
      })
      .catch((e: unknown) => {
        if (!ctrl.signal.aborted) setError(errorMessage(e));
      })
      .finally(() => {
        if (!ctrl.signal.aborted) setLoading(false);
      });
    return () => ctrl.abort();
  }, [api, filters]);

  async function older() {
    if (!next) return;
    setLoading(true);
    try {
      const p = await api.audit({ ...filters, target_id: filters.target_id.trim(), before: next });
      setRows((r) => [...r, ...p.entries].slice(0, MAX_ROWS));
      setNext(rows.length + p.entries.length >= MAX_ROWS ? undefined : p.next_before);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setLoading(false);
    }
  }

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    setFilters({ ...draft });
  }

  return (
    <>
      <form className="filters" onSubmit={onSubmit} aria-label="Audit filters">
        <label>
          Action
          <select
            value={draft.action}
            onChange={(e) => setDraft({ ...draft, action: e.target.value })}
          >
            <option value="">Any</option>
            {ACTIONS.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
        </label>
        <label>
          Target type
          <select
            value={draft.target_type}
            onChange={(e) => setDraft({ ...draft, target_type: e.target.value })}
          >
            <option value="">Any</option>
            <option value="user">user</option>
            <option value="device">device</option>
            <option value="scheme">scheme</option>
            <option value="bundle">bundle</option>
          </select>
        </label>
        <label>
          Target id
          <input
            value={draft.target_id}
            maxLength={100}
            onChange={(e) => setDraft({ ...draft, target_id: e.target.value })}
          />
        </label>
        <label>
          Per page
          <select
            value={draft.limit}
            onChange={(e) => setDraft({ ...draft, limit: Number(e.target.value) })}
          >
            {[25, 50, 100, 200].map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <button type="submit">Apply</button>
      </form>
      <ErrorBox error={error} />
      {loading && rows.length === 0 && <p className="muted">Loading…</p>}
      {!loading && rows.length === 0 && !error && <p>No entries.</p>}
      {rows.length > 0 && (
        <div className="table-scroll">
          <table>
            <caption className="sr-only">Audit log, newest first</caption>
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Actor</th>
                <th scope="col">Action</th>
                <th scope="col">Target</th>
                <th scope="col">Details</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((e) => (
                <tr key={e.id}>
                  <td>{fmtTime(e.at)}</td>
                  <td>{e.actor}</td>
                  <td>
                    <code>{e.action}</code>
                  </td>
                  <td>
                    {e.target_type}
                    {e.target_id && (
                      <div className="small">
                        <code>{e.target_id}</code>
                      </div>
                    )}
                  </td>
                  <td>
                    {e.details &&
                    typeof e.details === "object" &&
                    Object.keys(e.details).length > 0 ? (
                      <details>
                        <summary>Show</summary>
                        <pre className="json">{JSON.stringify(e.details, null, 2)}</pre>
                      </details>
                    ) : (
                      "—"
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {next && (
        <button type="button" onClick={older} disabled={loading}>
          {loading ? "Loading…" : "Load older entries"}
        </button>
      )}
    </>
  );
}

function Gate() {
  const user = useUser();
  if (!can(user, "viewAudit")) return <ErrorBox error="The audit log needs the auditor role." />;
  return <Audit />;
}

export default function AuditPage() {
  return (
    <Shell title="Audit log">
      <Gate />
    </Shell>
  );
}
