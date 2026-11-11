# Knowledge Sources

Last updated 2026-11-09. This map covers the official sources used for the
draft Bihar knowledge base (`packages/scheme-knowledge/`), whether they
could be reached, and the reuse terms each site sets. Read it together
with `packages/scheme-knowledge/REVIEW_CHECKLIST.md`.

## How the sources were collected

* Each source was fetched once or a few times, by hand, from the dev
  machine with a browser user agent. Requests were spaced out, nothing was
  crawled in bulk, and no site was scraped. The robots.txt files that
  exist allow access: udyami.bihar.gov.in serves `Allow: /`, and
  serviceonline.bihar.gov.in and 7nishchay-yuvaupmission.bihar.gov.in
  return 404 for robots.txt.
* **myScheme was not used** (myscheme.gov.in, api.myscheme.gov.in). The
  myScheme Terms of Use prohibit bots, scrapers and automated copying
  without written permission from NeGD. Staff may open myScheme pages in a
  browser as a pointer to the official document. A myScheme page is never
  a `source` for a record.
* Third-party sites (news, Vikaspedia, coaching and "sarkari yojana"
  blogs, insurance and fintech sites) were used only to find where an
  official document lives. No fact in the records comes from them.
* **Re-fetch for review findings (2026-11-09).** After the independent
  correctness review, these official documents were fetched once more (one
  request each, by hand, with timeouts and a size cap) to quote the exact
  sentence behind facts that had no excerpt: PM-KISAN Operational
  Guidelines, PMAY-G Framework (2022), RBI KCC Directions 2026, RTPS GAD and
  SWD tables, RTPS Quick Reference, CRS FAQ, pmuy.gov.in `about.html`,
  pmsonline.bihar.gov.in, the 7nishchay FAQ page and the Udyami FAQ (Kruti
  Dev, decoded by hand). nha.gov.in/PM-JAY (still a JavaScript shell) and
  pmjay.gov.in (still timing out) could not be read, so
  `ayushman-bharat-pmjay` now has an empty `official_url`. myScheme was not
  used.
* **Number check.** `go run ./cmd/kb validate -strict
  packages/scheme-knowledge/records` fails if any number in citizen-facing
  text (amounts, ages, days, phone numbers) appears in none of the record's
  source excerpts. Without `-strict` it only warns. Passing it is necessary,
  not sufficient: a number can match an unrelated number in the same
  excerpt, and number words in citizen-facing text are not checked. The
  reviewer still reads every excerpt.

## Source map

Reachability was checked on 2026-11-09 from the dev network in India.
"Reachable" means plain HTTPS without JavaScript worked.

### Bihar state

| Source | Used for | Reachable | Notes |
|---|---|---|---|
| serviceonline.bihar.gov.in (RTPS / ServicePlus) — `Bihar_RTPS_GAD_Services.pdf`, `Bihar_RTPS_SWD_Services.pdf`, `GAD_points.pdf`, `Bihar_RTPS_Quick_Reference.pdf`, home-page service list | Caste, income and residence certificates; pension eligibility tables; NFBS; Kanya Utthan (0–2); birth and death issuance; ration card application | Yes | PDFs are undated. The English tables extract cleanly; the Hindi text layer is garbled (broken font mapping). The RTPS SWD table predates MVPY and may be old. |
| betastate.bihar.gov.in (state portal managed by IPRD) — `/pension`, Social Welfare Dept pages, scheme notes uploaded in April 2024 | MVPY (₹1100 from June 2025), Laxmibai, disability pension, IGNOAPS, NFBS, Kabir Antyeshti, Kanya Utthan | Yes | The scheme-note PDFs have a garbled Unicode text layer, so drafters read them by eye. Several April 2024 notes show pension amounts of ₹400 that are now superseded. state.bihar.gov.in redirects in loops unless cookies are kept. |
| 7nishchay-yuvaupmission.bihar.gov.in — home, `/faq`, `New_Guideline_Final_Update1.pdf`, `Process_Flow_for_BSCC.pdf` | Student Credit Card, Swayam Sahayata Bhatta, helpline 18003456444 | Yes | The guidelines are in the legacy Kruti Dev font (decoded by hand). `Letter_No-355_Dated-17.10.2025.pdf` and the SHA graduation notice are image-only and were not read. |
| udyami.bihar.gov.in — `pdf/FAQ.pdf`, home notices | Mukhyamantri Udyami Yojana | Yes | The FAQ is in Kruti Dev. The 2025-26 guideline (Annexure-II) is image-only. |
| pmsonline.bihar.gov.in | BC/EBC post-matric scholarship | Yes | Rules are tied to each session. The SC/ST portal (scstpmsonline.bihar.gov.in) was not opened. |
| epds.bihar.gov.in, betastate.bihar.gov.in/fcp | Ration card (Smart PDS 2.0, status) | Yes | The Food Department's RTPS PDF uses a legacy font and could not be read. rconline.bihar.gov.in was not opened. |
| sspmis.bihar.gov.in | MVPY online application | **No** (timeout) | Retry from another network. |
| medhasoft.bih.nic.in | Kanya Utthan (intermediate and graduation parts) | **No** (timeout) | This gap is why `bihar-kanya-utthan` covers only the 0–2 part. |

### Central

| Source | Used for | Reachable | Notes |
|---|---|---|---|
| pmkisan.gov.in — Operational Guidelines, OG amendment, FAQ, home | PM-KISAN | Yes | The guideline PDFs are the 2019 originals (2-hectare text). No helpline number appears on the Contacts page. |
| pmayg.dord.gov.in — Framework for Implementation (2022), home | PMAY-G | Yes | Criteria for the new phase (Awaas+ 2024) were not fetched. |
| pmindia.gov.in — cabinet release, Sep 2024 | PM-JAY (₹5 lakh; all citizens 70+) | Yes | |
| nha.gov.in/PM-JAY, pmjay.gov.in | PM-JAY | nha: single-page app, no text; pmjay: **No** (re-checked 2026-11-09, same) | Helpline 14555 was not confirmed on an official page. Never read, so not used as `official_url`. |
| mudra.org.in — `/FAQ`, `/Offerings` | PM Mudra | Yes | Not a .gov.in host, so `official_url` is empty. |
| pmsvanidhi.mohua.gov.in — home, `FAQEng.pdf` | PM SVANidhi | Yes | The FAQ is the 2020 version. The portal mentions "enhanced limits" but does not give the figures. |
| rbi.org.in — KCC Directions 2026 (commercial banks) | Kisan Credit Card | Yes | The directions apply to loans from 01-01-2027. The rules in force now were not fetched. |
| pmuy.gov.in — `about.html` | PM Ujjwala, helplines 14438 and 1906 | Yes | |
| nfsa.gov.in — `/portal/NFSA-Act` | Ration entitlements (35 kg AAY, 5 kg per person PHH) | Yes | |
| dc.crsorgi.gov.in — `FAQ_of_CRS_Latest.pdf` | Birth and death registration | Yes (PDF) | The portal itself is a single-page app. |
| wcd.gov.in (home) | Helplines 181, 1098 | Yes | |
| dghs.mohfw.gov.in, static.pib.gov.in | Tele-MANAS 14416, ERSS 112, 181 description | Yes | 112.gov.in and telemanas.mohfw.gov.in did not respond or need JavaScript. |
| nalsa.gov.in | Helpline 15100 | Yes | |

## Licence and terms

There is **no general open licence** for scheme text on government
websites.

* **GODL-India (Government Open Data License) covers only datasets
  published on data.gov.in.** It does not cover department websites,
  guideline PDFs or portal text.
* Reproducing official facts with attribution is how we use these sources:
  a short excerpt, a link, and our own wording of the facts. Even so,
  several sites ask for permission before their material is reproduced.

| Site | Terms (copyright policy, read on 2026-11-09) | Action |
|---|---|---|
| pmkisan.gov.in | "may be reproduced free of charge after taking proper permission by sending a mail to us" | Ask for permission by email |
| nfsa.gov.in | Same GIGW wording: permission by mail | Ask for permission by email |
| nalsa.gov.in | Same: "after taking proper permission by sending a mail to us" | Ask for permission by email |
| pmayg.dord.gov.in | "may not be reproduced partially or fully without prior written permission from the Ministry of Rural Development" | **Written permission needed** |
| betastate.bihar.gov.in (IPRD) | "may be reproduced free of charge" with accurate reproduction and acknowledgement; no framing | Acknowledge the source; no permission needed |
| udyami.bihar.gov.in | Has a terms-and-conditions page (not read) | Read the terms; ask the Industries Dept |
| serviceonline.bihar.gov.in, 7nishchay-yuvaupmission, pmsonline, epds, pmuy, pmsvanidhi, crsorgi, wcd, dghs | No copyright policy found on the pages fetched. GIGW sites usually use the "permission by mail" wording. | Ask the departments as part of outreach |
| mudra.org.in, rbi.org.in | Not read | Ask before reproducing more than short excerpts |
| myscheme.gov.in (NeGD, MeitY) | Terms prohibit bots, scrapers and automated copying without written permission | **Do not automate.** Ask NeGD in writing if API or data access is wanted |

All sites state that their content is not a statement of law. The kiosk
always shows the official link and tells citizens to confirm at the office.

## Recommended outreach for Janman

Ask each office for written permission to reproduce scheme facts with
attribution. Where possible, also ask for a named contact who will tell us
when amounts, windows or forms change.

1. **Bihar Social Welfare Department** (Directorate of Social Welfare;
   Social Security Cell / SSPMIS). Ask for current pension amounts for
   all six pensions, the MVPY, Laxmibai and disability document lists, a
   working SSPMIS URL, and the Kabir Antyeshti procedure.
2. **Bihar Administrative Reforms Mission Society / GAD (RTPS)**. Ask for
   current RTPS service tables in Unicode, the caste, income and
   residence forms, and fees.
3. **Bihar Education Department**. Ask for the Kanya Utthan (Medhasoft)
   guideline, the post-matric BC/EBC rules, and the BSCC revised guideline
   with Letter 355 in Unicode or text form.
4. **Bihar Planning & Development / Labour Resources (MNSSBY, KYP)** and
   the **Industries Department (Udyami)**. Ask for the current guidelines
   in Unicode.
5. **Bihar Food & Consumer Protection Department**. Ask for ration card
   eligibility and documents, and the Smart PDS 2.0 / rconline process.
6. **Ministry of Rural Development (PMAY-G)**. Written permission is
   required. Ask for the criteria for the 2024–29 phase.
7. **Department of Agriculture & Farmers Welfare (PM-KISAN)**. Ask for
   the current Operational Guidelines and the official helpline numbers.
8. **National Health Authority / Bihar State Health Society (PM-JAY)**.
   Ask how eligibility is decided in Bihar, the documents needed for the
   card, and the official helpline.
9. **NeGD (myScheme)**. Only if Janman wants structured data. Ask for
   written permission or a data-sharing agreement.
10. **NALSA / Bihar State Legal Services Authority**. Ask for permission
    for helpline use and for referral contacts in each district.

Keep the replies with the review sheets. Record each permission and its
date in this file.
