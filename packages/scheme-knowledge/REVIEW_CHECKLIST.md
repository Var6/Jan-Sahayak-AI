# Knowledge record review checklist

Use this procedure for every scheme or service record before devices may
show it as fact. A citizen may act on what the kiosk says, for example by
travelling to a block office or paying an agent. A wrong amount or a
missing document costs that person real money and time. Check each fact.

## 1. Who may review

* The reviewer is a named person with the `scheme_reviewer` role in
  central-api. Janman staff or a trained paralegal who knows the Bihar
  department processes should fill this role.
* The reviewer must not be the person who saved the draft. Central-api
  enforces this: a reviewer cannot verify a version they edited.
* AI tools, scripts and import jobs never verify. The validator rejects
  reviewer names that contain "ai", "claude" or "llm". The reviewer
  account must belong to one human and must never be shared.
* If a record needs a legal reading, such as eligibility exclusions or
  appeal rights, ask the Janman legal team to look at it. Write their
  name in the review notes.

## 2. Before you start

1. Open the draft in the admin dashboard (`GET /v1/admin/schemes/{id}`).
   Read `verification.notes` first. It lists what the drafter could not
   confirm.
2. Open every URL in `sources`. If a URL is dead or redirects to a
   different document, find the current official document and replace the
   source. Never use a search snippet, a news article, a blog, a coaching
   site or a myScheme page as the source of a fact. You may read myScheme
   in your browser as a pointer to the official document.
3. Excerpts marked "decoded from legacy Kruti Dev font" or "text layer
   garbled" are the drafter's reading. Compare them with the PDF as
   displayed on screen, not with the copied text.

## 3. Field-by-field check

Mark each item ✔ (matches the source), ✎ (corrected) or ✗ (removed, not
supported).

| Field | What to compare | Source to use |
|---|---|---|
| `name` | The official scheme name. The Hindi name must match how the department writes it. | Department notification or portal |
| `department` | The department that implements the scheme now. Departments get renamed or reorganised. | Department website |
| `summary`, `eligibility` | Every condition: age, residence, BPL, category, exclusions. Remove any condition the source does not state. | Latest guideline or resolution (संकल्प) |
| `age_rule`, `income_rule` | Exact numbers and boundaries ("60 or above" and "above 60" are different rules). | Same |
| `benefits` | Amount, frequency, mode of payment. **Check the date of the source.** The amount must come from the latest revision. | Latest resolution or portal notice |
| `questions` | Each question follows from one eligibility condition. `ineligible_if` means "probably not eligible", never a final rejection. | — |
| `mandatory_docs`, `optional_docs`, `alternative_evidence` | Only documents the source lists. Keep the notes, such as "Aadhaar proves identity, not caste". | RTPS service table, guideline, application form |
| `application_steps`, `where_to_apply` | The office (RTPS counter, block, CDPO, DRCC, bank) and the portal. Confirm the portal currently accepts applications. | Portal, and a phone call to the office if needed |
| `official_url` | Opens and is on a `.gov.in` or `.nic.in` host. | — |
| `helpline` | Printed on an official page. Call it once to confirm it works. | Official page |
| `fees_note`, `exceptions` | As stated in the source. | — |
| `sources` | Each key fact has a source with a correct URL, title, `accessed_on` and a verbatim excerpt. | — |

Also check that:

* Hindi is plain and correct, and is understandable when read aloud on
  the kiosk. Use numbers written the way people say them.
* The record has no URL, phone number, amount or scheme name that
  is missing from the sources. The device's response validator trusts the
  record text.
* Hindi and English say the same thing.

## 4. Approve

1. If you corrected anything, an editor (not you) saves a new draft:
   `PUT /v1/admin/schemes/{id}` with `base_version` and a `change_note`.
   Central-api forces the status to `NEEDS_REVIEW`. In
   `verification.notes`, replace the drafter's notes with any remaining
   caveats.
2. Verify the reviewed version:
   `POST /v1/admin/schemes/{id}/verify` with
   `{"version": N, "review_due": "YYYY-MM-DD", "notes": "…"}`.
   Central-api records your user as `verified_by`, the time as
   `verified_at`, sets `last_verified`, and writes an audit entry.
3. In `notes`, write what you checked and how. For example: "Compared with SW
   Dept resolution no. … dated …, called block office … on …".
4. Publish a signed bundle (`POST /v1/admin/bundles`) when a batch of
   reviews is done.

## 5. Set `review_due`

Always set `review_due`. After that date, devices treat the record as
unverified (`Record.IsFactual`). This expiry is the only automatic
protection against stale facts.

| Record contains | `review_due` |
|---|---|
| Amounts, application windows, deadlines, session-specific rules (pensions, scholarships, Udyami, PMAY-G phase, SVANidhi limits) | **90 days** |
| Eligibility and document lists only (certificates, ration card, birth or death registration) | 180 days |
| Anything announced as "under revision", or with an open application window | the window's closing date, or 30 days |

Review sooner when any of these happens:

* The department issues a new resolution or guideline.
* The state or central budget is announced.
* A field team member reports that an office is asking for something
  different from the record.

## 6. Mark a record OUTDATED

A record is outdated when the scheme has closed or merged, when the amount
or eligibility changed and no corrected version has been verified yet, or
when the official source has been withdrawn.

Do this immediately:

1. The editor saves a draft with the corrected text, or with a clear
   "scheme closed / under revision" summary. In `verification.notes`,
   start with `OUTDATED:` and give the reason and date.
2. Review and verify the correction as in §4, or leave it as
   `NEEDS_REVIEW`.

> **Withdrawing a verified fact immediately:** a `scheme_reviewer` can call
> `POST /v1/admin/schemes/{id}/withdraw` (with a reason). This appends a new,
> immutable `OUTDATED` version, is audit-logged, and the next published
> bundle no longer carries the scheme's facts (devices fall back to "verified
> details are not available yet"). Use it as soon as a fact is known to be
> wrong, then verify a corrected version as in §4. `review_due` remains
> mandatory (§5) as the safety net for facts nobody re-checked.

Helplines (`helplines.json`) follow the same checks: the number, the name,
the purpose, an official source, and a test call. Record the sign-off in
the review sheet with your name and date. Central-api has no helpline
workflow yet.

## 7. Review sheet (copy per record)

```
Record id / version:
Reviewer (name, role):              Date:
Sources opened (URL + document date):
Corrections made (field → old → new):
Facts removed (and why):
Phone check of office/helpline (who, when, result):
review_due set to:
Remaining caveats for notes:
```
