"use client";

import { useEffect, useRef, useState } from "react";
import { errorMessage } from "@/lib/api";
import { diffJson, MAX_DIFF_ENTRIES, showValue } from "@/lib/diff";
import type { SchemeRecord, VersionMeta } from "@/lib/types";
import { useAuth } from "./Auth";
import { ErrorBox } from "./ui";

const CACHE_MAX = 20;

export function DiffTable({ before, after }: { before: unknown; after: unknown }) {
  const entries = diffJson(before, after);
  if (entries.length === 0) return <p>No differences.</p>;
  return (
    <div className="table-scroll">
      <table className="diff">
        <caption className="sr-only">Changed fields</caption>
        <thead>
          <tr>
            <th scope="col">Field</th>
            <th scope="col">Change</th>
            <th scope="col">Before</th>
            <th scope="col">After</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={e.path} className={`diff-${e.kind}`}>
              <td>
                <code>{e.path || "(record)"}</code>
              </td>
              <td>{e.kind}</td>
              <td className="diff-before">
                {e.kind === "added" ? "" : <del>{showValue(e.before)}</del>}
              </td>
              <td className="diff-after">
                {e.kind === "removed" ? "" : <ins>{showValue(e.after)}</ins>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {entries.length >= MAX_DIFF_ENTRIES && (
        <p className="muted">Showing the first {MAX_DIFF_ENTRIES} changes.</p>
      )}
    </div>
  );
}

export function VersionDiff({ id, history }: { id: string; history: VersionMeta[] }) {
  const { api } = useAuth();
  const versions = history.map((h) => h.version).sort((a, b) => b - a);
  const [to, setTo] = useState(versions[0] ?? 0);
  const [from, setFrom] = useState(versions[1] ?? versions[0] ?? 0);
  const cache = useRef(new Map<number, SchemeRecord>());
  const [pair, setPair] = useState<{ a: SchemeRecord; b: SchemeRecord } | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!from || !to) return;
    const ctrl = new AbortController();
    const get = async (v: number) => {
      const hit = cache.current.get(v);
      if (hit) return hit;
      const r = await api.schemeVersion(id, v, ctrl.signal);
      if (cache.current.size >= CACHE_MAX) {
        const first = cache.current.keys().next().value;
        if (first !== undefined) cache.current.delete(first);
      }
      cache.current.set(v, r.record);
      return r.record;
    };
    setError(null);
    Promise.all([get(from), get(to)])
      .then(([a, b]) => {
        if (!ctrl.signal.aborted) setPair({ a, b });
      })
      .catch((e: unknown) => {
        if (!ctrl.signal.aborted) setError(errorMessage(e));
      });
    return () => ctrl.abort();
  }, [api, id, from, to]);

  if (versions.length < 2) return <p className="muted">Only one version exists.</p>;
  return (
    <div className="stack">
      <div className="toolbar">
        <label>
          From version
          <select value={from} onChange={(e) => setFrom(Number(e.target.value))}>
            {versions.map((v) => (
              <option key={v} value={v}>
                v{v} ({history.find((h) => h.version === v)?.status})
              </option>
            ))}
          </select>
        </label>
        <label>
          To version
          <select value={to} onChange={(e) => setTo(Number(e.target.value))}>
            {versions.map((v) => (
              <option key={v} value={v}>
                v{v} ({history.find((h) => h.version === v)?.status})
              </option>
            ))}
          </select>
        </label>
      </div>
      <ErrorBox error={error} />
      {pair && <DiffTable before={pair.a} after={pair.b} />}
    </div>
  );
}
