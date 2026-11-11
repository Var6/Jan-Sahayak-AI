// Client-side mirror of the essentials of internal/scheme.Record.Validate()
// (Go). The server re-validates every draft and its 422 `details` are shown
// verbatim; this only catches mistakes before a round trip. It cannot check
// that facts are true — that is the human reviewer's job.

import type { SchemeRecord, Text } from "./types";

/** Must equal scheme.Categories in internal/scheme/scheme.go (a test checks). */
export const CATEGORIES = [
  "pension",
  "scholarship",
  "education",
  "farmer",
  "loan",
  "subsidy",
  "self_employment",
  "housing",
  "health",
  "food_security",
  "certificate",
  "identity_document",
  "women_child",
  "disability",
  "labour",
] as const;

/** JSON keys of scheme.Record. The server refuses unknown fields. */
export const RECORD_KEYS = [
  "id",
  "version",
  "kind",
  "name",
  "aliases",
  "state",
  "department",
  "category",
  "keywords",
  "summary",
  "eligibility",
  "benefits",
  "age_rule",
  "income_rule",
  "questions",
  "mandatory_docs",
  "optional_docs",
  "alternative_evidence",
  "application_steps",
  "where_to_apply",
  "official_url",
  "helpline",
  "fees_note",
  "exceptions",
  "sources",
  "last_verified",
  "verification",
] as const;

const ID_RE = /^[a-z0-9]+(-[a-z0-9]+)*$/;
const PHONE_RE = /^[0-9 +-]{3,20}$/;
const DATE_RE = /^(\d{4})-(\d{2})-(\d{2})$/;

/** Same acceptance as Go time.Parse("2006-01-02"): a real calendar date. */
export function isValidDate(s: string): boolean {
  const m = DATE_RE.exec(s);
  if (!m) return false;
  const [y, mo, d] = [Number(m[1]), Number(m[2]), Number(m[3])];
  const dt = new Date(Date.UTC(y, mo - 1, d));
  return dt.getUTCFullYear() === y && dt.getUTCMonth() === mo - 1 && dt.getUTCDate() === d;
}

function httpUrl(s: string): URL | null {
  try {
    const u = new URL(s);
    return u.protocol === "https:" || u.protocol === "http:" ? u : null;
  } catch {
    return null;
  }
}

/** Mirrors officialHost() in scheme.go. */
export function isOfficialHost(host: string): boolean {
  const h = host.toLowerCase();
  return h.endsWith(".gov.in") || h.endsWith(".nic.in") || h === "gov.in";
}

export interface ValidationIssue {
  field: string;
  message: string;
}

export function validateRecord(r: SchemeRecord, opts: { pathId?: string } = {}): ValidationIssue[] {
  const out: ValidationIssue[] = [];
  const add = (field: string, message: string) => out.push({ field, message });
  if (!ID_RE.test(r.id ?? ""))
    add("id", "Id must be a lowercase slug, e.g. bihar-example-pension.");
  if (opts.pathId && r.id !== opts.pathId)
    add("id", "Id cannot change after the scheme is created.");
  if ([...(r.id ?? "")].length > 80) add("id", "Id is at most 80 characters.");
  if (r.kind !== "scheme" && r.kind !== "service") add("kind", "Kind must be scheme or service.");
  if (!r.name?.hi || !r.name?.en) add("name", "Name in Hindi and English is required.");
  if (!(CATEGORIES as readonly string[]).includes(r.category))
    add("category", "Choose a category.");
  if (r.state !== "" && r.state !== "BR")
    add("state", "State must be BR (Bihar) or empty (central scheme).");
  if (!r.summary?.hi) add("summary", "Hindi summary is required.");
  const sources = r.sources ?? [];
  if (sources.length === 0) add("sources", "At least one official source is required.");
  sources.forEach((s, i) => {
    if (!httpUrl(s?.url ?? "")) {
      add(`sources.${i}.url`, `Source ${i + 1}: URL must start with https:// or http://.`);
      return;
    }
    if (!isValidDate(s.accessed_on ?? ""))
      add(`sources.${i}.accessed_on`, `Source ${i + 1}: accessed on must be a date (YYYY-MM-DD).`);
  });
  if (r.official_url) {
    const u = httpUrl(r.official_url);
    if (!u) add("official_url", "Official URL must start with https:// or http://.");
    else if (!isOfficialHost(u.hostname))
      add(
        "official_url",
        `Official URL host "${u.hostname}" is not a government domain (.gov.in / .nic.in).`,
      );
  }
  if (r.helpline && !PHONE_RE.test(r.helpline))
    add("helpline", "Helpline may contain only digits, spaces, + and - (3–20 characters).");
  if (r.verification?.review_due && !isValidDate(r.verification.review_due))
    add("verification.review_due", "Review due must be YYYY-MM-DD.");
  return out;
}

/** Top-level keys the server would refuse (DisallowUnknownFields). */
export function unknownKeys(obj: Record<string, unknown>): string[] {
  const known = new Set<string>(RECORD_KEYS);
  return Object.keys(obj).filter((k) => !known.has(k));
}

export const emptyText = (): Text => ({ hi: "", en: "" });

export function emptyRecord(): SchemeRecord {
  return {
    id: "",
    version: 0,
    kind: "scheme",
    name: emptyText(),
    aliases: [],
    state: "BR",
    department: emptyText(),
    category: "",
    keywords: [],
    summary: emptyText(),
    eligibility: [],
    benefits: [],
    mandatory_docs: [],
    application_steps: [],
    where_to_apply: [],
    official_url: "",
    sources: [{ url: "", title: "", accessed_on: "" }],
    verification: { status: "NEEDS_REVIEW" },
  };
}

/**
 * Parses raw JSON typed in the editor. Returns an error string for invalid
 * JSON, non-objects or unknown top-level fields.
 */
export function parseRecordJson(text: string): { record?: SchemeRecord; error?: string } {
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch (e) {
    return { error: `Invalid JSON: ${(e as Error).message}` };
  }
  if (typeof v !== "object" || v === null || Array.isArray(v))
    return { error: "The record must be a JSON object." };
  const unk = unknownKeys(v as Record<string, unknown>);
  if (unk.length) return { error: `Unknown field(s) the server will refuse: ${unk.join(", ")}` };
  return { record: v as SchemeRecord };
}
