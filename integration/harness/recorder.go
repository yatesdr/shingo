package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
)

// ── The scenario recorder ───────────────────────────────────────────────────
//
// RECORD ONLY. A scenario run with RecordEnv set to a directory writes, per
// test (each subtest is its own cell), what the harness carried and how hard it
// worked for it:
//
//   - <cell>.transcript: every envelope the Bus delivered for the cell, in the
//     order it was published, one line each: the bus's ordinal in the cell (a
//     restarted Edge gets a new Bus), the direction (E>C, C>E), and the
//     envelope normalised (normaliser below), so two runs of one cell compare
//     with diff and two cells with one transcript share a sha256.
//   - <cell>.raw: the same envelopes as they were sent, for reading a
//     transcript difference back to the bytes.
//   - <cell>.counts.json: the bins pushes by caller (and how many of each
//     changed the Edge's copy), every wait a scenario records (passes until
//     its condition held, wall, and whether the satisfying pass followed one of
//     Core's robot polls), counters, and notes (S6's TriggeredBy).
//
// Unset, RecorderFor returns nil, every method on a nil *Recorder and a nil
// *Wait returns at once, and the Bus and BinsPush take no stamp and build no
// line: a run without it costs one nil check per message, push and wait and
// takes every path it takes today.
//
// The reader is TestRecordReport (recorder_report_test.go).

// RecordEnv names the directory the recorder writes to. One run, one
// directory: a cell that runs twice in one process (-count=2) writes its
// second record beside the first with a ".rN" suffix.
const RecordEnv = "SHINGO_SCENARIO_RECORD"

var recordDir = sync.OnceValue(func() string { return os.Getenv(RecordEnv) })

var (
	recMu sync.Mutex
	recs  = map[string]*Recorder{}
)

// Recorder is one cell's record. Safe for concurrent use: Core's robot poll and
// the test goroutine write to it, and so may a callback a scenario taps (CATID's
// tick shipper).
type Recorder struct {
	name string
	dir  string

	mu      sync.Mutex
	flushed bool
	buses   int
	raw     []string // "<bus> <route> <payload as sent>", the views are built from at flush
	pushes  map[string]*pushCount
	waits   []*Wait
	counts  map[string]int64
	notes   map[string][]string
	polls   []time.Time // Core robot polls observed (deckFleet)
	started time.Time   // Core's engine started: the poll schedule's origin when no poll is observed
	failed  bool
}

type pushCount struct {
	N       int64 `json:"n"`
	Changed int64 `json:"changed"`
	WallNS  int64 `json:"wall_ns"`
}

// RecorderFor is t's recorder, or nil when RecordEnv is unset. Every caller
// with the same t gets the same one; it writes its files when t ends.
func RecorderFor(t testing.TB) *Recorder {
	dir := recordDir()
	if dir == "" {
		return nil
	}
	recMu.Lock()
	defer recMu.Unlock()
	name := t.Name()
	if r := recs[name]; r != nil {
		return r
	}
	r := &Recorder{
		name:   name,
		dir:    dir,
		pushes: map[string]*pushCount{},
		counts: map[string]int64{},
		notes:  map[string][]string{},
	}
	recs[name] = r
	t.Cleanup(func() {
		r.mu.Lock()
		r.failed = t.Failed()
		r.mu.Unlock()
		if err := r.flush(); err != nil {
			t.Logf("scenario recorder: %v", err)
		}
		recMu.Lock()
		delete(recs, name)
		recMu.Unlock()
	})
	return r
}

// On reports whether r records: a scenario that must do work to have
// something to record (a read the test would not make) asks first.
func (r *Recorder) On() bool { return r != nil }

// attachBus is a new Bus on the cell; it returns the bus's ordinal.
func (r *Recorder) attachBus() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buses++
	return r.buses
}

// msg records one envelope the Bus delivered.
func (r *Recorder) msg(bus int, dir string, payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.flushed {
		return
	}
	r.raw = append(r.raw, fmt.Sprintf("%d %s %s", bus, dir, payload))
}

// Sent records one envelope a scenario delivers past the Bus (CATID's tick
// shipper hands Core's ingestor the production ticks itself), on route; its
// line's bus ordinal is 0.
func (r *Recorder) Sent(route string, payload []byte) {
	if r == nil {
		return
	}
	r.msg(0, route, payload)
}

// push records one bins push: who called it, whether it changed the copy the
// Edge holds (its digest differs from the push before), and its wall.
func (r *Recorder) push(caller string, changed bool, wall time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pc := r.pushes[caller]
	if pc == nil {
		pc = &pushCount{}
		r.pushes[caller] = pc
	}
	pc.N++
	if changed {
		pc.Changed++
	}
	pc.WallNS += int64(wall)
}

// Inc adds one to the cell's counter key.
func (r *Recorder) Inc(key string) { r.Add(key, 1) }

// Add adds n to the cell's counter key.
func (r *Recorder) Add(key string, n int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.counts[key] += n
	r.mu.Unlock()
}

// Note records value under key, in order: a fact the cell does not assert
// on, kept for a reader (S6's TriggeredBy).
func (r *Recorder) Note(key, value string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.notes[key] = append(r.notes[key], value)
	r.mu.Unlock()
}

// CoreStarted stamps Core's engine start: Core's robot poll ticks every
// protocol.RobotPollInterval from about then, which is what a wait on a cell
// whose fleet reports no poll is read against (inferred).
func (r *Recorder) CoreStarted() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.started.IsZero() {
		r.started = time.Now()
	}
	r.mu.Unlock()
}

// RobotPoll stamps one of Core's robot polls, from a fleet that sees it
// (deckFleet's GetRobotsStatus).
func (r *Recorder) RobotPoll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.polls = append(r.polls, time.Now())
	r.mu.Unlock()
}

// Wait is one wait a scenario records: an eventually-style loop's passes.
type Wait struct {
	r *Recorder

	Kind   string `json:"kind"`
	What   string `json:"what"`
	Passes int    `json:"passes"`
	Met    bool   `json:"met"`
	WallMS int64  `json:"wall_ms"`
	// AfterPoll: one of Core's robot polls fell inside the satisfying pass
	// (after the last check that failed, or the wait's start, and before the
	// check that held). PollSource says how that is known: "observed" (the
	// fleet saw each poll), "inferred" (from Core's start and the poll
	// interval) or "" (neither).
	AfterPoll  bool   `json:"after_poll"`
	PollSource string `json:"poll_source"`

	start, last, end time.Time
}

// Wait starts recording one wait of kind (the loop) for what (its label).
func (r *Recorder) Wait(kind, what string) *Wait {
	if r == nil {
		return nil
	}
	now := time.Now()
	return &Wait{r: r, Kind: kind, What: what, start: now, last: now}
}

// Missed stamps a check that did not hold.
func (w *Wait) Missed() {
	if w == nil {
		return
	}
	w.last = time.Now()
}

// Done ends the wait after passes passes; met is whether its condition held.
func (w *Wait) Done(passes int, met bool) {
	if w == nil {
		return
	}
	w.end = time.Now()
	w.Passes, w.Met = passes, met
	w.WallMS = w.end.Sub(w.start).Milliseconds()
	w.r.mu.Lock()
	w.r.waits = append(w.r.waits, w)
	w.r.mu.Unlock()
}

// pollIn says whether a Core robot poll fell in (from, to]; the caller holds
// r.mu.
func (r *Recorder) pollIn(from, to time.Time) (bool, string) {
	if len(r.polls) > 0 {
		for _, p := range r.polls {
			if p.After(from) && !p.After(to) {
				return true, "observed"
			}
		}
		return false, "observed"
	}
	if r.started.IsZero() {
		return false, ""
	}
	k := func(at time.Time) int64 { return int64(at.Sub(r.started) / protocol.RobotPollInterval) }
	return k(to) > k(from), "inferred"
}

// recordCounts is <cell>.counts.json.
type recordCounts struct {
	Cell     string `json:"cell"`
	Failed   bool   `json:"failed"`
	Buses    int    `json:"buses"`
	Messages int    `json:"messages"`
	// Transcript and Orders are the sha256 of the two views (buildViews).
	Transcript string `json:"transcript_sha256"`
	Orders     string `json:"orders_sha256"`
	// Timers is what the views leave out: the messages a wall-clock timer
	// sent, by subject (timerSubjects).
	Timers     map[string]int        `json:"timer_messages"`
	Pushes     map[string]*pushCount `json:"pushes"`
	Counts     map[string]int64      `json:"counts"`
	Notes      map[string][]string   `json:"notes"`
	RobotPolls int                   `json:"robot_polls_observed"`
	Waits      []*Wait               `json:"waits"`
}

// cellFile is the cell's name as a file name: "/" between a test and its
// subtests becomes "__", and anything outside [A-Za-z0-9_.-] becomes "_".
func cellFile(name string) string {
	name = strings.ReplaceAll(name, "/", "__")
	return unsafeFileChars.ReplaceAllString(name, "_")
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9_.\-]`)

func (r *Recorder) flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.flushed {
		return nil
	}
	r.flushed = true
	for _, w := range r.waits {
		if w.Met {
			w.AfterPoll, w.PollSource = r.pollIn(w.last, w.end)
		}
	}
	v := buildViews(r.raw)
	c := recordCounts{
		Cell: r.name, Failed: r.failed, Buses: r.buses, Messages: v.messages,
		Transcript: sha(v.transcript), Orders: sha(v.orders), Timers: v.timers,
		Pushes: r.pushes, Counts: r.counts, Notes: r.notes, RobotPolls: len(r.polls), Waits: r.waits,
	}
	counts, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	base := filepath.Join(r.dir, cellFile(r.name))
	for n := 2; ; n++ {
		if _, err := os.Stat(base + ".counts.json"); os.IsNotExist(err) {
			break
		}
		base = filepath.Join(r.dir, fmt.Sprintf("%s.r%d", cellFile(r.name), n))
	}
	for ext, body := range map[string]string{
		".transcript": v.transcript, ".orders": v.orders, ".raw": joinLines(r.raw), ".counts.json": string(counts) + "\n",
	} {
		if err := os.WriteFile(base+ext, []byte(body), 0o644); err != nil {
			return err
		}
	}
	r.raw = nil
	return nil
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// ── The two views ───────────────────────────────────────────────────────────
//
// A cell's messages are written two ways, both normalised (normaliser):
//
//   - <cell>.transcript: every message in the order the Bus delivered it.
//   - <cell>.orders: the same messages by order. Each order's messages, in
//     the order they were delivered, make one block; the blocks are sorted;
//     the messages that name no order follow, sorted too (the CATID delta
//     sink sends one message per bin in map order). Every
//     uuid, vendor id, digest and row id is masked to its kind ("<U>", "<V>",
//     "<H>", "<order_id>"), so a block says what happened to an order, not
//     which order it was.
//
// Why two. Two orders in flight at once reach the outbox in whichever order
// their goroutines write them (KSR, 2026-11-23: the keeper's refill, then
// decided on the bins ticker's goroutine, against the test's own pumps; the
// ticker is gone since, and Core's own goroutines, the fulfillment scan a bin
// move starts among them, still write beside the test's), so the
// delivered order interleaves them differently from run to run while every
// order's own sequence, and every message, is the same. The transcript sees
// that interleaving; the orders view does not. A cell whose orders view
// differs made a different order, or one of its orders went another way.
//
// Both leave out timerSubjects: their count is how long the cell lived.

// timerSubjects are the data subjects an engine sends on a wall-clock timer,
// whatever the cell does: the Edge's lineside report, every 60 s of wall time
// (shingo-edge/engine/lineside_reporter.go, linesideReportInterval; "a report
// that does not arrive changes no decision"). A slow run carries more of them.
var timerSubjects = map[string]bool{protocol.SubjectLinesideLevelReport: true}

type recordViews struct {
	transcript, orders string
	messages           int
	timers             map[string]int
}

// buildViews builds both views from the raw lines ("<bus> <route> <payload>").
func buildViews(raw []string) recordViews {
	v := recordViews{timers: map[string]int{}}
	n := newNormaliser()
	var lines, rest []string
	blocks := map[string][]string{}
	var keys []string // first seen first
	for _, l := range raw {
		bus, after, _ := strings.Cut(l, " ")
		route, payload, _ := strings.Cut(after, " ")
		if subject := dataSubject([]byte(payload)); timerSubjects[subject] {
			v.timers[subject]++
			continue
		}
		line := fmt.Sprintf("%s %s %s", bus, route, n.envelope([]byte(payload)))
		lines = append(lines, line)
		masked := maskLabels(line)
		key := orderLabel(line)
		if key == "" {
			rest = append(rest, masked)
			continue
		}
		if _, seen := blocks[key]; !seen {
			keys = append(keys, key)
		}
		blocks[key] = append(blocks[key], masked)
	}
	v.messages = len(lines)
	v.transcript = joinLines(lines)
	texts := make([]string, 0, len(keys))
	for _, k := range keys {
		texts = append(texts, "order:\n"+joinLines(blocks[k]))
	}
	sort.Strings(texts)
	v.orders = strings.Join(texts, "")
	if len(rest) > 0 {
		sort.Strings(rest)
		v.orders += "no order:\n" + joinLines(rest)
	}
	return v
}

// dataSubject is a data envelope's subject, "" for any other envelope.
func dataSubject(payload []byte) string {
	var env struct {
		Type string `json:"type"`
		P    struct {
			Subject string `json:"subject"`
		} `json:"p"`
	}
	if json.Unmarshal(payload, &env) != nil || env.Type != protocol.TypeData {
		return ""
	}
	return env.P.Subject
}

// orderLabel is the normalised order uuid a normalised line names (its
// payload's order_uuid, or a data body's), "" when it names none.
func orderLabel(line string) string {
	_, after, _ := strings.Cut(line, " ")
	_, body, _ := strings.Cut(after, " ")
	var env struct {
		P struct {
			OrderUUID string `json:"order_uuid"`
			Data      struct {
				OrderUUID string `json:"order_uuid"`
			} `json:"data"`
		} `json:"p"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return ""
	}
	if env.P.OrderUUID != "" {
		return env.P.OrderUUID
	}
	return env.P.Data.OrderUUID
}

var (
	labelOrdinal = regexp.MustCompile(`<([UVH])[0-9]+>`)
	keyOrdinal   = regexp.MustCompile(`<([A-Za-z_]+)#[0-9]+>`)
)

// maskLabels takes the ordinals off a normalised line's names: "<U7>" is
// "<U>", "<order_id#3>" is "<order_id>".
func maskLabels(line string) string {
	return keyOrdinal.ReplaceAllString(labelOrdinal.ReplaceAllString(line, "<$1>"), "<$1>")
}

// ── The normaliser ──────────────────────────────────────────────────────────
//
// What a transcript line keeps is every field of the envelope and its payload
// as sent, re-encoded with sorted keys. What it takes out is what differs
// between two runs of the same cell without the cell doing anything
// different:
//
//   - The envelope's id. It names nothing but the message, so it goes; the
//     cor of a reply, which names the message it answers by that id, becomes
//     "<re:TYPE>", the type of the message it answers ("<re:?>" for one the
//     cell's buses never carried).
//   - Times. Every string that parses as an RFC 3339 time (the envelope's ts
//     and exp, and every *_at in a payload) becomes "<T>": a stamp is the wall
//     clock's. The zero time stays as it is, because protocol.NoExpiry and an
//     unset stamp mean something.
//   - UUIDs. Every other string shaped as one (order uuids, origin ids)
//     becomes "<U1>", "<U2>", ... in order of first appearance in the cell, and
//     so does its first eight characters where prose quotes them ("partner
//     order bb27025d"): the same uuid keeps its name, so an order keeps its
//     identity across messages.
//   - Vendor order ids. Core names a fleet order sg-<order id>-<8 random hex>
//     (dispatch.VendorIDPrefix): each becomes "<V1>", ... by first appearance,
//     whole or quoted in prose.
//   - Row ids and sequence stamps. Every number under a key that names an id
//     ("id", or ending in "_id" or "_ids") or a sequence ("_seq": Core's
//     bin_placement_seq in placed_seq, a delta stream's as_of_seq) becomes
//     "<key#n>", numbered by first appearance under that key in the cell: a
//     sequence's next value depends on what else drew from it (another
//     order's placement, a rolled-back insert, how a sink batched), not on
//     the cell.
//   - Digests. A hex string of 32 or more characters (a sha256 over content
//     that carries stamps or ids) becomes "<H1>", ... by first appearance.
//   - Robot names. A robot_id becomes "<robot_id#n>" by first appearance: the
//     scenarios name their robots in the order they first drive an order
//     (ksrCell.robotFor counts them), so two orders dispatched in the other
//     order swap names without a robot doing anything different.
//
// Nothing else is taken out: a count, a status, a node name or a reason that
// differs between two runs is a difference in what the cell did. (A row id
// quoted in prose, "order 41", stays: it is a difference only when the rows
// were made in another order, and that is worth seeing.)
type normaliser struct {
	ids    map[string]map[string]int // key -> value -> ordinal
	uuid   map[string]int
	prefix map[string]int // a known uuid's first eight characters -> its ordinal
	vendor map[string]int
	hash   map[string]int
	types  map[string]string // envelope id -> its type, for a reply's cor
}

func newNormaliser() *normaliser {
	return &normaliser{ids: map[string]map[string]int{}, uuid: map[string]int{}, prefix: map[string]int{},
		vendor: map[string]int{}, hash: map[string]int{}, types: map[string]string{}}
}

var (
	uuidShape   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hashShape   = regexp.MustCompile(`^[0-9a-f]{32,}$`)
	vendorShape = regexp.MustCompile(`sg-[0-9]+-[0-9a-f]{8}\b`)
	hex8        = regexp.MustCompile(`\b[0-9a-f]{8}\b`)
	uuidInProse = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
)

// envelope is one transcript line's body: the envelope normalised, or, for
// bytes that do not decode as JSON, their length and sha256.
func (n *normaliser) envelope(payload []byte) string {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		sum := sha256.Sum256(payload)
		return fmt.Sprintf("undecodable len=%d sha256=%s", len(payload), hex.EncodeToString(sum[:]))
	}
	if env, ok := v.(map[string]any); ok {
		if id, ok := env["id"].(string); ok {
			typ, _ := env["type"].(string)
			n.types[id] = typ
			delete(env, "id")
		}
		if cor, ok := env["cor"].(string); ok {
			if typ, seen := n.types[cor]; seen {
				env["cor"] = "<re:" + typ + ">"
			} else {
				env["cor"] = "<re:?>"
			}
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // keep "<T>" as written: the transcript is read by people
	if err := enc.Encode(n.value("", v)); err != nil {
		return "unencodable: " + err.Error()
	}
	return strings.TrimSuffix(out.String(), "\n")
}

func (n *normaliser) value(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		// Sorted, so first appearance is the same order on every run.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out[k] = n.value(k, x[k])
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = n.value(key, e)
		}
		return out
	case json.Number:
		if isIDKey(key) {
			return n.ordinal(key, x.String())
		}
		return x
	case string:
		if key == "robot_id" {
			return n.ordinal(key, x)
		}
		return n.str(x)
	default:
		return v
	}
}

func isIDKey(key string) bool {
	k := strings.ToLower(key)
	return k == "id" || strings.HasSuffix(k, "_id") || strings.HasSuffix(k, "_ids") || strings.HasSuffix(k, "_seq")
}

func (n *normaliser) ordinal(key, v string) string {
	m := n.ids[key]
	if m == nil {
		m = map[string]int{}
		n.ids[key] = m
	}
	o, ok := m[v]
	if !ok {
		o = len(m) + 1
		m[v] = o
	}
	return fmt.Sprintf("<%s#%d>", key, o)
}

func (n *normaliser) str(s string) string {
	if uuidShape.MatchString(s) {
		return n.uuidLabel(s)
	}
	if hashShape.MatchString(s) {
		o, ok := n.hash[s]
		if !ok {
			o = len(n.hash) + 1
			n.hash[s] = o
		}
		return fmt.Sprintf("<H%d>", o)
	}
	if len(s) >= 20 && s[4] == '-' && s[7] == '-' {
		if at, err := time.Parse(time.RFC3339Nano, s); err == nil {
			if at.IsZero() {
				return s
			}
			return "<T>"
		}
	}
	// Prose: a vendor order id, then a known uuid quoted whole or by its
	// first eight characters.
	if strings.Contains(s, "sg-") {
		s = vendorShape.ReplaceAllStringFunc(s, func(v string) string {
			o, ok := n.vendor[v]
			if !ok {
				o = len(n.vendor) + 1
				n.vendor[v] = o
			}
			return fmt.Sprintf("<V%d>", o)
		})
	}
	s = uuidInProse.ReplaceAllStringFunc(s, n.uuidLabel)
	if len(n.prefix) > 0 {
		s = hex8.ReplaceAllStringFunc(s, func(h string) string {
			if o, ok := n.prefix[h]; ok {
				return fmt.Sprintf("<U%d>", o)
			}
			return h
		})
	}
	return s
}

// uuidLabel is u's name in the cell, given at its first appearance.
func (n *normaliser) uuidLabel(u string) string {
	u = strings.ToLower(u)
	o, ok := n.uuid[u]
	if !ok {
		o = len(n.uuid) + 1
		n.uuid[u] = o
		n.prefix[u[:8]] = o
	}
	return fmt.Sprintf("<U%d>", o)
}
