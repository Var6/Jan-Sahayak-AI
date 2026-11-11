#!/usr/bin/env node
// Writes <out>/csp-hashes.json: for every exported HTML page, the CSP
// sha256 hashes of its inline <script> elements.
//
// Next.js static export puts the React Server Components payload and the
// bootstrap in inline scripts (self.__next_f.push(...)). central-api serves
// the dashboard with `script-src 'self'` and reads this file at startup to
// add exactly these hashes, per page, to that page's CSP. No 'unsafe-inline',
// no nonces (a static file server cannot mint them). See
// docs/API_DOCUMENTATION.md, "Admin dashboard (static UI)".
//
// Usage: node scripts/csp-hashes.mjs out

import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { inlineScriptHashes } from "./csp-lib.mjs";

const outDir = process.argv[2];
if (!outDir) {
  console.error("usage: csp-hashes.mjs <export-dir>");
  process.exit(2);
}

// Limits mirrored by central-api (static_ui.go); fail the build, not the server.
const MAX_FILES = 2000;
const MAX_HASHES_PER_FILE = 256;

function* htmlFiles(dir) {
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) yield* htmlFiles(p);
    else if (e.isFile() && e.name.endsWith(".html")) yield p;
  }
}

const files = {};
let count = 0;
let total = 0;
for (const f of htmlFiles(outDir)) {
  if (++count > MAX_FILES) {
    console.error(`csp-hashes: more than ${MAX_FILES} HTML files`);
    process.exit(1);
  }
  const rel = relative(outDir, f).split(sep).join("/");
  const hashes = inlineScriptHashes(readFileSync(f, "utf8"));
  if (hashes.length > MAX_HASHES_PER_FILE) {
    console.error(
      `csp-hashes: ${rel} has ${hashes.length} inline scripts (max ${MAX_HASHES_PER_FILE})`,
    );
    process.exit(1);
  }
  files[rel] = hashes;
  total += hashes.length;
}
if (count === 0) {
  console.error(`csp-hashes: no HTML files in ${outDir}`);
  process.exit(1);
}
writeFileSync(
  join(outDir, "csp-hashes.json"),
  `${JSON.stringify({ version: 1, files }, null, 1)}\n`,
);
process.stdout.write(
  `csp-hashes: ${count} pages, ${total} inline script hashes → ${join(outDir, "csp-hashes.json")}\n`,
);
