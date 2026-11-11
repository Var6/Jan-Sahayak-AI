"use client";

import Link from "next/link";
import { useState } from "react";
import { useAuth } from "@/components/Auth";
import { Can, Shell } from "@/components/Shell";
import { ErrorBox, StatusBadge } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { fmtTime } from "@/lib/format";
import { bilingual, CATEGORY_LABELS } from "@/lib/labels";
import type { RecordStatus } from "@/lib/types";
import { useLoad } from "@/lib/useLoad";

function Bundles() {
  const { api } = useAuth();
  const { data, error, reload } = useLoad((s) => api.bundles(s), [api]);
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [pubError, setPubError] = useState<string | null>(null);
  async function publish() {
    setBusy(true);
    setPubError(null);
    setMsg(null);
    try {
      const r = await api.publishBundle();
      setMsg(
        `Published bundle ${r.bundle.bundle_version}: ${r.bundle.record_count} records, ${r.bundle.verified_count} verified.`,
      );
      setConfirm(false);
      reload();
    } catch (err) {
      setPubError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  const latest = data?.bundles[0];
  return (
    <section className="panel">
      <h2>Knowledge bundles</h2>
      <p>
        {latest
          ? `Latest: ${latest.bundle_version} (${latest.record_count} records, ${latest.verified_count} verified) published by ${latest.published_by} at ${fmtTime(latest.created_at)}.`
          : "No bundle published yet."}
      </p>
      <ErrorBox error={error} />
      <Can cap="reviewSchemes">
        {!confirm ? (
          <button type="button" onClick={() => setConfirm(true)}>
            Publish a new bundle…
          </button>
        ) : (
          <fieldset className="confirm">
            <legend className="sr-only">Confirm publishing</legend>
            <p>
              Devices will download the new signed bundle. Per scheme it carries the latest VERIFIED
              version (unless withdrawn), otherwise the latest draft awaiting review, which kiosks
              show only as &ldquo;not yet verified&rdquo;. A scheme with neither disappears from
              kiosks.
            </p>
            <button type="button" onClick={publish} disabled={busy}>
              {busy ? "Publishing…" : "Publish bundle"}
            </button>{" "}
            <button
              type="button"
              className="secondary"
              onClick={() => setConfirm(false)}
              disabled={busy}
            >
              Cancel
            </button>
          </fieldset>
        )}
        <ErrorBox error={pubError} />
        {msg && <p role="status">{msg}</p>}
      </Can>
    </section>
  );
}

function Schemes() {
  const { api } = useAuth();
  const { data, error, loading } = useLoad((s) => api.schemes(s), [api]);
  const [q, setQ] = useState("");
  const [status, setStatus] = useState<"" | RecordStatus>("");
  const all = data?.schemes ?? [];
  const needle = q.trim().toLowerCase();
  const rows = all.filter(
    (s) =>
      (!status || s.status === status) &&
      (!needle ||
        s.id.includes(needle) ||
        s.name_en.toLowerCase().includes(needle) ||
        s.name_hi.includes(q.trim())),
  );
  const counts = all.reduce<Record<string, number>>((m, s) => {
    m[s.status] = (m[s.status] ?? 0) + 1;
    return m;
  }, {});
  return (
    <>
      <Bundles />
      <div className="toolbar">
        <label>
          Search
          <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Name or id" />
        </label>
        <label>
          Status
          <select value={status} onChange={(e) => setStatus(e.target.value as "" | RecordStatus)}>
            <option value="">All ({all.length})</option>
            <option value="NEEDS_REVIEW">Needs review ({counts.NEEDS_REVIEW ?? 0})</option>
            <option value="VERIFIED">Verified ({counts.VERIFIED ?? 0})</option>
            <option value="OUTDATED">Outdated ({counts.OUTDATED ?? 0})</option>
            <option value="UNVERIFIED">Unverified ({counts.UNVERIFIED ?? 0})</option>
          </select>
        </label>
        <Can cap="editSchemes">
          <Link className="button" href="/schemes/edit/">
            New scheme draft
          </Link>
        </Can>
      </div>
      <ErrorBox error={error} />
      {loading && !data && <p className="muted">Loading schemes…</p>}
      {data && rows.length === 0 && <p>No schemes match.</p>}
      {rows.length > 0 && (
        <div className="table-scroll">
          <table>
            <caption className="sr-only">Schemes and services</caption>
            <thead>
              <tr>
                <th scope="col">Name / नाम</th>
                <th scope="col">Category</th>
                <th scope="col">Status</th>
                <th scope="col" className="num">
                  Version
                </th>
                <th scope="col">Updated</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((s) => {
                const cat = bilingual(CATEGORY_LABELS, s.category);
                return (
                  <tr key={s.id}>
                    <td>
                      <Link href={`/schemes/detail/?id=${encodeURIComponent(s.id)}`}>
                        {s.name_en || s.id}
                      </Link>
                      <div lang="hi">{s.name_hi}</div>
                      <div className="muted small">
                        <code>{s.id}</code> · {s.kind}
                      </div>
                    </td>
                    <td>
                      {cat.en}
                      {cat.hi && (
                        <div lang="hi" className="small">
                          {cat.hi}
                        </div>
                      )}
                    </td>
                    <td>
                      <StatusBadge status={s.status} withdrawn={s.withdrawn} />
                      {s.latest_verified_version !== undefined && s.status !== "VERIFIED" && (
                        <div className="muted small">
                          last verified: v{s.latest_verified_version}
                        </div>
                      )}
                    </td>
                    <td className="num">v{s.current_version}</td>
                    <td>{fmtTime(s.updated_at)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

export default function SchemesPage() {
  return (
    <Shell title="Schemes & services">
      <Schemes />
    </Shell>
  );
}
