"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { type FormEvent, Suspense, useState } from "react";
import { useAuth, useUser } from "@/components/Auth";
import { Can, Shell } from "@/components/Shell";
import { ErrorBox, Notice, StatusBadge } from "@/components/ui";
import { VersionDiff } from "@/components/VersionDiff";
import { ApiError, errorMessage } from "@/lib/api";
import { fmtTime, istDate } from "@/lib/format";
import { bilingual, CATEGORY_LABELS } from "@/lib/labels";
import { hasLiveVerified, isOwnEdit, validReviewDue } from "@/lib/review";
import type { SchemeDetail, Text } from "@/lib/types";
import { useLoad } from "@/lib/useLoad";

function T({ t }: { t?: Text | null }) {
  if (!t || (!t.hi && !t.en)) return <span className="muted">—</span>;
  return (
    <>
      {t.hi && <span lang="hi">{t.hi}</span>}
      {t.hi && t.en && <br />}
      {t.en && <span className="muted">{t.en}</span>}
    </>
  );
}

function TList({ items }: { items?: Text[] | null }) {
  if (!items || items.length === 0) return <span className="muted">—</span>;
  return (
    <ol>
      {items.map((t, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: ordered, read-only list
        <li key={i}>
          <T t={t} />
        </li>
      ))}
    </ol>
  );
}

function RecordView({ d }: { d: SchemeDetail }) {
  const r = d.record;
  const cat = bilingual(CATEGORY_LABELS, r.category);
  return (
    <dl className="kv record">
      <dt>Name / नाम</dt>
      <dd>
        <T t={r.name} />
      </dd>
      <dt>Kind · category · state</dt>
      <dd>
        {r.kind} · {cat.en} / <span lang="hi">{cat.hi}</span> ·{" "}
        {r.state === "BR" ? "Bihar" : "Central"}
      </dd>
      <dt>Department / विभाग</dt>
      <dd>
        <T t={r.department} />
      </dd>
      <dt>Summary / सारांश</dt>
      <dd>
        <T t={r.summary} />
      </dd>
      <dt>Eligibility / पात्रता</dt>
      <dd>
        <TList items={r.eligibility} />
      </dd>
      <dt>Benefits / लाभ</dt>
      <dd>
        <TList items={r.benefits} />
      </dd>
      <dt>Required documents / ज़रूरी दस्तावेज़</dt>
      <dd>
        <TList items={r.mandatory_docs?.map((x) => x.name)} />
      </dd>
      <dt>How to apply / आवेदन कैसे करें</dt>
      <dd>
        <TList items={r.application_steps} />
      </dd>
      <dt>Where to apply / कहाँ आवेदन करें</dt>
      <dd>
        <TList items={r.where_to_apply} />
      </dd>
      <dt>Official URL</dt>
      <dd>{r.official_url ? <code>{r.official_url}</code> : "—"}</dd>
      <dt>Helpline</dt>
      <dd>{r.helpline || "—"}</dd>
      <dt>Sources</dt>
      <dd>
        <Sources d={d} />
      </dd>
      <dt>Verification</dt>
      <dd>
        {r.verification.status}
        {r.last_verified && ` · last verified ${r.last_verified}`}
        {r.verification.review_due && ` · review due ${r.verification.review_due}`}
        {r.verification.notes && <div className="muted">Notes: {r.verification.notes}</div>}
      </dd>
    </dl>
  );
}

function Sources({ d }: { d: SchemeDetail }) {
  const s = d.record.sources ?? [];
  if (s.length === 0) return <span className="muted">none</span>;
  return (
    <ul>
      {s.map((x) => (
        <li key={x.url}>
          <a href={x.url} target="_blank" rel="noopener noreferrer">
            {x.title || x.url}
          </a>{" "}
          <span className="muted small">(accessed {x.accessed_on})</span>
          {x.excerpt && <blockquote className="small">{x.excerpt}</blockquote>}
        </li>
      ))}
    </ul>
  );
}

function Verify({ d, onDone }: { d: SchemeDetail; onDone: () => void }) {
  const { api } = useAuth();
  const me = useUser();
  const [checked, setChecked] = useState(false);
  const [notes, setNotes] = useState("");
  const [due, setDue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  if (d.current.status !== "NEEDS_REVIEW") return null;
  const own = isOwnEdit(d.history, me.username);
  const today = istDate(new Date());
  const dueOk = validReviewDue(due, today);
  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.verify(d.id, d.current.version, notes.trim(), due);
      onDone();
    } catch (err) {
      setError(errorMessage(err));
      if (err instanceof ApiError && err.status === 409) onDone();
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="panel" aria-labelledby="verify-h">
      <h2 id="verify-h">Verify version {d.current.version}</h2>
      {own ? (
        <Notice>
          Four-eyes rule: you edited a draft of this scheme since its last verified version, so a
          different reviewer must verify it.
        </Notice>
      ) : (
        <form onSubmit={onSubmit} className="stack">
          <p>
            Verifying makes this version&apos;s facts appear on kiosks as verified information. Open
            each source below and compare every eligibility rule, amount, document, step, URL and
            helpline.
          </p>
          <Sources d={d} />
          <label className="check">
            <input
              type="checkbox"
              checked={checked}
              onChange={(e) => setChecked(e.target.checked)}
              required
            />
            I, {me.display_name || me.username}, personally checked every fact in this version
            against the official sources listed above. This is my own human review, not an automated
            or AI check.
          </label>
          <label>
            Reviewer notes (optional, max 500)
            <textarea
              value={notes}
              maxLength={500}
              rows={3}
              onChange={(e) => setNotes(e.target.value)}
            />
          </label>
          <label>
            Review due (optional, a future date)
            <input
              type="date"
              value={due}
              min={today}
              onChange={(e) => setDue(e.target.value)}
              aria-invalid={!dueOk}
            />
          </label>
          {!dueOk && <p className="bad-text">Review due must be after today ({today}, IST).</p>}
          <ErrorBox error={error} />
          <button type="submit" disabled={!checked || !dueOk || busy}>
            {busy ? "Verifying…" : `Verify version ${d.current.version}`}
          </button>
        </form>
      )}
    </section>
  );
}

function Withdraw({ d, onDone }: { d: SchemeDetail; onDone: () => void }) {
  const { api } = useAuth();
  const [reason, setReason] = useState("");
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  if (!hasLiveVerified(d.history)) return null;
  const len = [...reason.trim()].length;
  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.withdraw(d.id, d.current.version, reason.trim());
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <details className="panel danger">
      <summary>Withdraw the verified version</summary>
      <form onSubmit={onSubmit} className="stack">
        <p>
          Use this when verified facts are no longer correct. The verified version is marked
          OUTDATED and the next bundle will not carry it; kiosks then show a pending correction only
          as &ldquo;not yet verified&rdquo;, or nothing for this scheme.
        </p>
        <label>
          Reason (3–400 characters)
          <textarea
            value={reason}
            maxLength={400}
            rows={2}
            onChange={(e) => setReason(e.target.value)}
          />
        </label>
        <label className="check">
          <input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />
          Withdraw it. Remember to publish a new bundle so devices stop showing it.
        </label>
        <ErrorBox error={error} />
        <button
          type="submit"
          className="danger"
          disabled={len < 3 || len > 400 || !confirm || busy}
        >
          {busy ? "Withdrawing…" : "Withdraw"}
        </button>
      </form>
    </details>
  );
}

function History({ d }: { d: SchemeDetail }) {
  return (
    <div className="table-scroll">
      <table>
        <caption className="sr-only">Version history</caption>
        <thead>
          <tr>
            <th scope="col">Version</th>
            <th scope="col">Status</th>
            <th scope="col">Edited by</th>
            <th scope="col">Verified / withdrawn by</th>
            <th scope="col">Note</th>
          </tr>
        </thead>
        <tbody>
          {d.history.map((h) => (
            <tr key={h.version}>
              <td>
                v{h.version}
                {h.based_on_version ? (
                  <div className="muted small">from v{h.based_on_version}</div>
                ) : null}
              </td>
              <td>
                <StatusBadge status={h.status} />
              </td>
              <td>
                {h.edited_by}
                <div className="muted small">{fmtTime(h.edited_at)}</div>
              </td>
              <td>
                {h.verified_by && (
                  <>
                    {h.verified_by}
                    <div className="muted small">{fmtTime(h.verified_at)}</div>
                  </>
                )}
                {h.withdrawn_by && <>withdrawn by {h.withdrawn_by}</>}
                {!h.verified_by && !h.withdrawn_by && "—"}
              </td>
              <td>{h.change_note || "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function SchemeView() {
  const { api } = useAuth();
  const id = useSearchParams().get("id") ?? "";
  const { data, error, loading, reload } = useLoad(
    (s) => (id ? api.scheme(id, s) : Promise.reject(new Error("No scheme id in the address."))),
    [api, id],
  );
  return (
    <>
      <p>
        <Link href="/schemes/">← All schemes</Link>
      </p>
      <ErrorBox error={error} />
      {loading && !data && <p className="muted">Loading…</p>}
      {data && (
        <>
          <div className="toolbar">
            <h2 className="subtitle">
              {data.record.name.en || data.id} <span lang="hi">{data.record.name.hi}</span>
            </h2>
            <StatusBadge status={data.current.status} />
            <span className="muted">
              v{data.current.version} · edited by {data.current.edited_by}{" "}
              {fmtTime(data.current.edited_at)}
            </span>
            <Can cap="editSchemes">
              <Link className="button" href={`/schemes/edit/?id=${encodeURIComponent(data.id)}`}>
                Edit as new draft
              </Link>
            </Can>
          </div>
          {data.current.status !== "VERIFIED" && (
            <Notice>
              {data.current.status === "NEEDS_REVIEW"
                ? "This version has not been verified by a human reviewer. Kiosks show it only as “not yet verified”."
                : data.current.status === "OUTDATED"
                  ? "The verified version was withdrawn. Kiosks no longer show it as fact."
                  : "Unverified record."}
            </Notice>
          )}
          <Can cap="reviewSchemes">
            <Verify d={data} onDone={reload} />
            <Withdraw d={data} onDone={reload} />
          </Can>
          <section className="panel">
            <h2>Current version (v{data.current.version})</h2>
            <RecordView d={data} />
            <details>
              <summary>Raw record JSON</summary>
              <pre className="json">{JSON.stringify(data.record, null, 2)}</pre>
            </details>
          </section>
          <section className="panel">
            <h2>Version history</h2>
            <History d={data} />
          </section>
          <section className="panel">
            <h2>Compare versions</h2>
            <VersionDiff
              key={`${data.id}:${data.current.version}`}
              id={data.id}
              history={data.history}
            />
          </section>
        </>
      )}
    </>
  );
}

export default function SchemeDetailPage() {
  return (
    <Shell title="Scheme">
      <Suspense fallback={<p className="muted">Loading…</p>}>
        <SchemeView />
      </Suspense>
    </Shell>
  );
}
