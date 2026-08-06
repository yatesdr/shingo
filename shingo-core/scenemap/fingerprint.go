package scenemap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Fingerprinting: what counts as "this object changed".
//
// A hash over a map object is what makes an edit a first-class event — it is
// how a reader sees where a series breaks. Getting its SCOPE wrong is what
// makes that reader waste a day, and there are two ways to get it wrong in
// opposite directions.
//
// TOO WIDE. Hash the whole object and a zone's series breaks when somebody
// changes its label font size. TextFontSize is present on all eleven
// Springfield areas and colorPen/colorBrush on all of them too — and those
// colours are the editor's fixed default PER CLASS, not a decision anyone
// made about a particular area, which is exactly why they cannot mean
// anything changed.
//
// TOO NARROW. Hash only the geometry and a lane whose maxspeed was halved
// reports as continuous. maxspeed is set on 91 of Springfield's 405 curves,
// and a tech who slows a lane to fix a localization problem has made precisely
// the change this system exists to evaluate. That failure is silent and in the
// dangerous direction: the reader believes before and after are comparable.
//
// So: two hashes over three classes of field.
//
//	shape       endpoints, handles, polygon vertices, reflector position
//	            → ShapeHash AND DefHash. Changing it RE-ATTRIBUTES samples:
//	              readings may now snap elsewhere, so the before and after
//	              populations are not the same floor.
//	behaviour   direction, speed, obstacle distances, useAutoReloc
//	            → DefHash only. Nothing re-snaps; the same floor under
//	              different rules.
//	cosmetic    colours, label font, label orientation
//	            → neither.
//
// THE COSMETIC SET IS AN ALLOW-LIST AND EVERYTHING UNKNOWN IS INCLUDED. A
// vendor firmware update that adds a property should break the series
// conservatively and be reviewed, rather than pass silently because nobody
// taught the hash about it. This is the same rule the wire drift test applies
// one layer up, for the same reason.

// cosmeticKeys are the object properties that carry no measurable meaning.
// Adding to this list is a decision to make a class of edit invisible to
// every "did my change help" question — do it deliberately.
var cosmeticKeys = map[string]string{
	"TextFontSize": "the size of the label drawn over the zone in the editor",
	"TextColor":    "label ink",
	"LineWidth":    "how thickly the editor draws the outline",
}

// IsCosmetic reports whether a property key is excluded from fingerprints.
func IsCosmetic(key string) bool { _, ok := cosmeticKeys[key]; return ok }

// CosmeticReason returns why a key is excluded, for the record.
func CosmeticReason(key string) string { return cosmeticKeys[key] }

// Fingerprint is one object's identity-of-content at a point in time.
type Fingerprint struct {
	// ShapeHash covers position only. A change here re-attributes samples.
	ShapeHash string
	// DefHash covers shape AND behaviour. A change here means the object is
	// not the same object, even if it did not move.
	DefHash string
}

// AreaFingerprint hashes one area.
//
// The colours are excluded even though they are attributes rather than
// properties: every ReflectorArea in SPRAMRMAP carries the identical
// colorPen/colorBrush pair and both LocConfigAreas carry a different
// identical pair, which is the evidence that they are a class default and not
// a per-area decision.
func AreaFingerprint(a Area) Fingerprint {
	shape := newDigest()
	shape.str("area")
	shape.str(a.Name)
	// The class is SHAPE, not behaviour: ReflectorArea and LocConfigArea are
	// different kinds of region, and re-declaring one as the other changes
	// what the polygon means even if not one vertex moved. It is also the
	// strongest predictor in the data, so a silent change here would be the
	// most expensive one available.
	shape.str(a.Class)
	for _, p := range a.Polygon {
		shape.float(p.X)
		shape.float(p.Y)
	}

	def := newDigest()
	def.str(shape.sum())
	for _, k := range sortedNonCosmetic(a.Properties) {
		def.str(k)
		def.str(fmt.Sprint(a.Properties[k]))
	}
	return Fingerprint{ShapeHash: shape.sum(), DefHash: def.sum()}
}

// ReflectorFingerprint hashes one reflector.
//
// A reflector has no behaviour, so both hashes cover the same fields and are
// deliberately not collapsed into one: callers compare Fingerprints without
// caring what kind of object produced them, and a type whose meaning changes
// per object is worse than a duplicated digest.
func ReflectorFingerprint(r Reflector) Fingerprint {
	d := newDigest()
	d.str("reflector")
	d.str(r.Kind)
	d.float(r.X)
	d.float(r.Y)
	// Absent width is hashed as absent, not as zero. Three of Springfield's
	// 71 carry no width, and collapsing that to 0.0 would make a reflector
	// whose width was later measured look like a change that never happened.
	if r.Width == nil {
		d.str("width:absent")
	} else {
		d.float(*r.Width)
	}
	h := d.sum()
	return Fingerprint{ShapeHash: h, DefHash: h}
}

// sortedNonCosmetic returns an object's fingerprintable property keys in a
// stable order. Sorted because Go map iteration is randomised and a hash that
// changes between two runs over identical input is not a hash.
func sortedNonCosmetic(props map[string]any) []string {
	out := make([]string, 0, len(props))
	for k := range props {
		if IsCosmetic(k) {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MaxVertexDelta is how far an object moved between two versions, in metres.
//
// THE HASH IS A DETECTOR, NOT A DECISION, and this is the number that makes
// the difference usable. Map coordinates are stored to three decimals, so a
// re-registration nudging the whole plant by 2 mm changes EVERY hash in it —
// at the 5x map that is thousands of series breaking on one day for a change
// that moved nothing. With the magnitude alongside, the break decision
// happens at query time: "changed by 4 mm, series continues" and "changed by
// 2.3 m, series breaks" are different sentences and the model can say both.
//
// It is also what turns the diff log from "something happened" into "what
// kind of thing happened": 3,585 objects at a median of 0.004 m reads as a
// rescan at a glance, 17 objects at a median of 2.3 m reads as a re-route.
//
// Returns +Inf when the two versions cannot be compared vertex-to-vertex —
// a polygon that gained or lost a corner is a redraw, not a move, and
// pretending to measure it would invent a small number for a large change.
func MaxVertexDelta(before, after []Point) float64 {
	if len(before) != len(after) {
		return math.Inf(1)
	}
	worst := 0.0
	for i := range before {
		d := math.Hypot(after[i].X-before[i].X, after[i].Y-before[i].Y)
		if d > worst {
			worst = d
		}
	}
	return worst
}

// ── digest ────────────────────────────────────────────────────────────────

type digest struct{ b strings.Builder }

func newDigest() *digest { return &digest{} }

func (d *digest) str(s string) { d.b.WriteString(s); d.b.WriteByte(0) }

// float writes a float in a form that round-trips exactly and renders one way
// only. %v would print 1e-07 for one value and 0.0000001 for another
// depending on magnitude, which would make the hash depend on formatting
// rather than on the number.
func (d *digest) float(f float64) {
	// Negative zero and zero are the same coordinate; normalise so a sign bit
	// arriving off the wire cannot look like a move. This map genuinely
	// carries negative zeros — SEER publishes -0.0 for a missing confidence
	// and the editor writes them into geometry too.
	if f == 0 {
		f = 0
	}
	d.b.WriteString(fmt.Sprintf("%.9g", f))
	d.b.WriteByte(0)
}

func (d *digest) sum() string {
	h := sha256.Sum256([]byte(d.b.String()))
	return hex.EncodeToString(h[:])
}
