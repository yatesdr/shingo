package config

import (
	"fmt"
	"reflect"
	"sort"
	"time"
)

// provenance.go — where a display number came from, recorded as data.
//
// THE RISK THIS EXISTS FOR: a sim-derived constant shipped as though it were a
// real one. Phase 6's surfaces are built and tuned against the simulator, which
// exercises every code path that mints an episode but reports NOTHING about the
// distribution — not the typical duration, not a healthy cost ratio, not how
// often a zero-order episode really happens. A greying threshold or a ramp
// maximum chosen against that is a number that LOOKS like knowledge and was
// measured against the wrong thing. It is the same failure family as
// expected_orders rendering 0 or 1 as a real ratio: a value that is structurally
// indistinguishable from a real one at the point someone reads it.
//
// So every Phase 6 display constant is config, never a literal, and every one
// carries a provenance record saying how it was chosen and whether a plant has
// to re-derive it.
//
// A COMMENT WOULD NOT HAVE DONE. This repo found five stale comments in one day
// still describing reversed behaviour, and a comment is deletable without the
// compiler or a test noticing. The provenance here is a registry in exact
// bijection with the config struct, held by TestDisplayProvenanceCoversEveryField
// — adding a display constant without recording where it came from does not
// compile past the test suite. That is the difference between a rule and a rule
// with enforcement, and an unenforced rule decays.

// Provenance says how a display constant's value was chosen, and therefore what
// has to happen to it at a plant.
//
// The vocabulary is deliberately about CONSEQUENCE, not about method: two
// numbers with the same provenance get the same treatment on the deploy watch
// list. The Note on each record carries the method.
type Provenance string

const (
	// ProvenanceSME — domain knowledge, from a named person on a named date.
	//
	// Ships as-is. Does NOT need re-deriving after deploy, and re-deriving it
	// against the sim would REPLACE REAL PROVENANCE WITH WORSE PROVENANCE. A
	// person who has stood on the floor knows what forty-five minutes of a cell
	// waiting on material means; the simulator does not and cannot.
	ProvenanceSME Provenance = "SME-JUDGEMENT"

	// ProvenanceSimDerived — chosen WITHOUT PLANT DATA.
	//
	// That covers two methods with one consequence: measured against the
	// simulator, or reasoned from the code's own arithmetic in the absence of
	// anything better. Both must be re-derived from real data at a plant, and
	// both must appear on the deploy watch list. The Note says which it was.
	//
	// Kept as one value rather than split because the split would be a
	// distinction with no operational difference — every SIM-DERIVED constant
	// takes the same action — and a vocabulary that draws lines nobody acts on
	// is a vocabulary people stop maintaining.
	ProvenanceSimDerived Provenance = "SIM-DERIVED"

	// ProvenanceStructural — not a measurement at all.
	//
	// The value follows from the structure of something else in the system and
	// is CHECKABLE against it rather than validated against data. A plant cannot
	// re-derive it because there is nothing to measure; if it is wrong, a test
	// against the structure says so. It stays off the watch list for exactly
	// that reason — putting it there would ask a plant to validate a number that
	// real data has no opinion about.
	ProvenanceStructural Provenance = "STRUCTURAL"
)

// Valid reports whether p is one of the three known values. An unknown
// provenance is a bug, not a new category: adding a category is a deliberate
// act that edits this file.
func (p Provenance) Valid() bool {
	switch p {
	case ProvenanceSME, ProvenanceSimDerived, ProvenanceStructural:
		return true
	}
	return false
}

// NeedsPlantRederivation reports whether a plant has to re-measure this constant
// once real episodes are flowing. This is the predicate the deploy watch list is
// built from — see SimDerivedConstants.
func (p Provenance) NeedsPlantRederivation() bool { return p == ProvenanceSimDerived }

// ConstantProvenance is one display constant's record.
type ConstantProvenance struct {
	// Path is the YAML path of the field this describes, e.g.
	// "display.worry_after". It is the join key to DisplayConfig and the
	// bijection test asserts it resolves to a real field.
	Path string

	// Kind is how the value was chosen.
	Kind Provenance

	// Source names WHO decided and WHEN, for ProvenanceSME. Empty for the other
	// two kinds, which have no person behind them — and that emptiness is
	// asserted, so "SME judgement" can never be claimed anonymously.
	Source string

	// Note is the reasoning, in full. For SIM-DERIVED it must also say what a
	// plant should measure to replace it — a watch-list line that says "re-derive
	// this" without saying FROM WHAT is not actionable, and the test enforces
	// that the note is non-empty.
	Note string
}

// DisplayConfig holds every numeric constant the Phase 6 surfaces render with.
//
// NOTHING IN HERE IS A LITERAL AT A USE SITE. Every field is read through the
// config, so a plant can retune the surface without a rebuild, and — the reason
// that matters more — so that the value has one home that the provenance
// registry can point at. A threshold written inline at the render site has no
// place to attach a provenance record to.
type DisplayConfig struct {
	// WorryAfter is the duration at which an open demand episode stops being
	// routine. Crossing it takes ITS OWN VISUAL CHANNEL — a ring treatment plus
	// a printed band name — never colour alone.
	WorryAfter time.Duration `yaml:"worry_after"`

	// ConcernAfter is the duration at which an open episode is a problem.
	// Also its own channel, distinct from WorryAfter's.
	ConcernAfter time.Duration `yaml:"concern_after"`

	// RampSteps is how many steps the magnitude ramp has.
	//
	// THE RAMP CARRIES MAGNITUDE ONLY UP TO WorryAfter. It deliberately does not
	// span the whole duration range, because a smooth scale must never encode a
	// threshold: if ConcernAfter were simply the top of the ramp, a 61-minute
	// episode and a three-hour one would render identically, losing all
	// discrimination exactly past the line where it matters most. Calm is
	// graduated; alerts are loud and separate.
	RampSteps int `yaml:"ramp_steps"`

	// MinExpectedOrders is the smallest expected_orders for which a cost ratio
	// is shown at full strength. Below it the ratio is greyed and the absolute
	// order count carries the row.
	MinExpectedOrders int `yaml:"min_expected_orders"`

	// OrphanBucket is the time-bucket width for the orphan-rate trend (5.7).
	//
	// It is a DISPLAY constant and not merely a query scope, which is the
	// distinction episodeWindow sits on the other side of. A window states
	// itself on the page in words and nobody reads "the last 24 hours" as a
	// claim about the floor; a bucket width silently decides what the trend can
	// resolve. Too wide and a rate that climbed for an hour is averaged away;
	// too narrow and every bucket falls under MinBucketOrders and the whole
	// line greys out. That is a judgement about the data, so it carries
	// provenance.
	OrphanBucket time.Duration `yaml:"orphan_bucket"`

	// MinBucketOrders is the smallest order count for which an orphan RATE is
	// shown at full strength (5.7). Below it the rate is greyed and the raw
	// orphan count carries the bucket.
	//
	// The same rule as MinExpectedOrders, one surface along, and it is here for
	// the same reason: a rate over a denominator too small to support it is a
	// number that looks like knowledge. Distinct from a ZERO denominator, which
	// is not a small rate but no rate at all.
	MinBucketOrders int `yaml:"min_bucket_orders"`

	// BarSteps is how many steps the orphan-trend magnitude bars quantise to.
	//
	// STRUCTURAL, exactly like RampSteps: it is not a measurement, it is the
	// size of the CSS class set the bars render through, and a plant cannot
	// re-derive it because real orphan counts have no opinion about how many
	// .or-bar-N rules exist.
	BarSteps int `yaml:"bar_steps"`
}

// displayProvenance is the registry. One entry per DisplayConfig field, no more
// and no fewer — TestDisplayProvenanceCoversEveryField holds both directions.
var displayProvenance = []ConstantProvenance{
	{
		Path:   "display.worry_after",
		Kind:   ProvenanceSME,
		Source: "Stephen Brown, 2026-07-26",
		Note: "Domain judgement, not a measurement: forty-five minutes is how long a " +
			"cell can sit short of material before someone should be walking to it. " +
			"SHIPS AS-IS AND IS NOT RE-DERIVED AT A PLANT — the simulator has no " +
			"opinion about what a floor can absorb, so re-deriving this against sim " +
			"data would replace real provenance with worse provenance. OPEN, for the " +
			"report rather than for a decision: whether one worry line holds for all " +
			"three demand kinds. A loader below threshold, a cell asking mid-run and a " +
			"changeover waiting on its full set may not share it. Shipping 45 for all " +
			"three; confirm per-kind at the plant.",
	},
	{
		Path:   "display.concern_after",
		Kind:   ProvenanceSME,
		Source: "Stephen Brown, 2026-07-26",
		Note: "Domain judgement, same provenance and same standing as worry_after: at " +
			"an hour the episode is a problem rather than a delay. Ships as-is, not " +
			"re-derived. Same per-kind question open.",
	},
	{
		Path: "display.ramp_steps",
		Kind: ProvenanceStructural,
		Note: "Not a measurement. The magnitude ramp has as many steps as the token set " +
			"it renders through, and shared/tokens.css defines --viz-seq-1 .. " +
			"--viz-seq-5. A plant cannot re-derive this because real durations have no " +
			"opinion about how many teal tokens exist. It is CHECKED instead of " +
			"validated: TestRampStepsMatchesTokenSet counts the --viz-seq-N " +
			"declarations in tokens.css and fails if this number and that count " +
			"disagree, which is what turns a structural claim into a checkable one.",
	},
	{
		Path: "display.min_expected_orders",
		Kind: ProvenanceSimDerived,
		Note: "CHOSEN WITHOUT PLANT DATA — and by arithmetic, not by measurement, which " +
			"is worth saying plainly because the sim could not have supplied it " +
			"either. The reasoning: at expected_orders = 1 the ratio's smallest " +
			"possible non-trivial value is 2.0x and its next is 3.0x, so the column " +
			"cannot discriminate at all — one extra order and two extra orders are a " +
			"100-point jump apart. At 2 the steps are 0.5 and the column starts " +
			"saying something. So the floor greys exactly the expected=1 case and " +
			"nothing else. WHAT A PLANT SHOULD MEASURE INSTEAD: the distribution of " +
			"expected_orders across real episodes, and the ratio distribution " +
			"conditioned on it — the floor belongs wherever the ratio stops being " +
			"dominated by the granularity of its own denominator. Springfield has the " +
			"episodes to answer that; the sim does not.",
	},
	{
		Path: "display.orphan_bucket",
		Kind: ProvenanceSimDerived,
		Note: "CHOSEN WITHOUT PLANT DATA, and it could not have been otherwise: the " +
			"quantity this width has to be matched to is the plant's order-creation " +
			"RATE, and the sim's rate is a property of demo.yaml's tick rates rather " +
			"than of any floor. One hour is chosen so that a bucket has a chance of " +
			"clearing min_bucket_orders on a running line while still resolving a rate " +
			"that climbs over a shift. Both halves of that sentence are guesses. THE " +
			"FAILURE MODE IF IT IS WRONG IS NOT A WRONG NUMBER BUT A SILENT ONE: too " +
			"narrow and every bucket falls under the floor and the entire trend renders " +
			"greyed, which reads as 'nothing to see' rather than as 'badly bucketed'. " +
			"WHAT A PLANT SHOULD MEASURE INSTEAD: the distribution of orders created per " +
			"candidate bucket width over a fortnight, and pick the narrowest width whose " +
			"TENTH PERCENTILE bucket still clears min_bucket_orders — the tenth and not " +
			"the median, because the quiet buckets are the ones that grey out and a " +
			"median-tuned width greys half the line.",
	},
	{
		Path: "display.min_bucket_orders",
		Kind: ProvenanceSimDerived,
		Note: "CHOSEN WITHOUT PLANT DATA and by arithmetic, the same way and for the " +
			"same reason as min_expected_orders. The reasoning: in a bucket of n orders " +
			"a single orphan reads as 1/n, so the floor belongs where one orphan stops " +
			"DOMINATING the reading. Requiring one orphan to move the rate by no more " +
			"than five points gives 1/n <= 0.05, so n >= 20. Below that the line is " +
			"drawing individual events as though they were a rate, and the slope — which " +
			"is the entire signal — becomes a function of how many orders happened to be " +
			"created rather than of how often they orphaned. WHAT A PLANT SHOULD MEASURE " +
			"INSTEAD: the orphan rate's own distribution across buckets at the chosen " +
			"width, and put the floor where the bucket-to-bucket variance stops being " +
			"dominated by the granularity of 1/n. Springfield has the order volume to " +
			"answer this; the sim's orphan count is zero, so the sim cannot answer it " +
			"even in principle.",
	},
	{
		Path: "display.bar_steps",
		Kind: ProvenanceStructural,
		Note: "Not a measurement, and the same kind of claim as ramp_steps. The " +
			"orphan-trend bars quantise to as many steps as there are .or-bar-N rules " +
			"in shingo-core/www/static/style.css, because the style guide forbids " +
			"inline style= in new code and a bar height therefore has to be a class. " +
			"A plant cannot re-derive this: real orphan counts have no opinion about " +
			"how many CSS rules exist. It is CHECKED rather than validated — " +
			"TestBarStepsMatchesCSSClassSet counts the rules and fails if this number " +
			"and that count disagree, which is what turns a structural claim into a " +
			"checkable one. Without the check, a bar could quantise to a step whose " +
			"class does not exist and would render at zero height: a bucket with " +
			"orphans drawn as a bucket with none.",
	},
}

// DisplayProvenance returns every display constant's provenance record.
//
// Returned sorted by path and as a copy, so a caller cannot reorder or mutate
// the registry — a watch list built from a slice someone else can append to is a
// watch list that silently changes under its reader.
func DisplayProvenance() []ConstantProvenance {
	out := make([]ConstantProvenance, len(displayProvenance))
	copy(out, displayProvenance)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// SimDerivedConstants returns exactly the constants a plant must re-derive.
//
// THIS IS WHAT THE DEPLOY WATCH LIST CONSUMES. Stage 3.8 needs one named
// expectation per behaviour change, and a constant chosen without plant data is
// such a change: it renders a judgement the plant has not confirmed. Enumerating
// them from the registry rather than from a hand-kept list is the point — a hand
// kept list is exactly the artefact that goes stale the first time someone adds
// a constant and forgets, and this repo's recorded failure family is a document
// asserting a state nobody verified.
func SimDerivedConstants() []ConstantProvenance {
	var out []ConstantProvenance
	for _, c := range DisplayProvenance() {
		if c.Kind.NeedsPlantRederivation() {
			out = append(out, c)
		}
	}
	return out
}

// DisplayFieldPaths returns the YAML path of every field on DisplayConfig, by
// reflection over the struct rather than by a second hand-kept list.
//
// The bijection test compares this against the registry. Reflection is what
// makes the check total: a hand-written list of "the fields we have" would need
// updating in the same act that the test exists to catch someone forgetting.
func DisplayFieldPaths() []string {
	t := reflect.TypeOf(DisplayConfig{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		out = append(out, "display."+tag)
	}
	sort.Strings(out)
	return out
}

// DisplayDefaults returns the shipped values. Split out of Defaults() so the
// provenance tests can reach them without constructing a whole Config, and so
// the values sit adjacent to nothing else — a display constant edited by
// accident while editing a database default is a real way for this to go wrong.
func DisplayDefaults() DisplayConfig {
	return DisplayConfig{
		WorryAfter:        45 * time.Minute,
		ConcernAfter:      60 * time.Minute,
		RampSteps:         5,
		MinExpectedOrders: 2,
		OrphanBucket:      time.Hour,
		MinBucketOrders:   20,
		BarSteps:          10,
	}
}

// DisplayConstants returns the display constants under the read lock, falling
// back to the shipped default for any field left at its zero value.
//
// CALL THIS, never c.Display directly. The struct field is the raw YAML; this is
// the resolved value, and the difference is the fallback below.
//
// THE FALLBACK IS PER-FIELD, NOT WHOLE-STRUCT, because YAML unmarshals onto the
// defaults struct in Load and a partially-specified display block is therefore
// normal. The zero value is not meaningful for any of these four: a worry line
// of zero would put every episode past it on the first tick, and a ramp of zero
// steps has no rendering at all. So zero means unset, and unset means shipped.
//
// A NEGATIVE VALUE IS ALSO UNSET rather than clamped-to-zero. Clamping would
// honour a typo; falling back makes a typo visible as "the number did not
// change" rather than as "every row is now in the worry band".
func (c *Config) DisplayConstants() DisplayConfig {
	c.mu.RLock()
	d := c.Display
	c.mu.RUnlock()

	def := DisplayDefaults()
	if d.WorryAfter <= 0 {
		d.WorryAfter = def.WorryAfter
	}
	if d.ConcernAfter <= 0 {
		d.ConcernAfter = def.ConcernAfter
	}
	if d.RampSteps <= 0 {
		d.RampSteps = def.RampSteps
	}
	if d.MinExpectedOrders <= 0 {
		d.MinExpectedOrders = def.MinExpectedOrders
	}
	if d.OrphanBucket <= 0 {
		d.OrphanBucket = def.OrphanBucket
	}
	if d.MinBucketOrders <= 0 {
		d.MinBucketOrders = def.MinBucketOrders
	}
	if d.BarSteps <= 0 {
		d.BarSteps = def.BarSteps
	}
	return d
}

// Validate reports every display constant that is internally inconsistent.
//
// Only one relationship is load-bearing and it is the one a retune breaks:
// WorryAfter must be strictly below ConcernAfter. Inverted, the bands become
// unreachable in an order no rendering code checks for — an episode would be
// "concern" before it was ever "worry", and the ramp would carry magnitude past
// the very line it is supposed to stop at.
//
// Returned rather than logged, and never applied as a silent correction: a
// plant that inverts these has made a decision the surface cannot honour, and
// quietly swapping them would render numbers nobody chose.
func (d DisplayConfig) Validate() error {
	if d.WorryAfter >= d.ConcernAfter {
		return fmt.Errorf("display: worry_after (%s) must be strictly less than concern_after (%s)",
			d.WorryAfter, d.ConcernAfter)
	}
	return nil
}
