# AI Guardrails — Conversation Engine

Last updated 2026-11-09 (review fixes, workstream A). Code:
`services/device-agent/internal/dialogue`, `internal/knowledge`,
`internal/textnorm`, `internal/scheme`. Tests: the `*_test.go` files in those
packages (coverage map in section 7).

The kiosk is **deterministic first**. Rules, a bounded state machine and
human-verified knowledge records produce every fact. The local LLM (Gemma 4
E2B on llama-server) is optional. It may only (a) pick a label from a fixed
list when the rules cannot decide, and (b) rephrase text we already composed
from a verified record. A validator checks its output, and any failure falls
back to the template text. If the LLM is down, the kiosk works the same way,
just without the rephrasing.

## 1. Order of processing for one turn

```
option tap ──► option must be on screen (or a global/topic option) ──► state machine
voice/text ──► no speech / empty transcript ──► repeat / escalate
           ──► CRISIS detection on the FULL raw text (format chars removed, no length
               cap, before the confidence gate) ──► crisis reply, flow reset
           ──► STT confidence < 0.45 ──► repeat / escalate (+112 if distress words seen)
           ──► sanitise: remove control & format chars, cap 500 runes
           ──► sensitive-data detection + redaction ──► warning (value never echoed/stored)
           ──► prompt-injection detection ──► polite redirect (never reaches the LLM)
           ──► understand: sub-answer → option match → follow-up → scheme name
                           → deterministic intent → knowledge search → LLM label → clarify
           ──► compose from record (template) ──► optional LLM rephrase ──► validator
           ──► bounded history (redacted) + anonymous analytics
```

## 2. The eight layers

| # | Layer | What it does | Where |
|---|---|---|---|
| 1 | **Topic classifier** | Text is folded (`textnorm.Fold`: no nukta, ड़→ड, ँ→ं, ी→ि, ू→ु, ण→न, श/ष→स, nasal+virama→ं, geminates, ृ→्रि, digits → ASCII). It is then matched against a concept lexicon of Hindi, Hinglish and English words, exactly or by edit distance (0 edits for ≤3 runes, 1 for 4–6, 2 for 7–12; **romanised words of ≤4 letters are always exact**, so "pati" (husband) never becomes "jati" (caste)). Before matching, `Fold` also canonicalises look-alikes: fullwidth → ASCII, zero-width/format characters removed, Cyrillic/Greek look-alikes → Latin. Phrase matching handles STT errors such as `पीम किसान`, and the context rule `राशन कोड/कोर्ट` → card. Weighted scores pick one of 12 welfare intents, 8 commands or 10 disallowed topics. Bribery/weapons (`illegal`) and voting questions always win over a welfare topic in the same sentence ("लोन के लिए कितनी रिश्वत", "पेंशन वाली पार्टी को वोट"), except "वोटर कार्ड". "again"/"फिर से" is the REPEAT command only when it is (nearly) the whole utterance. The LLM is asked only when the rules are ambiguous or know nothing, and only for a label from a fixed enum (JSON-schema constrained, temperature 0, max 24 tokens, 4 s timeout, 500 ms health check first). Without an LLM, the kiosk asks a clarifying question with options. | `classify.go`, `textnorm/*`, `llm.go:llmClassify` |
| 2 | **State machine** | `State.Flow/Step/Slots`. Steps: sub-question (loan purpose, pension type, certificate type) → candidate pick (≤4) → eligibility questions from `Record.Questions` (≤3, yes/no or ≤5 choices) → answer card → "क्या आपको और मदद चाहिए?". Every reply offers "repeat" and "end". Slots hold only short enum values (≤16 slots, ≤128 runes each). History holds only redacted text (≤8 turns, ≤200/300 runes per entry). Everything lives in RAM and is dropped when the session ends. | `flows.go`, `engine.go` |
| 3 | **Verified retrieval** | Records are loaded from JSON into SQLite `schemes` + `schemes_fts`. One transaction replaces the whole set atomically. Each record must pass `Record.Validate()`; files are strict-decoded, so unknown fields are rejected and typos are caught. `bundle_version` is recorded in `device_meta`. Only `IsFactual(now)` records (VERIFIED by a named human and not past `review_due`) are shown as fact. Any other record yields **only its name** and a generic message ("verified details are not available yet — confirm at your nearest RTPS counter / CSC / block office"). That message never names an office from the record. `AllowUnverifiedForDev` shows unverified content under a "NOT verified" disclaimer, but only on `dev-*` device profiles; on every other profile it is ignored. If no record matches, the reply is "no verified information — ask at the office". The engine never invents an answer. | `knowledge/*` |
| 4 | **Tool permissions = none** | The LLM has no tools, functions, file access, network or memory. It receives only the messages we build and returns text. It cannot submit, store or look anything up. | `ai/llama.go`, `llm.go` |
| 5 | **Response validator** | Runs on rephrased text. **It is an ALLOWLIST** (decision 5): every folded token of the output must already occur in the template *source* text (not the whole record), apart from a short list of postpositions, pronouns and copulas (`rephraseFunctionWords`; negation, "only", quantifiers, modality and number words are deliberately not in it). So a rephrase can only drop words, reorder and add function words; numbers, number words, dates, names, documents and offices pass only if the source already has them. Before the allowlist, specific checks give informative reasons. It rejects the output if any of these is true: empty; longer than 1.5×source+60 runes or 600 runes; contains a URL or domain not in the source; any digit sequence (ASCII or Devanagari) not in the source; any number word (पाँच, हज़ार, लाख, five…) not in the source; any known scheme name other than the record under discussion; any "<word> योजना/scheme/मिशन" not in the source; any document word (पत्र, कार्ड, प्रमाण, दस्तावेज़, आधार, पैन…) or its qualifier not in the source; less than 70% Devanagari; any guarantee, eligibility verdict or submission claim not present in the source — the second-layer blocklist (ज़रूर मिलेगा, पक्की, बिल्कुल मिल…, गारंटी, पात्र हैं/नहीं, मंज़ूर/स्वीकृत, खारिज, भेज दिया, सबमिट, जमा कर दिया, approved, eligible, submitted…); prompt or role leaks (<<<, `system`, निर्देश, "as an AI", "I am an official"); finally any token outside the allowlist (`new_word`). On rejection or LLM error, the template text is used. `Reply.UsedLLM` is true only when validated LLM text is in the reply. | `llm.go:validateRephrase` |
| 6 | **Prompt-injection protection** | Regexes plus folded Hindi phrases detect "ignore previous instructions", "system prompt", "you are now", act as / pretend / role-play, jailbreak / DAN, "reveal your rules", chat-template tokens and code fences, `system:` prefixes, role-play and game framings ("let's play a game", "imagine you are", "you are the … now"), and their Hindi and Hinglish forms (पिछले निर्देश भूल जाओ, निर्देश छोड़ दो, नियमों को मत मानो, … की तरह जवाब दो, मान लो तुम … हो, अपने नियम बताओ, अब से तुम … हो, kalpana karo ki tum … ho, pichle nirdesh bhool jao …). Matching runs after look-alike folding (`textnorm.Canon`/`Fold`: fullwidth, zero-width, Cyrillic/Greek), and sanitising removes format characters instead of turning them into spaces, so "ig\u200bnore", "ｉｇｎｏｒｅ" and "іgnore" are caught. The input is answered with a polite redirect and is **never sent to the LLM**. Other text reaches the LLM only for classification, as quoted data: delimiters are stripped, it is redacted, capped at 300 runes, and passed inside `<<< >>>` under a fixed system prompt that says it is untrusted. The rephrase step never sees citizen text. | `guard.go`, `llm.go` |
| 7 | **Uncertainty handling** | Low STT confidence (< `MinSTTConfidence`, default 0.45), no speech, or empty input → "कृपया दोबारा बोलिए" — but crisis detection has already run on the transcript, so crisis words at any confidence get the crisis reply. If a low-confidence transcript contained weak distress words (मदद, बचा…, मार…, खून, पुलिस, help …), every following repeat/escalation prompt also says "अगर आप खतरे में हैं, तो तुरंत 112 पर कॉल करें" until a turn is understood (`State.Distress`, no text stored). A scheme the citizen *names* but we have no record of ("पीएम किसान" with no such record, "मुख्यमंत्री … योजना") → "इस योजना की सत्यापित जानकारी मेरे पास नहीं है" first, then related options; an answer later in that flow repeats that it is about a different scheme. Input that is not understood, or a tapped option that was not offered → a clarifying question with options. Weak signals (e.g. only "आय") → "which topic?". Two topics with close scores → a choice between them. Eligibility answers in `ineligible_if` → "you *may* not meet the conditions — the office decides". The kiosk never rejects outright. | `engine.go:miss/clarify`, `flows.go` |
| 8 | **Human escalation** | After 3 consecutive misses (`MaxMisses`) → `KindEscalate` with "type instead" and "help from a person". "किसी कर्मचारी/इंसान से बात" → escalate at once. A technical failure or missing verified information → refer to the RTPS counter / CSC / block office. | `engine.go` |

The engine always says what it is. The spoken greeting is ≤ ~10 s (≈25 words):
"a computer assistant, not a government official" and "do not tell me your
Aadhaar, OTP or PIN". The screen text adds the details (does not submit
applications, what it can help with, speak or tap). Every factual card
also carries "does not submit applications, cannot guarantee approval —
the office decides".

## 3. Crisis protocol

* Detection runs **first**, on the **full raw input** (control/format
  characters removed, no length cap), **before** the STT-confidence gate
  and every other content check. Types, in priority order: self-harm,
  child (abuse, child marriage, child labour), trafficking / bonded labour,
  violence (incl. domestic violence and dowry threats), **medical emergency**
  (बेहोश, साँस नहीं, साँप ने काटा, दिल का दौरा … → call 112 / ambulance),
  immediate danger.
* The vocabulary is a **data table** in `crisis.go` (`crisisPhrases`,
  `crisisPairs`, age rule) that **must be reviewed and signed off by a
  trained counsellor / psychosocial reviewer** before field deployment and
  after every change (pending, see section 8). Three mechanisms:
  1. phrases matched at a word start on folded text, the last word as a
     prefix ("जान दे दू" → दूंगा/दूंगी);
  2. co-occurrence within a token window: a person/relative/pronoun
     (पति, ससुराल, सास, husband, in-laws, मुझे, me …) + a violence-verb form
     (मार/पीट/जला/धमकी…, maarta/peet…, hit/beat/burn/kick…); दहेज + threat;
     poison/pesticide (कीटनाशक, pesticide) + drink/eat (पी, खा, pi, drink …);
     marriage + "minor" (नाबालिग, कम उम्र);
  3. child marriage by age: a marriage word near "<age < 18> साल/वर्ष/year"
     (digits or number words).
  Verb forms are listed explicitly (no prefix wildcards) so "मार्कशीट",
  "मर गए" (died), "भूख के मारे" (because of hunger), "heart attack" (as
  violence) and "पैसा डूब गया" do not match. Deliberate welfare exclusions:
  दुर्घटना बीमा, "ज़मीन बेच दी", "कीटनाशक पर सब्सिडी", "पति की मृत्यु".
* Reply `kind=crisis`, with empathetic wording by crisis type. **112** is
  always shown. Up to 3 helplines whose `use_for` tags map to the crisis
  type come from the verified helpline file. The welfare flow is abandoned in
  the same turn: no cards, state reset. History stores only `[crisis]`.
  Analytics: `intent=crisis, resolution=crisis`.
* **112** is India's single national emergency number (Emergency Response
  Support System, Ministry of Home Affairs). It is the only phone number
  written in code (`dialogue.EmergencyNumber`, also in the medical
  off-topic line). **Documented exception** to "facts only from verified
  records" (decision 10): it stays hard-coded as the emergency fallback so a
  crisis reply works with no helpline file. Needs human sign-off (section 8).
* All other helplines come from `packages/scheme-knowledge/helplines.json`
  (shape: `{"schema_version":1,"helplines":[{id, number, name{hi,en},
  use_for[], status, …}]}`). Verification fields may be flat or under
  `verification`. An entry is used only if `status` is `VERIFIED` **and**
  `verified_by` is a human reviewer (`scheme.ValidReviewer`), `verified_at`
  parses, `review_due` parses as YYYY-MM-DD, and its number is well formed.
  **Expiry is re-evaluated at every crisis reply** (`Helplines.ForAt`), not
  only at start-up. Tags map to crisis types through an explicit table
  (`helplineTagCrisis`): suicide/mental_health → self-harm;
  domestic_violence/women → violence (women also trafficking); child →
  child/trafficking; police → violence/trafficking/danger; ambulance →
  medical; fire → danger. **Not mapped on purpose:** `crisis` (too generic:
  112, 181, 1098, 14416 all carry it), `emergency` (the LPG gas-leak line
  1906 carries it and must never be offered for a personal crisis),
  `legal_aid`, `general_help`, `scheme`. A missing or invalid file degrades
  to 112 only.

## 4. Sensitive-data rules

* Detected: 12–19 digit runs (Aadhaar, card numbers); 9–11 digit runs
  (account numbers); 10-digit mobile numbers starting with 6–9; IFSC codes
  and PAN (masked whole, letters included); OTP/PIN/password/CVV keywords
  followed by a value ("is/है/:" value) **or with the value before है/hai**
  ("मेरा पासवर्ड गंगाजल है", "mera upi pin ganga hai"); account or Aadhaar
  keywords followed by ≥4 digits. Devanagari and fullwidth digits are
  handled, and **runs of ≥4 spoken digit words** (एक दो तीन…, ek do teen…,
  one two three…) are turned into digits first, so STT output that spells
  digits out is caught.
* `dialogue.RedactSensitive(text) string` exposes the same redaction for
  the API, which must redact STT transcripts before returning or showing
  them (guardrails review finding 5; wiring is workstream C).
* Every run of 3 or more digits is replaced with `[#]` **before** anything
  else sees the text: classifier, knowledge search, LLM, history.
  Values are never echoed, never stored in `State` and never put in analytics.
* The reply warns the citizen not to share such data. If the rest of the
  turn clearly asks about a topic ("…, पेंशन चाहिए"), the flow continues and
  the warning is shown at the top. Otherwise the reply is only the warning.
* Analytics (`Reply.Analytics`) carries only enum values: intent, category,
  record id, resolution. Tests check every reply against `^[a-z0-9_-]*$`.
* The engine logs no citizen text. It logs only validator rejection reasons
  and errors.

## 5. What the LLM can and cannot do

| Can | Cannot |
|---|---|
| Return one label from the fixed enum for an ambiguous message | Choose a scheme, answer a question, or produce facts |
| Rephrase a template summary (scheme name + record summary) into simpler Hindi | See citizen text during rephrase, see records beyond the one being shown, or see system internals beyond the fixed prompts |
| Be unavailable or slow: the kiosk falls back to deterministic text (circuit breaker in `ai`) | Call tools, browse, store, submit, approve, or mark anything VERIFIED |
| | Change cards: cards are always built only from record fields |

Rephrasing is turned off for name-only (unverified) answers and for English;
the validator is Devanagari-only. Per decision 5, rephrasing is
**disabled by default on the pi5 profile** until field-validated
(`llm.rephrase=false`, `internal/config/config.go` and `device.pi5.json`);
classification stays enabled.

Verification identity (`internal/scheme`): `verified_by` must be the central
reviewer's user id (UUID), `reviewer:<id>`, or a person's name. It is refused
if any whole word names a tool or model (gpt, llm, claude, gemma, copilot,
bot, model, machine, auto, ai, …; words split at letter/digit boundaries, so
"Sairam"/"Vaishali" pass and "GPT-4o" fails). `review_due` must be
YYYY-MM-DD; an unparseable one fails `Validate` and makes `IsFactual` false.
The device refuses a signed bundle whose `bundle_version` (YYYY-MM-DD.N) is
lower than the one loaded (`knowledge.ErrBundleRollback`).

## 6. Knowledge-author notes (affects matching)

* Put everyday topic words in `keywords`, in Hindi, Hinglish and English
  (e.g. "वृद्धावस्था पेंशन", "old age pension", "vridha pension"). For loan,
  pension and certificate flows, a record is offered only if its names,
  aliases or keywords contain both the flow word (loan/pension/certificate)
  and the sub-type word (farming/business/…, elderly/widow/disability,
  caste/income/…).
* Put specific spoken names in `aliases` ("पीएम किसान"). Aliases made only
  of generic words ("वृद्धावस्था पेंशन") do not count as "the citizen named
  this scheme".
* Unknown JSON fields cause the record file to be skipped and reported.

## 7. Test coverage map

| Guarantee | Test |
|---|---|
| Folding rules, STT spellings (मुझी लोन, जाती प्रमान पत्र, व्रिधावस्था पेशन, पीम किसान, कार्ड→कोड/कोर्ट/कॉर्ड, खगरिया) | `textnorm: TestFoldRules, TestFoldEquivalences, TestConceptsFromSTTAndHinglish, TestVocabFuzzyPhrases`; `knowledge: TestGazetteer, TestSearchRanksGarbledQueries` |
| Load validates, skips and reports (bad id, AI verifier, unknown field, broken JSON, duplicate); atomic replace; refusal to wipe; bundle file + verify hook; index rebuilt on reopen; concurrent reloads | `knowledge: TestLoadDir…, TestInvalidRecordsSkippedAndReported, TestReplaceIsAtomic…, TestLoadBundleFile, TestReopenRebuildsIndexes, TestConcurrentSearchDuringReload` |
| Presentation policy, including dev-override profile gating and overdue review | `knowledge: TestPresentationPolicy`; `dialogue: TestDevOverrideShowsDisclaimerOnlyOnDevProfile, TestNonFactualRecordsShowOnlyNameAndGenericMessage` |
| Welfare routing (the 4 clip texts, garbled forms, Hinglish, English, scheme by name) | `dialogue: TestWelfareQuestionsRoute` |
| Multi-turn flows, eligibility questions, ineligible note, amount and document follow-ups, Aadhaar≠caste rule | `TestLoanFlowMultiTurn, TestPensionEligibilityAndFollowUps, TestCasteCertificateAadhaarRule` |
| Facts not in any record → "verification required", no digits | `TestFactsNotInAnyRecordAreNeverInvented` |
| Incomplete information → clarifying question | `TestIncompleteInfoAsksClarifyingQuestion` |
| Off-topic redirects (9 categories) | `TestUnrelatedQuestionsRedirect` |
| Prompt injection (17 forms, including Hindi and Hinglish); LLM never called | `TestPromptInjectionNeverReachesLLM` |
| Crisis first, verified helplines only, flow reset, 112 fallback with no file | `TestCrisisFirstWithVerifiedHelplines, TestCrisisDegradesTo112WithoutHelplineFile` |
| Sensitive data never echoed, stored or sent to the LLM | `TestSensitiveDataNeverEchoedOrStored` |
| Low STT confidence → repeat; 3 misses → escalate | `TestLowConfidenceAndEscalation` |
| Validator rejects hallucinated numbers, number words, phones, URLs, schemes, documents, guarantees, English, leaks | `TestValidatorTable, TestRephraseAcceptedAndRejected` |
| LLM down or unhealthy → deterministic path; LLM not used when rules are confident | `TestRephraseAcceptedAndRejected, TestLLMClassifierForAmbiguousInput, TestLLMNotUsedWhenDeterministicIsConfident` |
| State bounds, long and control-character input, concurrent sessions | `TestStateIsBounded, TestConcurrentSessions` |
| Invariants on every reply (no guarantee or submission claims, no URL in speech, repeat/end offered, analytics enum-only, verified card has disclaimer) | `helpers_test.go: checkInvariants` (runs on every `say`/`tap`) |
| Real model: classifier accuracy and latency, validator accept/reject | `TestIntegrationRealLLM` (opt-in, `JS_LLM_URL`) |
| Review regressions (crisis phrasings, low confidence, beyond cap, sensitive data, injection, validator V1–V7, verified_by, named-unknown scheme, off-scope, shipped helpline schema, fuzzy misroutes) | `dialogue: adversarial_review_test.go` (`JS_REVIEW_STRICT=1` kept for future rounds) |
| Crisis corpus (Hindi/Hinglish/English, every reviewer example) and false-positive corpus; helplines by type, never 1906; low-confidence distress → 112; real helplines.json with tag map and per-use expiry; redaction export; short greeting; repeat only as whole utterance; illegal/voting redirect; look-alike injection | `dialogue: safety_test.go` |
| Reviewer identity and review_due format | `scheme: TestValidReviewer, TestReviewDueMustParse` |
| Bundle rollback refused | `knowledge: TestLoadBundleRefusesRollback` |
| Look-alike folding; short romanised words exact | `textnorm: TestCanonFoldsLookAlikes, TestShortLatinWordsAreNotFuzzy` |

Integration run on 2026-11-09 (M1 8 GB under heavy swap, Gemma 4 E2B Q4_0,
llama-server build 12110, `-c 4096 -np 1 -fa on --reasoning off -ngl 99`):
classifier 10/10, median 884 ms (first call 2.6 s); rephrase 6 accepted, 1
rejected (the model replaced "टेस्ट फिक्स्चर योजना" with "ट्रायल योजना", a
new scheme phrase, so it was correctly rejected), median 1.0 s.

## 8. Pending human sign-off

1. **Crisis vocabulary** (`dialogue/crisis.go`, `crisisPhrases`,
   `crisisPairs`, `crisisDistressWords`, `helplineTagCrisis`): review by a
   trained counsellor / psychosocial reviewer before deployment.
2. **112 hard-coded fallback** (decision 10): documented exception to the
   "facts only from verified records" rule.
3. **Rephrase on pi5** (decision 5): off by default; re-enable only after
   field validation of the allowlist validator.
