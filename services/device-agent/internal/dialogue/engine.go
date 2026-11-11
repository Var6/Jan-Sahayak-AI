package dialogue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/ai"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/textnorm"
)

// Config wires the conversation engine.
type Config struct {
	// Knowledge is required: the only source of scheme facts.
	Knowledge *knowledge.Store
	// Gazetteer (optional) recognises district names.
	Gazetteer *knowledge.Gazetteer
	// Helplines (optional) are verified crisis helplines; without them the
	// crisis reply still gives the national emergency number 112.
	Helplines *Helplines
	// LLM (optional) classifies ambiguous input and rephrases answers.
	LLM ai.LLM
	// EnableRephrase turns on LLM rephrasing of composed answers.
	EnableRephrase bool
	// MinSTTConfidence: voice input below this is asked to be repeated.
	MinSTTConfidence float64
	// MaxMisses: consecutive not-understood turns before escalation.
	MaxMisses int
	// Timeouts for LLM calls.
	ClassifyTimeout time.Duration
	RephraseTimeout time.Duration
	HealthTimeout   time.Duration
	// Logger receives no citizen text, only reasons and counters.
	Logger *slog.Logger
}

// Stats are counters for health pages and the integration test.
type Stats struct {
	ClassifyCalls, ClassifyFailed                      int64
	RephraseAccepted, RephraseRejected, RephraseFailed int64
}

type stats struct {
	classifyCalls, classifyFailed                      atomic.Int64
	rephraseAccepted, rephraseRejected, rephraseFailed atomic.Int64
}

// Conversation implements Engine. It holds no per-session state.
type Conversation struct {
	cfg   Config
	log   *slog.Logger
	stats stats
}

var _ Engine = (*Conversation)(nil)

// New validates cfg and applies defaults.
func New(cfg Config) (*Conversation, error) {
	if cfg.Knowledge == nil {
		return nil, errors.New("dialogue: Knowledge is required")
	}
	if cfg.MinSTTConfidence <= 0 {
		cfg.MinSTTConfidence = 0.45
	}
	if cfg.MaxMisses <= 0 {
		cfg.MaxMisses = 3
	}
	if cfg.ClassifyTimeout <= 0 {
		cfg.ClassifyTimeout = 4 * time.Second
	}
	if cfg.RephraseTimeout <= 0 {
		cfg.RephraseTimeout = 8 * time.Second
	}
	if cfg.HealthTimeout <= 0 {
		cfg.HealthTimeout = 500 * time.Millisecond
	}
	lg := cfg.Logger
	if lg == nil {
		lg = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Conversation{cfg: cfg, log: lg}, nil
}

// Stats returns a snapshot of the LLM counters.
func (e *Conversation) Stats() Stats {
	return Stats{
		ClassifyCalls: e.stats.classifyCalls.Load(), ClassifyFailed: e.stats.classifyFailed.Load(),
		RephraseAccepted: e.stats.rephraseAccepted.Load(), RephraseRejected: e.stats.rephraseRejected.Load(),
		RephraseFailed: e.stats.rephraseFailed.Load(),
	}
}

func normLang(l Lang) Lang {
	if l == English {
		return English
	}
	return Hindi
}

// Greet returns the opening message. It discloses that the kiosk is a
// computer assistant (not an official) and warns against sharing secrets.
func (e *Conversation) Greet(lang Lang) Reply {
	l := normLang(lang)
	return Reply{
		Kind: KindGreeting, Text: m.greeting.in(l), Speech: speakable(m.greetingSpeech.in(l)),
		Options:    withGlobals(l, topicOptions(l, primaryTopics, true)),
		ListenNext: true,
		Analytics:  Analytics{Intent: IntentGreeting, Resolution: ResInProgress},
	}
}

// Handle processes one citizen turn. Order of guards: option validation →
// empty speech → crisis (on the FULL raw text, before the STT-confidence
// gate and the length cap) → STT confidence → length/control-char
// sanitising → sensitive data → prompt injection → understanding.
func (e *Conversation) Handle(ctx context.Context, st *State, in Input) (Reply, error) {
	if st == nil {
		return Reply{}, errors.New("dialogue: nil state")
	}
	if st.Slots == nil {
		st.Slots = map[string]string{}
	}
	if st.Lang == "" {
		st.Lang = normLang(in.Lang)
	}
	st.Lang = normLang(st.Lang)
	r, logText := e.handle(ctx, st, in)
	e.finish(st, &r, logText)
	return r, nil
}

func (e *Conversation) handle(ctx context.Context, st *State, in Input) (Reply, string) {
	if in.Source == FromOption {
		id := strings.TrimSpace(in.OptionID)
		if !optionAllowed(st, id) {
			return e.miss(st, m.invalidOption), "[option]"
		}
		return e.handleOption(ctx, st, id), "[option]"
	}
	if in.Source == FromVoice && (in.NoSpeech || strings.TrimSpace(in.Text) == "") {
		return e.notHeard(st), ""
	}
	// Crisis first, on the whole input: a low STT confidence does not make
	// "आत्महत्या" less alarming, and a crisis phrase after the length cap
	// must still be seen.
	full := cleanText(in.Text)
	if ct, ok := detectCrisis(full); ok {
		return e.crisis(st, ct), "[crisis]"
	}
	if in.Source == FromVoice && in.STTConfidence < e.cfg.MinSTTConfidence {
		if hasDistress(full) {
			st.Distress = true
		}
		return e.notHeard(st), ""
	}
	text, _ := sanitize(in.Text)
	if text == "" {
		return e.notHeard(st), ""
	}
	kind, redacted := detectSensitive(text)
	if detectInjection(redacted) {
		return e.redirect(st, intentInjection, m.injection.in(st.Lang)), "[blocked]"
	}
	if kind != sensNone {
		// Continue only if the rest of the turn clearly asks about a topic;
		// otherwise the reply is just the privacy warning.
		cls := classify(textnorm.Analyze(redacted))
		if !(isIn(welfareIntents, cls.Intent) && cls.Score >= 2) && len(e.cfg.Knowledge.MatchNames(redacted)) == 0 {
			return e.withSensitiveWarning(st, Reply{}, false), redacted
		}
		r, understood := e.understand(ctx, st, redacted)
		return e.withSensitiveWarning(st, r, understood), redacted
	}
	r, _ := e.understand(ctx, st, redacted)
	return r, redacted
}

// withSensitiveWarning prefixes the privacy warning. If nothing else was
// understood the turn is only a warning (not counted as a miss).
func (e *Conversation) withSensitiveWarning(st *State, r Reply, understood bool) Reply {
	l := st.Lang
	w := m.sensitive.in(l)
	if !understood {
		opts := topicOptions(l, primaryTopics, true)
		if st.Last != nil && len(st.Last.Options) > 0 {
			opts = st.Last.Options
		}
		st.Misses = 0
		return Reply{Kind: KindQuestion, Text: w, Speech: speakable(w), Options: withGlobals(l, stripGlobals(opts)), ListenNext: true,
			Analytics: Analytics{Intent: intentSensitive, Resolution: ResInProgress}}
	}
	r.Text = w + "\n\n" + r.Text
	sp := r.Speech
	if sp == "" {
		sp = r.Text
	}
	r.Speech = speakable(w + " " + sp)
	return r
}

func stripGlobals(opts []Option) []Option {
	var out []Option
	for _, o := range opts {
		if o.ID != optRepeat && o.ID != optEnd {
			out = append(out, o)
		}
	}
	return out
}

// understand routes sanitised, redacted text. The bool is false when the
// engine could not make sense of the turn.
func (e *Conversation) understand(ctx context.Context, st *State, text string) (Reply, bool) {
	a := textnorm.Analyze(text)
	if d, ok := e.cfg.Gazetteer.Match(text); ok {
		setSlot(st, slotDistrict, d.ID)
	}
	cls := classify(a)
	names := e.cfg.Knowledge.MatchNames(text)

	// 1. The answer to a sub-question ("खेती के लिए") beats topic detection.
	if st.Step == stepSub {
		if fd, ok := flows[st.Flow]; ok && fd.sub != nil {
			if id, ok := fd.sub.matchSub(a); ok {
				return e.handleOption(ctx, st, optSubPrefix+id), true
			}
		}
	}
	strongNew := (len(names) > 0 && names[0].ID != st.Scheme) ||
		(isIn(welfareIntents, cls.Intent) && cls.Score >= 2.5 && len(cls.Ambiguous) == 0 && cls.Intent != st.Flow)

	// 2. Answers to offered options (yes/no, ordinals, labels).
	if !strongNew && (st.Flow != "" || st.Step == stepMore || st.Step == stepMenu) {
		if id, ok := matchOffered(a, st); ok {
			return e.handleOption(ctx, st, id), true
		}
	}
	// 3. Follow-ups about the scheme being discussed.
	if !strongNew && st.Scheme != "" {
		if r, ok := e.followUp(ctx, st, a); ok {
			return r, true
		}
	}
	// 4. A scheme named explicitly.
	if len(names) > 0 && (strongNew || st.Scheme == "") {
		rec, err := e.cfg.Knowledge.Get(ctx, names[0].ID)
		if err != nil {
			return e.technical(st), true
		}
		resetFlow(st)
		st.Flow = IntentSchemeByName
		return e.selectScheme(ctx, st, rec), true
	}
	// A scheme the citizen NAMED but we have no record of ("पीएम किसान"
	// without a pm-kisan record): say so first, then offer related options,
	// so another scheme's facts are never taken as the answer.
	unknownName := len(names) == 0 && namesUnknownScheme(a)
	// 5. Ambiguous welfare topics: LLM label if available, else clarify.
	if len(cls.Ambiguous) > 0 && cls.Score < 3 || len(cls.Ambiguous) > 1 {
		if lbl, ok := e.llmClassify(ctx, text); ok && isIn(welfareIntents, lbl) {
			return e.markUnknownName(st, e.startFlow(ctx, st, lbl, cls.Slots, a), unknownName), true
		}
		return e.markUnknownName(st, e.clarify(st, cls.Ambiguous), unknownName), true
	}
	// 6. Deterministic intent.
	if r, ok := e.route(ctx, st, cls.Intent, cls.Slots, a); ok {
		if isIn(welfareIntents, cls.Intent) {
			r = e.markUnknownName(st, r, unknownName)
		}
		return r, true
	}
	// 7. Unknown: try the knowledge index, then the LLM, then ask.
	if cands, err := e.cfg.Knowledge.Search(ctx, text, knowledge.SearchOptions{Limit: MaxCandidates}); err == nil && len(cands) > 0 {
		resetFlow(st)
		st.Flow = IntentSchemeByName
		var recs []scheme.Record
		for _, c := range cands {
			recs = append(recs, c.Record)
		}
		return e.offerPick(st, recs, m.suggestionsFromSearch.in(st.Lang)), true
	}
	if lbl, ok := e.llmClassify(ctx, text); ok {
		if r, ok := e.route(ctx, st, lbl, nil, a); ok {
			return r, true
		}
	}
	return e.miss(st, m.notUnderstood), false
}

// route dispatches a known intent.
func (e *Conversation) route(ctx context.Context, st *State, intent string, slots map[string]string, a textnorm.Analysis) (Reply, bool) {
	l := st.Lang
	switch {
	case isIn(welfareIntents, intent):
		return e.startFlow(ctx, st, intent, slots, a), true
	case isIn(disallowedIntents, intent):
		txt := m.offTopic.in(l)
		if line, ok := offTopicLine[intent]; ok {
			txt += " " + line.in(l)
		}
		return e.redirect(st, intentOffTopic, txt+" "+m.menuPrompt.in(l)), true
	}
	switch intent {
	case IntentGreeting:
		g := e.Greet(l)
		resetFlow(st)
		return g, true
	case IntentThanks:
		st.Step = stepMore
		t := m.thanks.in(l) + " " + m.moreHelp.in(l)
		return Reply{Kind: KindQuestion, Text: t, Options: moreOptions(l), ListenNext: true,
			Analytics: Analytics{Intent: IntentThanks, Resolution: ResInProgress}}, true
	case IntentHelp:
		resetFlow(st)
		return e.menuReply(st, m.help.in(l), IntentHelp, primaryTopics), true
	case IntentRepeat:
		return e.repeat(st), true
	case IntentEnd:
		return e.goodbye(st), true
	case IntentHuman:
		return e.human(st), true
	case IntentKeyboard:
		return e.keyboard(st), true
	case IntentChangeLang:
		if a.Has(textnorm.CEnglish) {
			st.Lang = English
		} else if a.Has(textnorm.CHindi) {
			st.Lang = Hindi
		}
		l = st.Lang
		return e.menuReply(st, m.langSwitched.in(l)+" "+m.menuPrompt.in(l), IntentChangeLang, primaryTopics), true
	}
	return Reply{}, false
}

// topicConcepts are carried from the opening utterance into the search.
var topicConcepts = []string{
	textnorm.CLoan, textnorm.CPension, textnorm.CElderly, textnorm.CWidow, textnorm.CDisability, textnorm.CScholarship,
	textnorm.CEducation, textnorm.CFarmer, textnorm.CCertificate, textnorm.CCaste, textnorm.CIncome, textnorm.CResidence,
	textnorm.CBirth, textnorm.CDeath, textnorm.CRation, textnorm.CCard, textnorm.CHousing, textnorm.CHealth,
	textnorm.CInsurance, textnorm.CBusiness, textnorm.CJob, textnorm.CWomen, textnorm.CChild, textnorm.CIdentity,
}

func (e *Conversation) startFlow(ctx context.Context, st *State, intent string, slots map[string]string, a textnorm.Analysis) Reply {
	fd, ok := flows[intent]
	if !ok {
		return e.miss(st, m.notUnderstood)
	}
	resetFlow(st)
	st.Flow = intent
	var kw []string
	for _, c := range topicConcepts {
		if a.Has(c) {
			kw = append(kw, c)
		}
	}
	setSlot(st, slotKeywords, strings.Join(kw, " "))
	for k, v := range slots {
		if fd.sub != nil && k == fd.sub.slot {
			setSlot(st, k, v)
		}
	}
	if intent == IntentCertificate && a.Has(textnorm.CAadhaar) {
		setSlot(st, slotAadhaarOnly, "1")
	}
	if fd.sub != nil && st.Slots[fd.sub.slot] == "" {
		return e.askSub(st, fd.sub)
	}
	return e.findCandidates(ctx, st)
}

func (e *Conversation) askSub(st *State, q *subQuestion) Reply {
	l := st.Lang
	st.Step = stepSub
	var opts []Option
	for _, o := range q.opts {
		opts = append(opts, Option{ID: optSubPrefix + o.id, Label: o.label.in(l)})
	}
	return Reply{Kind: KindQuestion, Text: q.ask.in(l), Options: withGlobals(l, opts), ListenNext: true,
		Analytics: Analytics{Intent: st.Flow, Resolution: ResInProgress}}
}

// findCandidates searches the knowledge base for the current flow.
func (e *Conversation) findCandidates(ctx context.Context, st *State) Reply {
	fd := flows[st.Flow]
	cats, query := fd.categories, fd.query
	var subConcept string
	if fd.sub != nil {
		if o, ok := fd.sub.find(st.Slots[fd.sub.slot]); ok {
			cats, query = o.categories, o.query
			if len(o.concepts) > 0 && o.concepts[0] != textnorm.CMore {
				subConcept = o.concepts[0]
			}
		}
	}
	query += " " + st.Slots[slotKeywords]
	cands, err := e.cfg.Knowledge.Search(ctx, query, knowledge.SearchOptions{Limit: knowledge.MaxSearchResults, Categories: cats})
	if err != nil {
		e.log.Error("dialogue: knowledge search failed", "err", err)
		return e.technical(st)
	}
	var recs []scheme.Record
	top := 0.0
	for _, c := range cands {
		if fd.sub != nil && fd.concept != "" {
			if !recordHas(c.Record, fd.concept) || (subConcept != "" && !recordHas(c.Record, subConcept)) {
				continue
			}
		}
		if top == 0 {
			top = c.Score
		}
		if c.Score < 0.6*top {
			break
		}
		recs = append(recs, c.Record)
		if len(recs) >= MaxCandidates {
			break
		}
	}
	if len(recs) == 0 && fd.sub == nil {
		for _, cat := range cats {
			rs, err := e.cfg.Knowledge.ByCategory(ctx, cat)
			if err != nil {
				return e.technical(st)
			}
			for _, r := range rs {
				if len(recs) < MaxCandidates {
					recs = append(recs, r)
				}
			}
		}
	}
	switch len(recs) {
	case 0:
		return e.noVerified(st, cats)
	case 1:
		return e.selectScheme(ctx, st, recs[0])
	default:
		return e.offerPick(st, recs, m.pickScheme.in(st.Lang))
	}
}

// recordHas reports whether a record's names/aliases/keywords carry a concept.
func recordHas(r scheme.Record, concept string) bool {
	parts := append([]string{r.Name.Hi, r.Name.En}, r.Aliases...)
	parts = append(parts, r.Keywords...)
	for _, p := range parts {
		if textnorm.Analyze(p).Has(concept) {
			return true
		}
	}
	return false
}

func (e *Conversation) offerPick(st *State, recs []scheme.Record, prompt string) Reply {
	l := st.Lang
	st.Step = stepPick
	var ids []string
	var opts []Option
	for _, r := range recs {
		if len(opts) >= MaxCandidates {
			break
		}
		ids = append(ids, r.ID)
		opts = append(opts, Option{ID: optPickPrefix + r.ID, Label: tx(r.Name, l)})
	}
	setSlot(st, slotCands, strings.Join(ids, ","))
	return Reply{Kind: KindQuestion, Text: prompt, Options: withGlobals(l, opts), ListenNext: true,
		Analytics: Analytics{Intent: st.Flow, Resolution: ResInProgress}}
}

func (e *Conversation) selectScheme(ctx context.Context, st *State, rec scheme.Record) Reply {
	st.Scheme = rec.ID
	mode := e.cfg.Knowledge.Mode(rec)
	if mode != knowledge.ModeNameOnly && len(questionsFor(rec)) > 0 {
		st.Step = stepQ
		setSlot(st, slotQIndex, "0")
		return e.askQuestion(st, rec, 0, mode)
	}
	return e.answer(ctx, st, rec)
}

func (e *Conversation) askQuestion(st *State, rec scheme.Record, i int, mode knowledge.Mode) Reply {
	l := st.Lang
	q := questionsFor(rec)[i]
	var opts []Option
	if q.Kind == "yes_no" {
		opts = []Option{{ID: optAnsPrefix + "yes", Label: m.optYes.in(l)}, {ID: optAnsPrefix + "no", Label: m.optNo.in(l)}}
	} else {
		for j, c := range q.Choices {
			opts = append(opts, Option{ID: optAnsPrefix + "c" + strconv.Itoa(j), Label: tx(c, l)})
		}
	}
	text := tx(q.Ask, l)
	if mode == knowledge.ModeUnverifiedDev {
		text = knowledge.UnverifiedDisclaimer(string(l)) + "\n\n" + text
	}
	return Reply{Kind: KindQuestion, Text: text, Speech: speakable(text), Options: withGlobals(l, opts), ListenNext: true,
		Analytics: Analytics{Intent: st.Flow, Category: rec.Category, Service: rec.ID, Resolution: ResInProgress}}
}

func (e *Conversation) answer(ctx context.Context, st *State, rec scheme.Record) Reply {
	l := st.Lang
	mode := e.cfg.Knowledge.Mode(rec)
	var notes []string
	if st.Slots[slotAadhaarOnly] == "1" && (st.Slots[slotCert] == "caste" || recordHas(rec, textnorm.CCaste)) {
		notes = append(notes, m.aadhaarNotCaste.in(l)) // general fact about Aadhaar, not a record fact
	}
	parts := composeAnswer(rec, mode, l, notes, st.Slots[slotIneligible] == "1")
	summary, used := parts.summary, false
	if mode != knowledge.ModeNameOnly {
		summary, used = e.rephrase(ctx, parts.summary, []scheme.Record{rec}, l)
	}
	text, speech := parts.render(l, summary)
	if st.Slots[slotUnknownName] == "1" {
		// The citizen named a different scheme earlier in this flow.
		text = m.unknownSchemeAnswer.in(l) + "\n\n" + text
		speech = speakable(m.unknownSchemeAnswer.in(l) + " " + speech)
	}
	st.Step = stepMore
	res := ResAnswered
	if mode != knowledge.ModeFactual {
		res = ResUnverified
	}
	return Reply{Kind: KindAnswer, Text: text, Speech: speech, Cards: []Card{parts.card}, Options: moreOptions(l),
		ListenNext: true, UsedLLM: used,
		Analytics: Analytics{Intent: st.Flow, Category: rec.Category, Service: rec.ID, Resolution: res}}
}

// followUp answers "how much?" / "which documents?" / "I only have Aadhaar"
// about the scheme being discussed, strictly from the record.
func (e *Conversation) followUp(ctx context.Context, st *State, a textnorm.Analysis) (Reply, bool) {
	rec, err := e.cfg.Knowledge.Get(ctx, st.Scheme)
	if err != nil {
		return Reply{}, false
	}
	l := st.Lang
	mode := e.cfg.Knowledge.Mode(rec)
	if a.Has(textnorm.CAadhaar) && (st.Slots[slotCert] == "caste" || recordHas(rec, textnorm.CCaste)) {
		setSlot(st, slotAadhaarOnly, "1")
		return e.answer(ctx, st, rec), true
	}
	var items []string
	var empty bi
	switch {
	case a.Has(textnorm.CAmount) || a.Has(textnorm.CMoney) && !a.Has(textnorm.CNotReceived):
		items, empty = txs(rec.Benefits, l), m.noBenefitInfo
	case a.Has(textnorm.CDocument):
		items, empty = docs(rec.MandatoryDocs, l), m.noDocInfo
	default:
		return Reply{}, false
	}
	st.Step = stepMore
	name := tx(rec.Name, l)
	res := ResAnswered
	var body string
	switch {
	case mode == knowledge.ModeNameOnly:
		body, res = knowledge.NotVerifiedMessage(string(l)), ResUnverified
	case len(items) == 0:
		body, res = empty.in(l), ResUnresolved
	default:
		body = strings.Join(items, "। ")
		if mode == knowledge.ModeUnverifiedDev {
			body = knowledge.UnverifiedDisclaimer(string(l)) + " " + body
			res = ResUnverified
		}
	}
	text := name + ": " + body + "\n\n" + m.moreHelp.in(l)
	return Reply{Kind: KindAnswer, Text: text, Speech: speakable(text), Options: moreOptions(l), ListenNext: true,
		Analytics: Analytics{Intent: st.Flow, Category: rec.Category, Service: rec.ID, Resolution: res}}, true
}

// handleOption executes a validated option id.
func (e *Conversation) handleOption(ctx context.Context, st *State, id string) Reply {
	l := st.Lang
	switch {
	case id == optRepeat:
		return e.repeat(st)
	case id == optEnd || id == optMoreNo:
		return e.goodbye(st)
	case id == optHuman:
		return e.human(st)
	case id == optKeyboard:
		return e.keyboard(st)
	case id == optMenu || id == optMoreYes:
		resetFlow(st)
		return e.menuReply(st, m.menuPrompt.in(l), IntentHelp, primaryTopics)
	case id == optOtherTopics:
		resetFlow(st)
		return e.menuReply(st, m.menuPrompt.in(l), IntentHelp, otherTopics)
	case strings.HasPrefix(id, optTopicPrefix):
		return e.startFlow(ctx, st, strings.TrimPrefix(id, optTopicPrefix), nil, textnorm.Analysis{})
	case strings.HasPrefix(id, optSubPrefix):
		fd, ok := flows[st.Flow]
		if !ok || fd.sub == nil {
			return e.miss(st, m.invalidOption)
		}
		v := strings.TrimPrefix(id, optSubPrefix)
		if _, ok := fd.sub.find(v); !ok {
			return e.miss(st, m.invalidOption)
		}
		setSlot(st, fd.sub.slot, v)
		return e.findCandidates(ctx, st)
	case strings.HasPrefix(id, optPickPrefix):
		rid := strings.TrimPrefix(id, optPickPrefix)
		if !isIn(strings.Split(st.Slots[slotCands], ","), rid) {
			return e.miss(st, m.invalidOption)
		}
		rec, err := e.cfg.Knowledge.Get(ctx, rid)
		if err != nil {
			return e.technical(st)
		}
		return e.selectScheme(ctx, st, rec)
	case strings.HasPrefix(id, optAnsPrefix):
		return e.answerQuestion(ctx, st, strings.TrimPrefix(id, optAnsPrefix))
	}
	return e.miss(st, m.invalidOption)
}

func (e *Conversation) answerQuestion(ctx context.Context, st *State, v string) Reply {
	if st.Step != stepQ || st.Scheme == "" {
		return e.miss(st, m.invalidOption)
	}
	rec, err := e.cfg.Knowledge.Get(ctx, st.Scheme)
	if err != nil {
		return e.technical(st)
	}
	qs := questionsFor(rec)
	i, _ := strconv.Atoi(st.Slots[slotQIndex])
	if i < 0 || i >= len(qs) {
		return e.answer(ctx, st, rec)
	}
	q := qs[i]
	vals := []string{v}
	if strings.HasPrefix(v, "c") && q.Kind == "choice" {
		if j, err := strconv.Atoi(v[1:]); err == nil && j >= 0 && j < len(q.Choices) {
			vals = append(vals, q.Choices[j].Hi, q.Choices[j].En, strconv.Itoa(j))
		}
	}
	for _, x := range q.IneligibleIf {
		if isIn(vals, x) {
			setSlot(st, slotIneligible, "1")
		}
	}
	setSlot(st, slotQPrefix+q.ID, v)
	i++
	setSlot(st, slotQIndex, strconv.Itoa(i))
	if i < len(qs) {
		return e.askQuestion(st, rec, i, e.cfg.Knowledge.Mode(rec))
	}
	return e.answer(ctx, st, rec)
}

// --- simple replies ----------------------------------------------------

func moreOptions(l Lang) []Option {
	return withGlobals(l, []Option{{ID: optMoreYes, Label: m.optMoreYes.in(l)}, {ID: optMoreNo, Label: m.optMoreNo.in(l)}})
}

func (e *Conversation) menuReply(st *State, text, intent string, topics []string) Reply {
	l := st.Lang
	st.Step = stepMenu
	return Reply{Kind: KindQuestion, Text: text, Speech: speakable(text),
		Options:    withGlobals(l, topicOptions(l, topics, isIn(topics, IntentLoan))),
		ListenNext: true, Analytics: Analytics{Intent: intent, Resolution: ResInProgress}}
}

func (e *Conversation) clarify(st *State, intents []string) Reply {
	l := st.Lang
	resetFlow(st)
	st.Step = stepMenu
	topics := intents
	if len(topics) < 2 {
		topics = append(append([]string{}, topics...), primaryTopics...)
	}
	var uniq []string
	for _, t := range topics {
		if !isIn(uniq, t) && isIn(welfareIntents, t) {
			uniq = append(uniq, t)
		}
	}
	return Reply{Kind: KindQuestion, Text: m.clarifyBetween.in(l), Options: withGlobals(l, topicOptions(l, uniq, false)), ListenNext: true,
		Analytics: Analytics{Intent: IntentUnknown, Resolution: ResInProgress}}
}

func (e *Conversation) redirect(st *State, intent, text string) Reply {
	l := st.Lang
	resetFlow(st)
	st.Step = stepMenu
	return Reply{Kind: KindRedirect, Text: text, Speech: speakable(text), Options: withGlobals(l, topicOptions(l, primaryTopics, true)),
		ListenNext: true, Analytics: Analytics{Intent: intent, Resolution: ResRedirected}}
}

func (e *Conversation) noVerified(st *State, cats []string) Reply {
	l := st.Lang
	st.Step = stepMore
	st.Scheme = ""
	cat := ""
	if len(cats) > 0 {
		cat = cats[0]
	}
	text := m.noVerified.in(l) + "\n\n" + m.moreHelp.in(l)
	return Reply{Kind: KindEscalate, Text: text, Speech: speakable(text), Options: moreOptions(l), ListenNext: true,
		Analytics: Analytics{Intent: st.Flow, Category: cat, Resolution: ResUnresolved}}
}

func (e *Conversation) technical(st *State) Reply {
	l := st.Lang
	resetFlow(st)
	return Reply{Kind: KindEscalate, Text: m.technical.in(l), Options: withGlobals(l, []Option{{ID: optMenu, Label: m.optMenu.in(l)}}),
		ListenNext: true, Analytics: Analytics{Intent: st.Flow, Resolution: ResUnresolved}}
}

func (e *Conversation) repeat(st *State) Reply {
	if st.Last == nil {
		return e.Greet(st.Lang)
	}
	r := *st.Last
	r.Analytics = Analytics{Intent: IntentRepeat, Category: st.Last.Analytics.Category, Service: st.Last.Analytics.Service, Resolution: ResInProgress}
	return r
}

func (e *Conversation) goodbye(st *State) Reply {
	l := st.Lang
	resetFlow(st)
	return Reply{Kind: KindGoodbye, Text: m.goodbye.in(l), Speech: speakable(m.goodbye.in(l)), EndSession: true,
		Analytics: Analytics{Intent: IntentEnd, Resolution: ResAnswered}}
}

func (e *Conversation) human(st *State) Reply {
	l := st.Lang
	resetFlow(st)
	return Reply{Kind: KindEscalate, Text: m.humanHelp.in(l), Speech: speakable(m.humanHelp.in(l)),
		Options:    withGlobals(l, []Option{{ID: optMenu, Label: m.optMenu.in(l)}}),
		ListenNext: false, Analytics: Analytics{Intent: IntentHuman, Resolution: ResEscalated}}
}

func (e *Conversation) keyboard(st *State) Reply {
	l := st.Lang
	return Reply{Kind: KindQuestion, Text: m.keyboard.in(l), Options: withGlobals(l, []Option{{ID: optMenu, Label: m.optMenu.in(l)}}),
		ListenNext: false, Analytics: Analytics{Intent: IntentKeyboard, Resolution: ResInProgress}}
}

// notHeard handles no speech / low STT confidence.
func (e *Conversation) notHeard(st *State) Reply {
	return e.miss(st, m.repeatAudio)
}

// markUnknownName prefixes the "no verified information on that scheme"
// line and remembers it for the rest of the flow (answer() repeats it).
func (e *Conversation) markUnknownName(st *State, r Reply, unknown bool) Reply {
	if !unknown || r.Kind == KindCrisis || r.Kind == KindGoodbye {
		return r
	}
	l := st.Lang
	setSlot(st, slotUnknownName, "1")
	r.Text = m.unknownScheme.in(l) + "\n\n" + r.Text
	sp := r.Speech
	if sp == "" {
		sp = r.Text
	} else {
		sp = m.unknownScheme.in(l) + " " + sp
	}
	r.Speech = speakable(sp)
	return r
}

// miss counts a not-understood turn; after MaxMisses it escalates to the
// keyboard / a human.
func (e *Conversation) miss(st *State, msg bi) Reply {
	l := st.Lang
	st.Misses++
	extra := ""
	if st.Distress {
		extra = " " + distress112.in(l)
	}
	if st.Misses >= e.cfg.MaxMisses {
		st.Misses = 0
		return Reply{Kind: KindEscalate, Text: m.tooManyMisses.in(l) + extra, Speech: speakable(m.tooManyMisses.in(l) + extra),
			Options: withGlobals(l, []Option{{ID: optKeyboard, Label: m.optKeyboard.in(l)}, {ID: optHuman, Label: m.optHuman.in(l)},
				{ID: optMenu, Label: m.optMenu.in(l)}}),
			ListenNext: false, Analytics: Analytics{Intent: intentNoSpeech, Resolution: ResEscalated}}
	}
	opts := topicOptions(l, primaryTopics, true)
	if st.Last != nil && len(stripGlobals(st.Last.Options)) > 0 {
		opts = stripGlobals(st.Last.Options)
	}
	kind := KindRepeat
	if msg == m.notUnderstood || msg == m.invalidOption {
		kind = KindQuestion
	}
	return Reply{Kind: kind, Text: msg.in(l) + extra, Speech: speakable(msg.in(l) + extra), Options: withGlobals(l, opts), ListenNext: true,
		Analytics: Analytics{Intent: intentNoSpeech, Resolution: ResUnresolved}}
}

// crisis builds the safety reply. The welfare flow is abandoned.
func (e *Conversation) crisis(st *State, ct crisisType) Reply {
	l := st.Lang
	resetFlow(st)
	lines := []string{crisisIntro[ct].in(l), crisisCall112.in(l)}
	if hs := e.cfg.Helplines.ForAt(ct, 3, e.now()); len(hs) > 0 {
		lines = append(lines, crisisOtherNumbers.in(l))
		for _, h := range hs {
			lines = append(lines, "• "+h.Name.in(l)+": "+h.Number)
		}
	}
	lines = append(lines, crisisOutro.in(l))
	text := strings.Join(lines, "\n")
	return Reply{Kind: KindCrisis, Text: text, Speech: speakable(strings.ReplaceAll(text, "•", "")),
		Options:    withGlobals(l, []Option{{ID: optHuman, Label: m.optHuman.in(l)}}),
		ListenNext: false, Analytics: Analytics{Intent: intentCrisis, Resolution: ResCrisis}}
}

// now is the engine clock (the knowledge store's, so tests can fix it).
func (e *Conversation) now() time.Time {
	if c := e.cfg.Knowledge.Config(); c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// --- state helpers -------------------------------------------------------

func optionAllowed(st *State, id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	if globalOptions[id] {
		return true
	}
	if strings.HasPrefix(id, optTopicPrefix) {
		_, ok := flows[strings.TrimPrefix(id, optTopicPrefix)]
		return ok
	}
	if st.Last == nil {
		return false
	}
	for _, o := range st.Last.Options {
		if o.ID == id {
			return true
		}
	}
	return false
}

// setSlot stores a bounded value; new keys beyond MaxSlots are dropped.
func setSlot(st *State, k, v string) bool {
	if utf8.RuneCountInString(v) > maxSlotValue {
		v = string([]rune(v)[:maxSlotValue])
	}
	if _, exists := st.Slots[k]; !exists && len(st.Slots) >= MaxSlots {
		return false
	}
	st.Slots[k] = v
	return true
}

func resetFlow(st *State) {
	st.Flow, st.Step, st.Scheme = "", "", ""
	for k := range st.Slots {
		if k != slotDistrict {
			delete(st.Slots, k)
		}
	}
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// finish records bounded history and the last reply, and guarantees the
// "repeat" / "end" options on every non-final reply.
func (e *Conversation) finish(st *State, r *Reply, logText string) {
	if r.Kind != KindGoodbye && !r.EndSession {
		hasRepeat := false
		for _, o := range r.Options {
			if o.ID == optRepeat {
				hasRepeat = true
			}
		}
		if !hasRepeat {
			r.Options = withGlobals(st.Lang, r.Options)
		}
	}
	if r.Analytics.Intent != intentNoSpeech {
		st.Misses = 0
		st.Distress = false
	}
	last := *r
	st.Last = &last
	st.History = append(st.History, Turn{Citizen: truncRunes(logText, 200), Kiosk: truncRunes(r.Text, 300)})
	if over := len(st.History) - MaxHistory; over > 0 {
		st.History = append([]Turn(nil), st.History[over:]...)
	}
}
