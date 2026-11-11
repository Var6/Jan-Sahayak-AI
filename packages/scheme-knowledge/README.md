# scheme-knowledge

Draft knowledge base for Jan Sahayak: Bihar and central government schemes
and certificate services, plus the helpline list used by the kiosk's crisis
panel. **Everything here is `UNVERIFIED`.** Devices show a record as fact
only after a named human reviewer verifies it in central-api (see
`REVIEW_CHECKLIST.md`).

```
records/          one scheme.Record per file, named <id>.json
helplines.json    national helplines (schema below)
gazetteer/        district gazetteer (maintained separately)
REVIEW_CHECKLIST.md   human review procedure
```

Sources, reachability and licence notes are in `docs/KNOWLEDGE_SOURCES.md`.

## Record format

Each file in `records/` is one `scheme.Record` (`internal/scheme/scheme.go`).
Rules the tooling enforces on top of `Record.Validate`:

* The file name is `<id>.json`. Ids are unique, lowercase slugs.
* Strict JSON: unknown fields and trailing data are errors. Files are at
  most 256 KiB.
* `name.hi` is Devanagari. `department`, documents, eligibility, benefits,
  steps and `where_to_apply` entries have both `hi` and `en`.
* Every source is `https`, has a title, an `accessed_on` that is not in
  the future, and an excerpt of at most 1200 characters.
* Questions have snake_case ids, `kind` is `yes_no` or `choice`. For
  `yes_no`, `ineligible_if` holds only `"yes"` or `"no"`.
* Repository drafts are never `VERIFIED`. `last_verified`, `verified_by` and
  `verified_at` stay empty, and `verification.notes` must tell the reviewer
  what to check.
* `official_url` must be on a `.gov.in` / `.nic.in` host or be left empty.
* Every number in citizen-facing text (name, summary, eligibility,
  benefits, rules, questions, documents, steps, where to apply, fees,
  exceptions, helpline) must appear in at least one of the record's
  `sources[].excerpt`. `kb validate` warns; `kb validate -strict` fails.
  Digit grouping, Devanagari digits, phone-number spacing and spelled-out
  numbers in excerpts ("Rupees One Thousand", "चार लाख") are normalised.
  Host names are ignored. Run `-strict` before handing records to a
  reviewer.

Drafting rules (checked by reviewers, not by code):

* Facts (amounts, ages, income limits, documents, deadlines, phone numbers,
  URLs) come only from an official page that was actually fetched. Put a
  short verbatim excerpt in `sources[].excerpt`. If a fact cannot be
  confirmed, leave it out and say so in `verification.notes`.
* Some official Bihar PDFs use legacy Hindi fonts (Kruti Dev) or have a broken
  text layer. Excerpts taken from them are marked "decoded from legacy Kruti
  Dev font" or "text layer garbled; reading". The `hi` fields are
  plain-language translations written by the drafter, not quotes.
* `aliases` and `keywords` are speech-matching aids (Hinglish, common
  misspellings, abbreviations). They are not facts.
* Mark facts that change often (amounts, application windows) in the notes.

## Tooling

```sh
go run ./cmd/kb validate -strict packages/scheme-knowledge/records
go run ./cmd/kb helplines packages/scheme-knowledge/helplines.json
go run ./cmd/kb bundle packages/scheme-knowledge/records -out bundle.json [-version 2026-11-09.1]
go test ./internal/scheme/ ./cmd/kb/
```

`kb bundle` writes an **unsigned** `scheme.Bundle` with records sorted by id.
Use it to import or inspect records. Bundles for devices are built and
signed only by central-api (`POST /v1/admin/bundles`), which includes only
`VERIFIED` records and current `NEEDS_REVIEW` drafts.
`-require-verified` makes the command fail if any record is not `VERIFIED`.
`-allow-verified` accepts `VERIFIED` records when you check an export taken
from central-api.

## helplines.json schema (v1)

```jsonc
{
  "schema_version": 1,                       // must be 1
  "helplines": [
    {
      "id": "tele-manas-14416",              // lowercase slug, unique
      "number": "14416",                     // digits, spaces or hyphens only, 3–20 chars
      "alt_numbers": ["1800-89-14416"],      // optional; no number may appear twice in the file
      "name":    { "hi": "…", "en": "…" },   // hi must be Devanagari
      "purpose": { "hi": "…", "en": "…" },
      "use_for": ["crisis", "mental_health"],// from: crisis emergency police fire ambulance women
                                             //   domestic_violence child mental_health suicide
                                             //   legal_aid general_help scheme
      "scope": "national",                   // "national" | "BR"
      "source_url": "https://…gov.in/…",     // https, .gov.in or .nic.in host
      "source_title": "…",
      "source_excerpt": "… 14416 …",         // verbatim; must contain `number`
      "accessed_on": "2026-11-09",           // YYYY-MM-DD, not in the future
      "status": "UNVERIFIED",                // UNVERIFIED | NEEDS_REVIEW | OUTDATED (never VERIFIED in this file)
      "notes": "what the reviewer must check"
    }
  ]
}
```

Helplines included: 112 (ERSS), 181 (women), 1098 (child), 14416 /
1800-89-14416 (Tele-MANAS), 15100 (NALSA legal aid) and 1906 (LPG
emergency). PM-KISAN and PM-JAY helplines are **not** included because no
official page we fetched printed them. Scheme-specific helplines that were
printed on an official page are in the records themselves (Bihar Yuva
portal 18003456444 and Ujjwala 14438).

Central-api has no review workflow for helplines yet. Until it does, a
reviewer approves helplines through the same sign-off sheet as records
(REVIEW_CHECKLIST.md §6), and an engineer copies the result into the device
configuration.
