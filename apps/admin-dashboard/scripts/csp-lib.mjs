import { createHash } from "node:crypto";

// Inline script = <script ...> without a src attribute. Script element
// content is raw text in HTML (no entity decoding), so the hash is over the
// exact characters between the tags, as the browser computes it.
const SCRIPT_RE = /<script\b([^>]*)>([\s\S]*?)<\/script\s*>/gi;
const SRC_RE = /(?:^|\s)src\s*=/i;
const NON_JS_TYPE_RE = /(?:^|\s)type\s*=\s*["']?(application\/(?:ld\+)?json)/i;

export function inlineScriptHashes(html) {
  const hashes = new Set();
  for (const m of html.matchAll(SCRIPT_RE)) {
    const attrs = m[1] ?? "";
    if (SRC_RE.test(attrs) || NON_JS_TYPE_RE.test(attrs)) continue;
    const body = m[2] ?? "";
    hashes.add(`sha256-${createHash("sha256").update(body, "utf8").digest("base64")}`);
  }
  return [...hashes].sort();
}
