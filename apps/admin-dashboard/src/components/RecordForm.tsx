"use client";

import { type FormEvent, type ReactNode, useId, useState } from "react";
import { CATEGORY_LABELS } from "@/lib/labels";
import {
  CATEGORIES,
  emptyText,
  parseRecordJson,
  type ValidationIssue,
  validateRecord,
} from "@/lib/scheme";
import type { Doc, SchemeRecord, Source, Text } from "@/lib/types";
import { ErrorBox } from "./ui";

const MAX_ROWS = 60;

function TextPair({
  label,
  value,
  onChange,
  multiline,
  required,
}: {
  label: string;
  value: Text | undefined;
  onChange: (t: Text) => void;
  multiline?: boolean;
  required?: "hi" | "both";
}) {
  const v = value ?? emptyText();
  const id = useId();
  const field = (lang: "hi" | "en", text: string, set: (s: string) => void) =>
    multiline ? (
      <textarea
        id={`${id}-${lang}`}
        lang={lang}
        value={text}
        rows={3}
        onChange={(e) => set(e.target.value)}
      />
    ) : (
      <input id={`${id}-${lang}`} lang={lang} value={text} onChange={(e) => set(e.target.value)} />
    );
  return (
    <fieldset className="pair">
      <legend>{label}</legend>
      <label htmlFor={`${id}-hi`}>
        हिन्दी (Hindi){required ? " *" : ""}
        {field("hi", v.hi, (hi) => onChange({ ...v, hi }))}
      </label>
      <label htmlFor={`${id}-en`}>
        English{required === "both" ? " *" : ""}
        {field("en", v.en, (en) => onChange({ ...v, en }))}
      </label>
    </fieldset>
  );
}

function Rows<T>({
  label,
  items,
  onChange,
  make,
  render,
  hint,
}: {
  label: string;
  items: T[] | null | undefined;
  onChange: (xs: T[]) => void;
  make: () => T;
  render: (item: T, set: (x: T) => void, i: number) => ReactNode;
  hint?: string;
}) {
  const xs = items ?? [];
  return (
    <fieldset className="rows">
      <legend>{label}</legend>
      {hint && <p className="muted small">{hint}</p>}
      {xs.map((it, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: rows are positional (ordered steps)
        <div className="row" key={i}>
          <div className="row-body">
            {render(it, (x) => onChange(xs.map((y, j) => (j === i ? x : y))), i)}
          </div>
          <div className="row-actions">
            <button
              type="button"
              className="secondary"
              aria-label={`Move ${label} item ${i + 1} up`}
              disabled={i === 0}
              onClick={() => {
                const c = [...xs];
                [c[i - 1], c[i]] = [c[i] as T, c[i - 1] as T];
                onChange(c);
              }}
            >
              ↑
            </button>
            <button
              type="button"
              className="secondary"
              aria-label={`Remove ${label} item ${i + 1}`}
              onClick={() => onChange(xs.filter((_, j) => j !== i))}
            >
              Remove
            </button>
          </div>
        </div>
      ))}
      <button
        type="button"
        className="secondary"
        disabled={xs.length >= MAX_ROWS}
        onClick={() => onChange([...xs, make()])}
      >
        Add {label.toLowerCase()} item
      </button>
    </fieldset>
  );
}

function StringList({
  label,
  value,
  onChange,
  hint,
}: {
  label: string;
  value: string[] | null | undefined;
  onChange: (v: string[]) => void;
  hint: string;
}) {
  const id = useId();
  return (
    <label>
      {label}
      <textarea
        aria-describedby={`${id}-h`}
        rows={3}
        value={(value ?? []).join("\n")}
        onChange={(e) =>
          onChange(
            e.target.value
              .split("\n")
              .map((s) => s.trimStart())
              .slice(0, 200),
          )
        }
        onBlur={(e) =>
          onChange(
            e.target.value
              .split("\n")
              .map((s) => s.trim())
              .filter(Boolean)
              .slice(0, 200),
          )
        }
      />
      <span id={`${id}-h`} className="muted small">
        {hint}
      </span>
    </label>
  );
}

/** Optional bilingual field: both halves empty → the field is omitted. */
function optText(t: Text): Text | undefined {
  return t.hi || t.en ? t : undefined;
}

function cleanRecord(r: SchemeRecord): SchemeRecord {
  const out: SchemeRecord = { ...r };
  for (const k of ["age_rule", "income_rule", "fees_note"] as const) {
    const v = out[k];
    if (v) out[k] = optText(v);
    if (out[k] === undefined) delete out[k];
  }
  out.aliases = (out.aliases ?? []).map((s) => s.trim()).filter(Boolean);
  out.keywords = (out.keywords ?? []).map((s) => s.trim()).filter(Boolean);
  if (!out.helpline) delete out.helpline;
  return out;
}

export function RecordForm({
  initial,
  isNew,
  onSave,
  busy,
  serverError,
  serverDetails,
}: {
  initial: SchemeRecord;
  isNew: boolean;
  onSave: (r: SchemeRecord, changeNote: string) => void;
  busy?: boolean;
  serverError?: string | null;
  serverDetails?: string[];
}) {
  const [rec, setRec] = useState<SchemeRecord>(initial);
  const [mode, setMode] = useState<"form" | "json">("form");
  const [json, setJson] = useState("");
  const [jsonError, setJsonError] = useState<string | null>(null);
  const [note, setNote] = useState("");
  const [issues, setIssues] = useState<ValidationIssue[]>([]);
  const set = <K extends keyof SchemeRecord>(k: K, v: SchemeRecord[K]) =>
    setRec((r) => ({ ...r, [k]: v }));

  function toJson() {
    setJson(JSON.stringify(cleanRecord(rec), null, 2));
    setJsonError(null);
    setMode("json");
  }
  function toForm(): SchemeRecord | null {
    const p = parseRecordJson(json);
    if (p.error || !p.record) {
      setJsonError(p.error ?? "Invalid record");
      return null;
    }
    setRec(p.record);
    setJsonError(null);
    setMode("form");
    return p.record;
  }

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    let r = rec;
    if (mode === "json") {
      const p = parseRecordJson(json);
      if (p.error || !p.record) {
        setJsonError(p.error ?? "Invalid record");
        return;
      }
      r = p.record;
    }
    r = cleanRecord(r);
    const v = validateRecord(r, { pathId: isNew ? undefined : initial.id });
    if ([...note].length > 500)
      v.push({ field: "change_note", message: "Change note is at most 500 characters." });
    setIssues(v);
    if (v.length) return;
    onSave(r, note.trim());
  }

  return (
    <form
      onSubmit={onSubmit}
      className="stack record-form"
      noValidate
      aria-label="Scheme record editor"
    >
      <fieldset className="tabs">
        <legend className="sr-only">Editor mode</legend>
        <button
          type="button"
          aria-pressed={mode === "form"}
          onClick={() => mode === "json" && toForm()}
        >
          Form
        </button>
        <button
          type="button"
          aria-pressed={mode === "json"}
          onClick={() => mode === "form" && toJson()}
        >
          Raw JSON
        </button>
      </fieldset>
      {mode === "json" ? (
        <div className="stack">
          <p className="muted small">
            Every field of the record, including those the form does not show (questions, optional
            documents, exceptions…). Version and verification are set by the server.
          </p>
          <label>
            Record JSON
            <textarea
              className="json-input"
              spellCheck={false}
              rows={28}
              value={json}
              onChange={(e) => setJson(e.target.value)}
            />
          </label>
          <ErrorBox error={jsonError} />
        </div>
      ) : (
        <div className="stack">
          <div className="grid-3">
            <label>
              Id (slug) *
              <input
                value={rec.id}
                readOnly={!isNew}
                spellCheck={false}
                maxLength={80}
                placeholder="bihar-example-scheme"
                onChange={(e) => set("id", e.target.value.trim().toLowerCase())}
              />
            </label>
            <label>
              Kind *
              <select
                value={rec.kind}
                onChange={(e) => set("kind", e.target.value as SchemeRecord["kind"])}
              >
                <option value="scheme">Scheme (benefit)</option>
                <option value="service">Service (certificate / document)</option>
              </select>
            </label>
            <label>
              Category / श्रेणी *
              <select value={rec.category} onChange={(e) => set("category", e.target.value)}>
                <option value="">Choose…</option>
                {CATEGORIES.map((c) => (
                  <option key={c} value={c}>
                    {CATEGORY_LABELS[c]?.en} / {CATEGORY_LABELS[c]?.hi}
                  </option>
                ))}
              </select>
            </label>
            <label>
              State
              <select value={rec.state} onChange={(e) => set("state", e.target.value)}>
                <option value="BR">Bihar (BR)</option>
                <option value="">Central scheme</option>
              </select>
            </label>
          </div>
          <TextPair
            label="Name / नाम"
            value={rec.name}
            onChange={(t) => set("name", t)}
            required="both"
          />
          <TextPair
            label="Department / विभाग"
            value={rec.department}
            onChange={(t) => set("department", t)}
          />
          <TextPair
            label="Summary / सारांश"
            value={rec.summary}
            onChange={(t) => set("summary", t)}
            multiline
            required="hi"
          />
          <Rows
            label="Eligibility"
            items={rec.eligibility}
            onChange={(x) => set("eligibility", x)}
            make={emptyText}
            render={(t, s) => <TextPair label="Rule" value={t} onChange={s} />}
          />
          <Rows
            label="Benefits"
            items={rec.benefits}
            onChange={(x) => set("benefits", x)}
            make={emptyText}
            render={(t, s) => <TextPair label="Benefit" value={t} onChange={s} />}
          />
          <TextPair
            label="Age rule (optional)"
            value={rec.age_rule}
            onChange={(t) => set("age_rule", t)}
          />
          <TextPair
            label="Income rule (optional)"
            value={rec.income_rule}
            onChange={(t) => set("income_rule", t)}
          />
          <Rows<Doc>
            label="Mandatory documents"
            items={rec.mandatory_docs}
            onChange={(x) => set("mandatory_docs", x)}
            make={() => ({ name: emptyText() })}
            render={(d, s) => (
              <TextPair label="Document" value={d.name} onChange={(t) => s({ ...d, name: t })} />
            )}
          />
          <Rows
            label="Application steps"
            items={rec.application_steps}
            onChange={(x) => set("application_steps", x)}
            make={emptyText}
            render={(t, s, i) => <TextPair label={`Step ${i + 1}`} value={t} onChange={s} />}
          />
          <Rows
            label="Where to apply"
            items={rec.where_to_apply}
            onChange={(x) => set("where_to_apply", x)}
            make={emptyText}
            render={(t, s) => <TextPair label="Place" value={t} onChange={s} />}
          />
          <TextPair
            label="Fees note (optional)"
            value={rec.fees_note}
            onChange={(t) => set("fees_note", t)}
          />
          <div className="grid-2">
            <label>
              Official URL (government domain)
              <input
                value={rec.official_url}
                type="url"
                spellCheck={false}
                onChange={(e) => set("official_url", e.target.value.trim())}
              />
            </label>
            <label>
              Helpline
              <input
                value={rec.helpline ?? ""}
                inputMode="tel"
                onChange={(e) => set("helpline", e.target.value)}
              />
            </label>
          </div>
          <StringList
            label="Aliases (one per line)"
            value={rec.aliases}
            onChange={(v) => set("aliases", v)}
            hint="Spoken variants, Hinglish spellings, abbreviations."
          />
          <StringList
            label="Keywords (one per line)"
            value={rec.keywords}
            onChange={(v) => set("keywords", v)}
            hint="Terms used to match a citizen's question to this record."
          />
          <Rows<Source>
            label="Sources"
            items={rec.sources}
            onChange={(x) => set("sources", x)}
            make={() => ({ url: "", title: "", accessed_on: "" })}
            hint="Official pages a reviewer will check every fact against. At least one is required."
            render={(src, s) => (
              <div className="grid-3">
                <label>
                  URL *
                  <input
                    value={src.url}
                    type="url"
                    spellCheck={false}
                    onChange={(e) => s({ ...src, url: e.target.value.trim() })}
                  />
                </label>
                <label>
                  Title
                  <input value={src.title} onChange={(e) => s({ ...src, title: e.target.value })} />
                </label>
                <label>
                  Accessed on *
                  <input
                    type="date"
                    value={src.accessed_on}
                    onChange={(e) => s({ ...src, accessed_on: e.target.value })}
                  />
                </label>
                <label className="span-3">
                  Verbatim excerpt (optional)
                  <textarea
                    rows={2}
                    value={src.excerpt ?? ""}
                    onChange={(e) => {
                      const { excerpt: _drop, ...rest } = src;
                      s(e.target.value ? { ...rest, excerpt: e.target.value } : rest);
                    }}
                  />
                </label>
              </div>
            )}
          />
          <p className="muted small">
            Questions, optional documents, alternative evidence and exceptions are kept as they are;
            edit them in Raw JSON.
          </p>
        </div>
      )}
      <label>
        Change note (what changed and why; max 500)
        <textarea rows={2} maxLength={500} value={note} onChange={(e) => setNote(e.target.value)} />
      </label>
      {issues.length > 0 && (
        <div className="error" role="alert" data-testid="form-issues">
          <p>Please fix before saving:</p>
          <ul>
            {issues.map((i) => (
              <li key={`${i.field}:${i.message}`}>{i.message}</li>
            ))}
          </ul>
        </div>
      )}
      <ErrorBox error={serverError ?? null} details={serverDetails} />
      <p className="muted small">
        Saving creates a new version with status NEEDS_REVIEW. Only a different human reviewer can
        verify it.
      </p>
      <button type="submit" disabled={busy}>
        {busy ? "Saving…" : "Save draft for review"}
      </button>
    </form>
  );
}
