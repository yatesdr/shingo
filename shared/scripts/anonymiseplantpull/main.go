// Command anonymiseplantpull derives a committable fixture from a real plant
// pull.
//
// The fixtures under shared/scenefixtures/ used to be the pulls themselves,
// which put customer part numbers, PLC tag names, pull timestamps and the
// plant's floor map into the repository. Owner ruling 2026-09-13: no plant
// data or information in the repo. The reviewers were also right that a
// hand-typed fixture "gets the join right by accident" — a scene small enough
// to type is one in which both the `instance_name` join and the `label` join
// work, and a test on it cannot tell them apart. So this derives a SYNTHETIC
// fixture from a real pull, keeping every property the tests rely on and none
// of the plant's facts.
//
// THIS PROGRAM CARRIES NO DATA. Its input lives outside the repo — the design
// folder in the GitHub root, whose README records the hosts and the method.
// Its output is what gets committed. From the repo root:
//
//	go run ./shared/scripts/anonymiseplantpull \
//	    --in  ../hmi-flow-composer-design-2026-09-02/data/hk-press-setups-2026-09-03.json \
//	    --out shared/scenefixtures/a-press-setups.json --tag A
//
// DETERMINISTIC, AND THE TRANSFORM IS IN HERE, NOT ON THE COMMAND LINE. Every
// rename is a pure function of the source value and the plant tag, and each
// tag's rigid motion is a constant in transforms below, so the invocation
// above is the whole invocation and a regenerated fixture is a no-op diff. The
// first cut took --rotate/--mirror/--tx/--ty as flags, which meant the bytes
// in the repo could only be reproduced by someone who remembered four numbers
// nothing recorded. There is no RNG.
//
// WHAT IS PRESERVED, and why each one matters:
//
//   - Row counts, ids, and every structural relation — style_id, process_id,
//     operator_station_id, parent_name, the edge from/to join on
//     instance_name.
//   - WHICH scene points carry a `label`. Hopkinsville populates 60 of 383
//     and Springfield 12 of 350, which is the trap the fixtures exist for: a
//     node->map join on `label` looks right at one plant and renders the
//     other empty. Renaming a label must never create or destroy one.
//   - The vendor's class strings verbatim (ActionPoint, LocationMark,
//     DegenerateBezier, StraightPath, BezierPath) and the bezier handles. 494
//     of Springfield's 588 segments say DegenerateBezier and only the handles
//     say which of them bow.
//   - The dirty rows: claims whose style is gone, claims on soft-deleted
//     styles, the claim whose process row is gone. A naive join drops them and
//     a join that assumes success throws; the backfill must do neither.
//   - Relative timestamp ORDER, shifted to a fixed epoch.
//   - Node names (PLN_01, SMN_BUF_100, Supermarket Area), the plant tags, and
//     every mode/role/state enum. These are shingo's own vocabulary, not the
//     plant's — owner ruling, same day.
//
// WHAT IS REPLACED:
//
//   - Part numbers and payload codes, and the style names shaped like them.
//   - Process, station and group names and descriptions.
//   - PLC names and tag names, and expected CATIDs.
//   - Every timestamp, and the plant's own `pulled_at`.
//   - THE FLOOR PLAN, by one rigid motion per plant.
//
// WHAT IS DROPPED, rather than rewritten:
//
//   - `scene_points.properties_json`. It is the vendor's per-point blob and it
//     carries the plant three ways at once: `bindRobotMap` names the site's own
//     .smap file on all 733 points, a `binTask` names its carrier recfile, and
//     a LocationMark's `points` key holds the corner polygon in RAW,
//     UNTRANSFORMED coordinates — which would have handed back the floor plan
//     the transform exists to take away. Nothing in the tree reads the column,
//     so it goes rather than being rewritten key by key. The self-check looks
//     inside it, so re-adding the column fails the run instead of shipping.
//
// THE TRANSFORM IS A RIGID MOTION — mirror, then rotate, then translate; scale
// 1. An affine with a scale would satisfy "straight stays straight and a
// DegenerateBezier keeps bowing the same way" just as well, but it would change
// every DISTANCE, and distances are load-bearing: operator-flow.test.js pins
// PLN_01 to PLN_02 at 1.772 m, the picture draws positions at true spacing,
// and the key-route walk sums real edge lengths. A rigid motion keeps all of
// that exactly while making the map not the map. Headings (`dir`) rotate with
// it, and mirroring negates them, so a point still faces the way its geometry
// says.
//
// THE OUTPUT FORMAT IS BYTE-STABLE: shared/scripts/internal/fixturejson keeps
// column order, integers apart from floats, and the float spelling the
// committed fixtures carry. Exit 0 = written; 1 = refused or failed; 2 = bad
// arguments.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"shingo/shared/scripts/internal/fixturejson"
)

type value = fixturejson.Value

// transform is one plant tag's rigid motion: rotate degrees, mirror in x,
// translate. Scale is 1 by construction.
type transform struct {
	rotate float64
	mirror bool
	tx, ty float64
}

// transforms are the numbers the committed fixtures were derived with;
// changing one rewrites that plant's whole map, which is a fixture re-pin, not
// a tweak.
//
// THE MOTION MAY NOT TOUCH y, and that is not an aesthetic choice. The station
// picture places cards from the plant's own coordinates and then clusters them
// into a FRONT row and a BACK row on screen y (operator-flow.js rowsOf, through
// an unrotated projector), so the map's ORIENTATION is load-bearing where its
// position is not. Both alternatives were tried and both broke a rule the
// fixture exists to hold:
//
//   - 37 degrees: a press's two rows come out diagonal, no legible scale keeps
//     the cards apart, and the picture drops to "positions not to scale" — the
//     fixture silently no longer exercising the placed path.
//   - 180 degrees (or any y flip): the rows survive and SWAP. PLN_01 is Kind
//     `front` and is DRAWN in the BACK row at this plant, which is F2's whole
//     point; upside down it draws in the front row and the shot harness fails
//     on the caption.
//   - 90 degrees turns the rows into columns.
//
// So: mirror in x, translate. A rigid motion cannot change the shape anyway —
// that is what preserving distances MEANS — so what the transform can take
// away is the absolute position and the handedness, and it takes both.
var transforms = map[string]transform{
	"A": {0.0, true, 120.0, -45.0},
	"B": {0.0, true, -60.0, 210.0},
}

// epoch is the fixed date every shifted timestamp lands on.
const epoch = "2026-01-05T"

// note is the fixture's own provenance line.
const note = "Synthetic. Derived from a real plant pull by " +
	"shared/scripts/anonymiseplantpull; the pull is held outside the repo. " +
	"Structure, counts, joins, label sparsity, bezier handles and dirty rows " +
	"are the plant's; part numbers, names, PLC tags, timestamps and the map " +
	"are not."

// stampKeys is EVERY column whose values are dates, enumerated from both pulls
// rather than guessed: "last_seen" was in this list and the column is
// "last_seen_at", so an operator station carried the pull's own clock time
// straight into the fixture.
var stampKeys = map[string]bool{
	"created_at": true, "updated_at": true, "deleted_at": true, "synced_at": true,
	"started_at": true, "completed_at": true, "below_reorder_since": true,
	"last_seen_at": true, "archived_at": true,
}

// h is a stable small integer for (tag, kind, value).
func h(tag, kind, v string, mod uint64) uint64 {
	d := sha256.Sum256([]byte(tag + "\x00" + kind + "\x00" + v))
	return binary.BigEndian.Uint64(d[:8]) % mod
}

// partCode is an invented part number: SYN-<tag>-<kind><n>, e.g. SYN-A-P007.
//
// NOT SHAPED LIKE A REAL PART NUMBER, and it used to be. It returned the
// plants' own format, on the reasoning that a fixture should exercise the
// shapes the plants use. Nothing downstream cares — the only code that
// inspects a part string is composer-model's shortPart, which strips a
// `PIA<n>`/`Payload` suffix and is a no-op on every other shape — and it
// defeated the point of the exercise: a committed fixture, a test literal and
// a handoff shot all then read like a customer's part number. A synthetic
// value that is indistinguishable from the real thing is not anonymised, it is
// unlabelled.
//
// SEQUENTIAL, NOT HASHED. Distinctness is guaranteed rather than probable — the
// B fixture has 242 part codes, and 242 draws from a four-digit hash collide
// better than half the time — and the numbers are readable in a diff and in a
// shot. Stability is per RUN, which is all anything needs: the anonymiser
// holds one invented code per real one for the whole document, and a new pull
// replaces the fixture wholesale.
func partCode(tag, kind string, n int) string {
	return fmt.Sprintf("SYN-%s-%s%03d", tag, kind, n)
}

// styleName is a style name. Real ones are part numbers or a word; both become
// a code. S AND P ARE SEPARATE NUMBER LINES so a style name and a payload code
// can never read as the same thing.
func styleName(tag string, n int) string {
	return "PART " + partCode(tag, "S", n)
}

type anonymiser struct {
	tag    string
	theta  float64
	mirror float64
	tx, ty float64
	parts  map[string]string // real payload code -> invented
	styles map[string]string // real style name -> invented
	names  map[string]string // real process/station/group name -> invented
	nameN  int
	times  map[string]string // real timestamp -> invented, order preserved
}

func newAnonymiser(tag string, t transform) *anonymiser {
	m := 1.0
	if t.mirror {
		m = -1.0
	}
	return &anonymiser{
		tag: tag, theta: t.rotate * (math.Pi / 180.0), mirror: m, tx: t.tx, ty: t.ty,
		parts: map[string]string{}, styles: map[string]string{}, names: map[string]string{},
		times: map[string]string{},
	}
}

// key is a scalar's identity for the rename maps: its kind and its text.
func key(v *value) string {
	return fmt.Sprintf("%d\x00%s", v.Kind, fixturejson.Compact(v, false))
}

func (a *anonymiser) part(v *value) (*value, error) {
	if v.IsEmptyOrNull() {
		return v, nil
	}
	if v.Kind != fixturejson.String {
		return nil, fmt.Errorf("a payload code is not a string: %s", fixturejson.Compact(v, false))
	}
	// Sentinels and obvious test rows are shingo's, not the plant's.
	if strings.HasPrefix(v.S, "__") || v.S == "Test-Payload" || v.S == "Core_Test" {
		return v, nil
	}
	if _, ok := a.parts[v.S]; !ok {
		a.parts[v.S] = partCode(a.tag, "P", len(a.parts)+1)
	}
	return fixturejson.Str(a.parts[v.S]), nil
}

func (a *anonymiser) style(v *value) *value {
	if v.IsEmptyOrNull() {
		return v
	}
	k := key(v)
	if _, ok := a.styles[k]; !ok {
		a.styles[k] = styleName(a.tag, len(a.styles)+1)
	}
	return fixturejson.Str(a.styles[k])
}

// label is a process / station / group name.
//
// THE PLANT TAG IS IN THE NAME so the scheme cannot collide with a real one. It
// used to be "Press 1", "Press 2" — and Springfield really does run presses
// called "Press 4" and "Press 6", so two of the invented names came out
// identical to the ones they were replacing. The self-check caught it, which
// is the reason that check compares whole values rather than trusting the
// rename.
func (a *anonymiser) label(kind string, v *value) *value {
	if v.IsEmptyOrNull() {
		return v
	}
	k := key(v)
	if _, ok := a.names[k]; !ok {
		a.nameN++
		a.names[k] = fmt.Sprintf("%s %s%d", kind, a.tag, a.nameN)
	}
	return fixturejson.Str(a.names[k])
}

// when records a timestamp; finishTimes assigns the shifted stamps once every
// one is known, so ORDER survives — `recent changeovers` sorts on it and the
// claim round-trip compares before and after — and the real dates do not.
func (a *anonymiser) when(v *value) error {
	if v.IsEmptyOrNull() {
		return nil
	}
	if v.Kind != fixturejson.String {
		return fmt.Errorf("a timestamp is not a string: %s", fixturejson.Compact(v, false))
	}
	if _, ok := a.times[v.S]; !ok {
		a.times[v.S] = ""
	}
	return nil
}

func (a *anonymiser) finishTimes() {
	real := make([]string, 0, len(a.times))
	for t := range a.times {
		real = append(real, t)
	}
	sort.Strings(real)
	// One minute apart from the epoch, keeping the order and nothing else.
	for i, t := range real {
		a.times[t] = fmt.Sprintf("%s%02d:%02d:00Z", epoch, (i/60)%24, i%60)
	}
}

func (a *anonymiser) rewriteTime(v *value) *value {
	if v.Kind != fixturejson.String || v.S == "" {
		return v
	}
	if t, ok := a.times[v.S]; ok {
		return fixturejson.Str(t)
	}
	return v
}

// xy moves a point. The conversions to float64 keep each product rounded on
// its own, so no platform fuses a multiply-add and moves a bit.
func (a *anonymiser) xy(x, y *value) (*value, *value, error) {
	if x.IsNull() || y.IsNull() {
		return x, y, nil
	}
	fx, err := x.Float64()
	if err != nil {
		return nil, nil, err
	}
	fy, err := y.Float64()
	if err != nil {
		return nil, nil, err
	}
	c, s := math.Cos(a.theta), math.Sin(a.theta)
	mx := fx * a.mirror
	rx := float64(mx*c) - float64(fy*s)
	ry := float64(mx*s) + float64(fy*c)
	return fixturejson.Num(fixturejson.Round(rx+a.tx, 3)), fixturejson.Num(fixturejson.Round(ry+a.ty, 3)), nil
}

// heading turns a `dir` (radians) with the map.
func (a *anonymiser) heading(v *value) (*value, error) {
	if v.IsNull() {
		return v, nil
	}
	d, err := v.Float64()
	if err != nil {
		return nil, err
	}
	if a.mirror < 0 {
		d = math.Pi - d
	}
	return fixturejson.Num(fixturejson.Round(floorMod(d+a.theta, 2*math.Pi), 6)), nil
}

// floorMod is a modulo whose result takes the divisor's sign.
func floorMod(x, y float64) float64 {
	m := math.Mod(x, y)
	if m == 0 {
		return math.Copysign(0, y)
	}
	if (m < 0) != (y < 0) {
		m += y
	}
	return m
}

// table is a named array of row objects.
func table(doc *fixturejson.Obj, side, name string) ([]*fixturejson.Obj, error) {
	v, ok := doc.Get(name)
	if !ok {
		return nil, fmt.Errorf("the pull has no %s.%s", side, name)
	}
	return objRows(v, side+"."+name)
}

func objRows(v *value, what string) ([]*fixturejson.Obj, error) {
	if v.Kind != fixturejson.Array {
		return nil, fmt.Errorf("%s is not an array", what)
	}
	out := make([]*fixturejson.Obj, 0, len(v.A))
	for _, r := range v.A {
		if r.Kind != fixturejson.Object {
			return nil, fmt.Errorf("%s has a row that is not an object", what)
		}
		out = append(out, r.O)
	}
	return out, nil
}

// rows copies rows, rewriting every timestamp. Column order is preserved so
// the output diffs against the source row by row.
func (a *anonymiser) rows(src []*fixturejson.Obj) []*fixturejson.Obj {
	out := make([]*fixturejson.Obj, 0, len(src))
	for _, r := range src {
		o := fixturejson.NewObj()
		for _, k := range r.Keys() {
			v, _ := r.Get(k)
			if stampKeys[k] {
				v = a.rewriteTime(v)
			}
			o.Set(k, v)
		}
		out = append(out, o)
	}
	return out
}

func sides(src *value) (edge, core *fixturejson.Obj, err error) {
	if src.Kind != fixturejson.Object {
		return nil, nil, errors.New("the pull is not an object")
	}
	e, ok1 := src.O.Get("edge")
	c, ok2 := src.O.Get("core")
	if !ok1 || !ok2 || e.Kind != fixturejson.Object || c.Kind != fixturejson.Object {
		return nil, nil, errors.New("the pull needs an `edge` object and a `core` object")
	}
	return e.O, c.O, nil
}

// collectTimes walks every table of both sides so the order survives the
// shift, and the pull's own pulled_at with them.
func (a *anonymiser) collectTimes(src *value, edge, core *fixturejson.Obj) error {
	for _, side := range []*fixturejson.Obj{edge, core} {
		for _, name := range side.Keys() {
			t, _ := side.Get(name)
			if t.Kind != fixturejson.Array || len(t.A) == 0 || t.A[0].Kind != fixturejson.Object {
				continue
			}
			rows, err := objRows(t, name)
			if err != nil {
				return err
			}
			for _, r := range rows {
				for _, k := range r.Keys() {
					if v, _ := r.Get(k); stampKeys[k] && v.Truthy() {
						if err := a.when(v); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	if err := a.when(src.O.GetOrNull("pulled_at")); err != nil {
		return err
	}
	a.finishTimes()
	return nil
}

func (a *anonymiser) processes(edge *fixturejson.Obj) ([]*fixturejson.Obj, error) {
	src, err := table(edge, "edge", "processes")
	if err != nil {
		return nil, err
	}
	out := a.rows(src)
	for _, p := range out {
		name := a.label("Press", p.GetOrNull("name"))
		p.Set("name", name)
		if p.GetOrNull("description").Truthy() {
			p.Set("description", name)
		}
		// A PLC struct path and its tag are the plant's naming, not shingo's.
		if p.GetOrNull("counter_plc_name").Truthy() {
			if name.Kind != fixturejson.String {
				return nil, errors.New("a process with a PLC name has no name")
			}
			p.Set("counter_plc_name", fixturejson.Str("PLC-"+strings.ReplaceAll(name.S, " ", "-")))
		}
		if p.GetOrNull("counter_tag_name").Truthy() {
			if name.Kind != fixturejson.String {
				return nil, errors.New("a process with a PLC tag has no name")
			}
			p.Set("counter_tag_name", fixturejson.Str("MES_"+strings.ReplaceAll(name.S, " ", "_")+".Prod_Counter_01"))
		}
	}
	return out, nil
}

func (a *anonymiser) stations(edge *fixturejson.Obj) ([]*fixturejson.Obj, error) {
	src, err := table(edge, "edge", "operator_stations")
	if err != nil {
		return nil, err
	}
	out := a.rows(src)
	for _, s := range out {
		name := a.label("Screen", s.GetOrNull("name"))
		s.Set("name", name)
		if s.GetOrNull("code").Truthy() {
			if name.Kind != fixturejson.String {
				return nil, errors.New("a station with a code has no name")
			}
			s.Set("code", fixturejson.Str(strings.ReplaceAll(strings.ToLower(name.S), " ", "-")))
		}
		// A free-text note an engineer typed at the plant.
		if s.GetOrNull("note").Truthy() {
			s.Set("note", fixturejson.Str(""))
		}
	}
	return out, nil
}

// catid is a CATID, the PLC's number for a style or a part: kept numeric and
// distinct, its value replaced.
func (a *anonymiser) catid(v *value) (*value, error) {
	t, err := v.Text()
	if err != nil {
		return nil, err
	}
	return fixturejson.Str(fmt.Sprint(1000 + h(a.tag, "catid", t, 9000))), nil
}

func (a *anonymiser) styleRows(edge *fixturejson.Obj) ([]*fixturejson.Obj, error) {
	src, err := table(edge, "edge", "styles")
	if err != nil {
		return nil, err
	}
	out := a.rows(src)
	for _, s := range out {
		name := a.style(s.GetOrNull("name"))
		s.Set("name", name)
		if s.GetOrNull("description").Truthy() {
			s.Set("description", name)
		}
		if c := s.GetOrNull("expected_catid"); c.Truthy() {
			v, err := a.catid(c)
			if err != nil {
				return nil, err
			}
			s.Set("expected_catid", v)
		}
	}
	return out, nil
}

func (a *anonymiser) partList(vs []*value) (*value, error) {
	out := make([]*value, 0, len(vs))
	for _, x := range vs {
		p, err := a.part(x)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return fixturejson.Arr(out...), nil
}

func (a *anonymiser) claims(edge *fixturejson.Obj) ([]*fixturejson.Obj, error) {
	src, err := table(edge, "edge", "style_node_claims")
	if err != nil {
		return nil, err
	}
	out := a.rows(src)
	for _, c := range out {
		p, err := a.part(c.GetOrNull("payload_code"))
		if err != nil {
			return nil, err
		}
		c.Set("payload_code", p)
		apc := c.GetOrNull("allowed_payload_codes")
		switch {
		case apc.Kind == fixturejson.Array:
			l, err := a.partList(apc.A)
			if err != nil {
				return nil, err
			}
			c.Set("allowed_payload_codes", l)
		case apc.Kind == fixturejson.String && strings.HasPrefix(apc.S, "["):
			inner, err := fixturejson.Parse([]byte(apc.S))
			if err != nil {
				return nil, fmt.Errorf("allowed_payload_codes: %w", err)
			}
			if inner.Kind != fixturejson.Array {
				return nil, errors.New("allowed_payload_codes is not a list")
			}
			l, err := a.partList(inner.A)
			if err != nil {
				return nil, err
			}
			c.Set("allowed_payload_codes", fixturejson.Str(fixturejson.Spaced(l, true)))
		}
	}
	return out, nil
}

// catalog rewrites the payload catalog. A CATALOG ROW HAS THREE PLACES TO PUT A
// PART NUMBER, and the trimmed copy that used to be committed carried only two
// of them. `description` is the part's ENGINEERING NAME at Springfield, which
// is customer data of a different shape and slipped straight through a
// code-shaped rename. Everything that is not the code becomes the code.
func (a *anonymiser) catalog(edge *fixturejson.Obj) ([]*fixturejson.Obj, error) {
	src, err := table(edge, "edge", "payload_catalog")
	if err != nil {
		return nil, err
	}
	out := a.rows(src)
	for _, e := range out {
		code, err := a.part(e.GetOrNull("code"))
		if err != nil {
			return nil, err
		}
		e.Set("code", code)
		for _, k := range []string{"name", "description"} {
			if _, ok := e.Get(k); ok {
				e.Set(k, code)
			}
		}
		// The PLC's number for the part, same as a style's expected_catid.
		if c := e.GetOrNull("catid"); !c.IsEmptyOrNull() {
			v, err := a.catid(c)
			if err != nil {
				return nil, err
			}
			e.Set("catid", v)
		}
	}
	return out, nil
}

func (a *anonymiser) payloads(core *fixturejson.Obj) ([]*fixturejson.Obj, error) {
	src, err := table(core, "core", "payloads")
	if err != nil {
		return nil, err
	}
	out := a.rows(src)
	for _, p := range out {
		code, err := a.part(p.GetOrNull("code"))
		if err != nil {
			return nil, err
		}
		p.Set("code", code)
		for _, k := range []string{"name", "description"} {
			if _, ok := p.Get(k); ok {
				p.Set(k, code)
			}
		}
	}
	return out, nil
}

func (a *anonymiser) points(core *fixturejson.Obj) ([]*fixturejson.Obj, error) {
	src, err := table(core, "core", "scene_points")
	if err != nil {
		return nil, err
	}
	out := a.rows(src)
	for _, p := range out {
		x, y, err := a.xy(p.GetOrNull("pos_x"), p.GetOrNull("pos_y"))
		if err != nil {
			return nil, err
		}
		p.Set("pos_x", x)
		p.Set("pos_y", y)
		d, err := a.heading(p.GetOrNull("dir"))
		if err != nil {
			return nil, err
		}
		p.Set("dir", d)
		// THE LABEL IS A NODE NAME WHERE IT IS POPULATED, and node names stay.
		// Whether it is populated is the fixture's whole point, so the empty
		// string stays the empty string.
		//
		// properties_json GOES ENTIRELY — the vendor blob, three leaks deep.
		// Nothing reads it. See the package comment.
		p.Delete("properties_json")
	}
	return out, nil
}

func (a *anonymiser) edges(core *fixturejson.Obj) ([]*fixturejson.Obj, error) {
	src, err := table(core, "core", "scene_edges")
	if err != nil {
		return nil, err
	}
	out := a.rows(src)
	for _, e := range out {
		for _, p := range []string{"from", "to", "ctrl1", "ctrl2"} {
			x, y, err := a.xy(e.GetOrNull(p+"_x"), e.GetOrNull(p+"_y"))
			if err != nil {
				return nil, err
			}
			e.Set(p+"_x", x)
			e.Set(p+"_y", y)
		}
	}
	return out, nil
}

// field is one top-level entry of the fixture, in output order.
type field struct {
	key  string
	val  *value
	rows []*fixturejson.Obj
	// isTable marks rows as the value, written one row per line.
	isTable bool
}

// anonymise builds the fixture. The order of the steps is load-bearing: part
// codes are numbered in the order they are first met.
func anonymise(src *value, tag string) ([]field, error) {
	edge, core, err := sides(src)
	if err != nil {
		return nil, err
	}
	a := newAnonymiser(tag, transforms[tag])
	if err := a.collectTimes(src, edge, core); err != nil {
		return nil, err
	}
	steps := []struct {
		key string
		run func() ([]*fixturejson.Obj, error)
	}{
		{"processes", func() ([]*fixturejson.Obj, error) { return a.processes(edge) }},
		{"operator_stations", func() ([]*fixturejson.Obj, error) { return a.stations(edge) }},
		{"styles", func() ([]*fixturejson.Obj, error) { return a.styleRows(edge) }},
		{"style_node_claims", func() ([]*fixturejson.Obj, error) { return a.claims(edge) }},
		{"payload_catalog", func() ([]*fixturejson.Obj, error) { return a.catalog(edge) }},
		{"payloads", func() ([]*fixturejson.Obj, error) { return a.payloads(core) }},
		{"scene_points", func() ([]*fixturejson.Obj, error) { return a.points(core) }},
		{"scene_edges", func() ([]*fixturejson.Obj, error) { return a.edges(core) }},
	}
	built := map[string][]*fixturejson.Obj{}
	for _, s := range steps {
		r, err := s.run()
		if err != nil {
			return nil, err
		}
		built[s.key] = r
	}
	copied := func(side *fixturejson.Obj, sideName, name string) ([]*fixturejson.Obj, error) {
		r, err := table(side, sideName, name)
		if err != nil {
			return nil, err
		}
		return a.rows(r), nil
	}
	for _, c := range []struct {
		side     *fixturejson.Obj
		sideName string
		name     string
	}{{edge, "edge", "process_nodes"}, {core, "core", "nodes"}, {core, "core", "payload_bin_types"}} {
		r, err := copied(c.side, c.sideName, c.name)
		if err != nil {
			return nil, err
		}
		built[c.name] = r
	}

	out := []field{
		{key: "plant", val: fixturejson.Str(tag)},
		{key: "note", val: fixturejson.Str(note)},
	}
	for _, k := range []string{
		"processes", "operator_stations", "process_nodes", "styles", "style_node_claims",
		"payload_catalog", "scene_points", "scene_edges", "nodes", "payloads", "payload_bin_types",
	} {
		out = append(out, field{key: k, rows: built[k], isTable: true})
	}
	return out, nil
}

// vocabularyKeys are fields that keep the plant's word by owner ruling: node
// names are shingo's vocabulary (PLN_01, SMN_BUF_100, Supermarket Area, and the
// line designators that are node names too), and the vendor's class and point
// identifiers are the vendor's. A sensitive string may appear in one of these.
var vocabularyKeys = map[string]bool{
	"core_node_name": true, "paired_core_node": true, "second_paired_core_node": true,
	"inbound_source": true, "inbound_staging": true, "outbound_staging": true, "outbound_source": true,
	"outbound_destination": true, "changeover_evac_destination": true, "changeover_evac_nodes": true,
	"release_node": true, "staging_node": true, "inbound_source_node": true, "outbound_source_node": true,
	"inbound_source_node_group": true, "outbound_source_node_group": true,
	"name": true, "parent_name": true, "label": true, "instance_name": true, "from_name": true, "to_name": true,
	"class_name": true, "area_name": true, "point_name": true, "node_type_code": true, "code": true,
}

// vocabularyTables are the tables in which a vocabulary key's value may repeat
// a sensitive string.
var vocabularyTables = map[string]bool{
	"nodes": true, "process_nodes": true, "scene_points": true, "scene_edges": true, "style_node_claims": true,
}

// sensitiveStrings is every string in the pull that must not survive into the
// fixture.
//
// AN OUTPUT CHECK, NOT AN INPUT LIST. The rename works column by column, and
// the pull has more columns than anyone holds in their head: the first run left
// payload_catalog.description untouched, which at Springfield is the part's
// engineering name — customer data of a shape no part-number regex matches. So
// the program checks its own output against its own input, and a column nobody
// thought of fails the run instead of shipping.
//
// Node names, vendor class strings, enums and ids are deliberately NOT here:
// they are shingo's vocabulary and the vendor's, and they stay by owner ruling.
func sensitiveStrings(src *value) map[string]bool {
	out := map[string]bool{}
	edge, core, _ := sides(src)
	addStr := func(v *value) {
		if v != nil && v.Kind == fixturejson.String && utf8.RuneCountInString(v.S) > 3 {
			out[v.S] = true
		}
	}
	add := func(side *fixturejson.Obj, name string, keys ...string) {
		t, ok := side.Get(name)
		if !ok || t.Kind != fixturejson.Array {
			return
		}
		for _, r := range t.A {
			if r.Kind != fixturejson.Object {
				continue
			}
			for _, k := range keys {
				addStr(r.O.GetOrNull(k))
			}
		}
	}
	add(edge, "payload_catalog", "code", "name", "description")
	add(core, "payloads", "code", "name", "description")
	add(edge, "styles", "name", "description", "expected_catid")
	add(edge, "processes", "name", "description", "counter_plc_name", "counter_tag_name")
	add(edge, "operator_stations", "name", "note")
	add(edge, "style_node_claims", "payload_code", "auto_request_payload")
	addStr(src.O.GetOrNull("pulled_at"))
	// THE VENDOR BLOB, flattened into the same set. properties_json is dropped,
	// so nothing here can survive — which is the point: if someone puts the
	// column back, the .smap name and the untransformed corner polygons fail the
	// run instead of shipping. Its `label` key is skipped: that one holds the
	// node name, which stays by owner ruling and is a value the fixture is
	// REQUIRED to keep.
	if t, ok := core.Get("scene_points"); ok && t.Kind == fixturejson.Array {
		for _, p := range t.A {
			if p.Kind != fixturejson.Object {
				continue
			}
			pj := p.O.GetOrNull("properties_json")
			if pj.Kind != fixturejson.Array {
				continue
			}
			for _, kv := range pj.A {
				if kv.Kind != fixturejson.Object {
					continue
				}
				if k := kv.O.GetOrNull("key"); k.Kind == fixturejson.String && vocabularyKeys[k.S] {
					continue
				}
				addStr(kv.O.GetOrNull("stringValue"))
			}
		}
	}
	// Shingo's own sentinels, which are not the plant's to take away.
	delete(out, "Test-Payload")
	delete(out, "Core_Test")
	return out
}

// leakedValues returns the sensitive source strings that survive as a WHOLE
// field value, with the paths they sit at.
//
// Whole values, not substrings. A substring scan reports every invented
// "Press A41" that contains a real "Press 4" and hides the ones that matter
// underneath them.
func leakedValues(fields []field, sensitive map[string]bool) map[string]map[string]bool {
	hits := map[string]map[string]bool{}
	var walk func(v *value, key, path string)
	walk = func(v *value, key, path string) {
		switch v.Kind {
		case fixturejson.Object:
			for _, k := range v.O.Keys() {
				x, _ := v.O.Get(k)
				walk(x, k, path+"."+k)
			}
		case fixturejson.Array:
			for _, x := range v.A {
				walk(x, key, path+"[]")
			}
		case fixturejson.String:
			if !sensitive[v.S] {
				return
			}
			// The table is the path's first component without its list marker:
			// ".scene_points[].label" is in scene_points.
			tbl := ""
			if strings.Contains(path[1:], ".") {
				tbl = strings.TrimSuffix(strings.Split(path, ".")[1], "[]")
			}
			if vocabularyKeys[key] && vocabularyTables[tbl] {
				return
			}
			if hits[v.S] == nil {
				hits[v.S] = map[string]bool{}
			}
			hits[v.S][path] = true
		}
	}
	for _, f := range fields {
		walk(f.value(), f.key, "."+f.key)
	}
	return hits
}

func (f field) value() *value {
	if !f.isTable {
		return f.val
	}
	vs := make([]*value, 0, len(f.rows))
	for _, r := range f.rows {
		vs = append(vs, fixturejson.ObjValue(r))
	}
	return fixturejson.Arr(vs...)
}

// render writes ONE ROW PER LINE — the encoding the pulls this replaces already
// used. A row is the unit: a diff between two pulls is rows added, removed and
// changed, and a reviewer reads it that way. A field-per-line encoding turned
// 1,238 lines of one plant into 24,322 for no information at all. Top-level
// keys get a line each so the tables are findable; scalars and the row objects
// are compact.
func render(fields []field) string {
	var sb strings.Builder
	sb.WriteString("{\n")
	for i, f := range fields {
		tail := ","
		if i == len(fields)-1 {
			tail = ""
		}
		fixturejson.Quote(&sb, f.key, true)
		if f.isTable && len(f.rows) > 0 {
			sb.WriteString(": [\n")
			for j, r := range f.rows {
				sb.WriteString("  ")
				sb.WriteString(fixturejson.Compact(fixturejson.ObjValue(r), false))
				if j < len(f.rows)-1 {
					sb.WriteString(",")
				}
				sb.WriteString("\n")
			}
			sb.WriteString("]" + tail + "\n")
			continue
		}
		sb.WriteString(": " + fixturejson.Spaced(f.value(), false) + tail + "\n")
	}
	sb.WriteString("}\n")
	return sb.String()
}

// repr spells a string for a message: single-quoted unless it holds a single
// quote and no double one, with non-printing runes escaped.
func repr(s string) string {
	q := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var sb strings.Builder
	sb.WriteByte(q)
	for _, r := range s {
		switch {
		case r == '\\':
			sb.WriteString(`\\`)
		case r == rune(q):
			sb.WriteByte('\\')
			sb.WriteByte(q)
		case r == '\t':
			sb.WriteString(`\t`)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r == ' ' || (r > 0x7f && unicode.IsPrint(r)) || (r > 0x20 && r < 0x7f):
			sb.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&sb, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			fmt.Fprintf(&sb, `\U%08x`, r)
		}
	}
	sb.WriteByte(q)
	return sb.String()
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("anonymiseplantpull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "a real pull, from OUTSIDE the repo")
	out := fs.String("out", "", "the fixture to write")
	tag := fs.String("tag", "", "the synthetic plant tag (A or B); its rigid motion is fixed in this program")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if _, ok := transforms[*tag]; *in == "" || *out == "" || !ok {
		fmt.Fprintln(stderr, "usage: anonymiseplantpull --in SRC --out DST --tag {A,B}")
		return 2
	}

	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	src, err := fixturejson.Parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", *in, err)
		return 1
	}
	fields, err := anonymise(src, *tag)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", *in, err)
		return 1
	}

	// THE SELF-CHECK. Fails the run rather than writing a fixture that still
	// carries the plant.
	sensitive := sensitiveStrings(src)
	if leaked := leakedValues(fields, sensitive); len(leaked) > 0 {
		fmt.Fprintf(stderr, "REFUSING TO WRITE %s — %d source value(s) survived:\n", *out, len(leaked))
		vals := make([]string, 0, len(leaked))
		for v := range leaked {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		for i, v := range vals {
			if i == 20 {
				fmt.Fprintf(stderr, "   ... and %d more\n", len(vals)-20)
				break
			}
			paths := make([]string, 0, len(leaked[v]))
			for p := range leaked[v] {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			fmt.Fprintf(stderr, "   %s at %s\n", repr(v), strings.Join(paths, ", "))
		}
		return 1
	}

	if err := os.WriteFile(*out, []byte(render(fields)), 0o644); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	count := func(k string) int {
		for _, f := range fields {
			if f.key == k {
				return len(f.rows)
			}
		}
		return 0
	}
	fmt.Fprintf(stderr, "%s -> %s  (%d points, %d edges, %d claims, %d styles, %d source strings checked)\n",
		*in, *out, count("scene_points"), count("scene_edges"), count("style_node_claims"), count("styles"), len(sensitive))
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }
