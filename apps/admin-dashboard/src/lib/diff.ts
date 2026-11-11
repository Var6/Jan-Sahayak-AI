// Structural diff of two JSON values (scheme records), as a flat list of
// changed leaf paths. Arrays are compared index by index, which is what a
// reviewer needs for ordered lists such as application steps.

export type DiffKind = "added" | "removed" | "changed";

export interface DiffEntry {
  path: string;
  kind: DiffKind;
  before?: unknown;
  after?: unknown;
}

const isObj = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

function join(path: string, key: string | number): string {
  if (typeof key === "number") return `${path}[${key}]`;
  return path ? `${path}.${key}` : key;
}

/** Bounded so a pathological record cannot freeze the page. */
export const MAX_DIFF_ENTRIES = 2000;

export function diffJson(a: unknown, b: unknown, opts: { ignore?: string[] } = {}): DiffEntry[] {
  const out: DiffEntry[] = [];
  const ignore = new Set(opts.ignore ?? []);
  const walk = (x: unknown, y: unknown, path: string) => {
    if (out.length >= MAX_DIFF_ENTRIES || ignore.has(path)) return;
    if (x === undefined && y === undefined) return;
    if (x === undefined) {
      out.push({ path, kind: "added", after: y });
      return;
    }
    if (y === undefined) {
      out.push({ path, kind: "removed", before: x });
      return;
    }
    if (Array.isArray(x) && Array.isArray(y)) {
      const n = Math.max(x.length, y.length);
      for (let i = 0; i < n; i++) walk(x[i], y[i], join(path, i));
      return;
    }
    if (isObj(x) && isObj(y)) {
      const keys = [...new Set([...Object.keys(x), ...Object.keys(y)])].sort();
      for (const k of keys) walk(x[k], y[k], join(path, k));
      return;
    }
    // null vs [] / {} and "" vs missing are real differences in the stored JSON.
    if (x !== y) out.push({ path, kind: "changed", before: x, after: y });
  };
  walk(a, b, "");
  return out;
}

export function showValue(v: unknown): string {
  if (v === undefined) return "";
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}
