"use client";

import { type FormEvent, useState } from "react";
import { useAuth } from "@/components/Auth";
import { fillDays, SessionsBarChart } from "@/components/BarChart";
import { Shell } from "@/components/Shell";
import { ErrorBox, Notice, Stat } from "@/components/ui";
import { fmtCount, fmtPct } from "@/lib/format";
import {
  bilingual,
  CATEGORY_LABELS,
  END_REASON_LABELS,
  LANGUAGE_LABELS,
  RESOLUTION_LABELS,
  SOURCE_LABELS,
} from "@/lib/labels";
import type { AnalyticsSummary, CountCell } from "@/lib/types";
import { useLoad } from "@/lib/useLoad";

interface Filters {
  from: string;
  to: string;
  district: string;
}

function CountTable({
  caption,
  rows,
  label,
  min,
}: {
  caption: string;
  rows: CountCell[];
  label: (k: string) => string;
  min: number;
}) {
  return (
    <table>
      <caption>{caption}</caption>
      <thead>
        <tr>
          <th scope="col">Value</th>
          <th scope="col" className="num">
            Count
          </th>
        </tr>
      </thead>
      <tbody>
        {rows.length === 0 && (
          <tr>
            <td colSpan={2} className="muted">
              No data
            </td>
          </tr>
        )}
        {rows.map((r) => (
          <tr key={r.key}>
            <td>{label(r.key)}</td>
            <td className="num">{fmtCount(r.count, min)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function Summary({ a }: { a: AnalyticsSummary }) {
  const min = a.suppression.min_count;
  return (
    <>
      <p className="muted">
        {a.from} to {a.to} ({a.timezone}){a.district ? ` · district ${a.district}` : ""}
      </p>
      {a.suppression.applied && (
        <Notice>
          <strong>Small counts are hidden.</strong> To protect citizens&apos; privacy, any count
          from 1 to {min - 1} is shown as &ldquo;&lt;{min}&rdquo;, and where only one value in a
          group would be hidden, the next smallest is hidden too so it cannot be worked out from the
          total. Rates built on a hidden count are shown as &ldquo;—&rdquo;.
        </Notice>
      )}
      <section aria-label="Key figures" className="stats">
        <Stat label="Sessions" value={fmtCount(a.sessions_total, min)} />
        <Stat
          label="Avg. turns per session"
          value={a.avg_turns_per_session === null ? "—" : a.avg_turns_per_session.toFixed(1)}
        />
        <Stat
          label="Resolution rate"
          value={fmtPct(a.resolution_rate)}
          hint="Sessions with ≥1 answered turn ÷ sessions with turns"
        />
        <Stat
          label="Unresolved sessions"
          value={fmtCount(a.sessions_unresolved, min)}
          hint="No turn answered, escalated or crisis-handled"
        />
        <Stat label="Turns" value={fmtCount(a.turns_total, min)} />
        <Stat label="Turns using the LLM" value={fmtCount(a.turns_using_llm, min)} />
      </section>
      <section className="panel">
        <SessionsBarChart data={fillDays(a.sessions_per_day, a.from, a.to)} minCount={min} />
      </section>
      <section className="panel">
        <h2>Categories</h2>
        <div className="table-scroll">
          <table>
            <caption className="sr-only">Turns, sessions and unresolved turns by category</caption>
            <thead>
              <tr>
                <th scope="col">Category / श्रेणी</th>
                <th scope="col" className="num">
                  Sessions
                </th>
                <th scope="col" className="num">
                  Turns
                </th>
                <th scope="col" className="num">
                  Unresolved turns
                </th>
              </tr>
            </thead>
            <tbody>
              {a.categories.length === 0 && (
                <tr>
                  <td colSpan={4} className="muted">
                    No data
                  </td>
                </tr>
              )}
              {a.categories.map((c) => {
                const l = bilingual(CATEGORY_LABELS, c.category);
                return (
                  <tr key={c.category || "(none)"}>
                    <td>
                      {l.en}
                      {l.hi && (
                        <>
                          {" "}
                          / <span lang="hi">{l.hi}</span>
                        </>
                      )}
                    </td>
                    <td className="num">{fmtCount(c.sessions, min)}</td>
                    <td className="num">{fmtCount(c.turns, min)}</td>
                    <td className="num">{fmtCount(c.unresolved_turns, min)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </section>
      <div className="grid-2">
        <section className="panel">
          <CountTable
            caption="Languages / भाषा (sessions)"
            rows={a.languages}
            min={min}
            label={(k) => {
              const l = bilingual(LANGUAGE_LABELS, k);
              return l.hi ? `${l.en} / ${l.hi}` : l.en;
            }}
          />
        </section>
        <section className="panel">
          <CountTable
            caption="Turn outcomes"
            rows={a.turns_by_resolution}
            min={min}
            label={(k) => RESOLUTION_LABELS[k] ?? k}
          />
        </section>
        <section className="panel">
          <CountTable
            caption="How sessions ended"
            rows={a.end_reasons}
            min={min}
            label={(k) => END_REASON_LABELS[k] ?? k}
          />
        </section>
        <section className="panel">
          <CountTable
            caption="Input method (turns)"
            rows={a.turns_by_source}
            min={min}
            label={(k) => SOURCE_LABELS[k] ?? k}
          />
        </section>
      </div>
    </>
  );
}

function Overview() {
  const { api } = useAuth();
  const [draft, setDraft] = useState<Filters>({ from: "", to: "", district: "" });
  const [applied, setApplied] = useState<Filters>(draft);
  const { data, error, loading } = useLoad(
    (signal) =>
      api.analytics(
        { from: applied.from, to: applied.to, district: applied.district.trim() },
        signal,
      ),
    [api, applied],
  );
  function onSubmit(e: FormEvent) {
    e.preventDefault();
    setApplied({ ...draft });
  }
  return (
    <>
      <form className="filters" onSubmit={onSubmit} aria-label="Analytics filters">
        <label>
          From
          <input
            type="date"
            value={draft.from}
            onChange={(e) => setDraft({ ...draft, from: e.target.value })}
          />
        </label>
        <label>
          To
          <input
            type="date"
            value={draft.to}
            onChange={(e) => setDraft({ ...draft, to: e.target.value })}
          />
        </label>
        <label>
          District
          <input
            value={draft.district}
            maxLength={60}
            placeholder="All"
            onChange={(e) => setDraft({ ...draft, district: e.target.value })}
          />
        </label>
        <button type="submit">Apply</button>
        <span className="muted small">
          Default: last 30 days. Anonymous counts only — no citizen text is stored.
        </span>
      </form>
      <ErrorBox error={error} />
      {loading && !data && <p className="muted">Loading analytics…</p>}
      {data && <Summary a={data} />}
    </>
  );
}

export default function OverviewPage() {
  return (
    <Shell title="Overview">
      <Overview />
    </Shell>
  );
}
