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

	// ProvenanceSimDerived — chosen WITHOUT DATA FROM THE PLANT IT WILL RUN AT.
	//
	// That covers THREE methods with one consequence:
	//
	//   1. measured against the simulator;
	//   2. reasoned from the code's own arithmetic in the absence of anything
	//      better;
	//   3. MEASURED AT ONE PLANT, over one window — which is a fact about that
	//      plant and not about the next one.
	//
	// All three must be re-derived from real data at a plant, and all three must
	// appear on the deploy watch list. The Note says which it was, and for (3) it
	// must say WHICH plant, WHICH window and HOW MANY samples, because those are
	// what a reader needs in order to judge whether the number travels.
	//
	// The third method was added 2026-07-27 with the 5.11 constants, whose values
	// come off the Springfield 2026-07-27 dump. The label still reads SIM-DERIVED
	// and that is now slightly a misnomer; the value was left alone deliberately.
	// THE VOCABULARY IS ABOUT CONSEQUENCE, NOT METHOD (see above), and (3) takes
	// exactly the same action as (1) and (2). A fourth Provenance value was
	// considered and rejected on that rule: it would have split the watch-list
	// predicate in two for a distinction nobody acts on differently, and a
	// vocabulary that draws lines nobody acts on is a vocabulary people stop
	// maintaining.
	//
	// The one thing method (3) must NOT be recorded as is ProvenanceSME. A
	// measurement is not a judgement, SME is the kind that ships without
	// re-derivation, and taking that exemption for a single-plant measurement is
	// the exact failure TestSMEExemptionsArePinned exists to make deliberate.
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
	// ── Cycle time (5.10) ────────────────────────────────────────────────────

	// CycleMinSamples is the fewest gaps a (station, payload, direction) key
	// needs before its p90 and p99 are reported at all.
	CycleMinSamples int `yaml:"cycle_min_samples"`

	// CycleSpreadMultiple sets where a key's tail begins:
	// median + k × (p90 − median). Expressed against the key's OWN spread rather
	// than as a percentage of its takt, because dispersion inverts between
	// Springfield's fast and slow payload families and a percentage would
	// over-trigger on one and under-trigger on the other.
	CycleSpreadMultiple float64 `yaml:"cycle_spread_multiple"`

	// CycleBandWidth is the histogram band width, in multiples of the key's own
	// median. The band count follows from it — see domain.CycleBands.
	CycleBandWidth float64 `yaml:"cycle_band_width"`

	// CycleFlushInterval is the Edge accumulator's delta flush cadence. A key
	// whose median cycle is at or below it is measuring the transport, not the
	// cell, and the page says so instead of printing the number as a takt.
	CycleFlushInterval time.Duration `yaml:"cycle_flush_interval"`

	// ── Route index (U3, the per-robot breakdown) ────────────────────────────

	// RouteIndexMinRouteSamples is the fewest missions a route needs before its
	// median may be used as the denominator of a robot's route index.
	//
	// It is the load-bearing one: the index is duration / that route's median, so
	// a route with two missions has a "median" that IS one of the two durations,
	// and the ratio it produces measures nothing. Routes below this floor are
	// excluded from the index entirely rather than greyed, and if NO route clears
	// it the column is dropped from the table.
	RouteIndexMinRouteSamples int `yaml:"route_index_min_route_samples"`

	// RouteIndexMinRobotSamples is the fewest indexed missions a robot needs
	// before ITS index is shown at full strength. Below it the figure is greyed
	// and the mission count carries the row — the same rule as
	// MinExpectedOrders and MinBucketOrders, one surface along.
	//
	// Separate from RouteIndexMinRouteSamples because the two floors protect
	// different things: the route floor protects the DENOMINATOR from being a
	// non-median, and the robot floor protects the AGGREGATE from being one
	// unlucky trip. A robot with 40 missions all on unqualified routes has an
	// index of nothing; a robot with one mission on a well-sampled route has an
	// index of one trip. Those are different failures and one number cannot
	// express both.
	RouteIndexMinRobotSamples int `yaml:"route_index_min_robot_samples"`

	// ── Material flags (5.11) ────────────────────────────────────────────────

	// StaleBindingAfter is how long a carrier may hold one binding before that
	// binding is a CANDIDATE for having gone stale — ShinGo still believes the
	// carrier holds payload P at node N while the floor has moved on around it.
	//
	// THIS IS THE SELECTOR FOR THE LEDGER HALF OF /material-flags, AND THE
	// LEDGER'S SIGN IS NOT. That is the whole design of the surface: a flag hung
	// off "uop_remaining < 0" inherits the ledger's noise, and the Springfield
	// data says so in both directions at once — see the provenance note.
	StaleBindingAfter time.Duration `yaml:"stale_binding_after"`

	// OverpackBinloads is where a negative ledger stops being explicable as ONE
	// overpacked carrier, expressed in multiples of that carrier's own payload
	// capacity rather than in units.
	//
	// It CLASSIFIES A READING AND SELECTS NOTHING. A negative shallower than this
	// is overpack-shaped: more parts went into the carrier than were declared,
	// which is a cycle count. Deeper than this and one overpack cannot account for
	// it — more than a binload of parts moved without ShinGo seeing it. Both are
	// cycle counts and neither is an outage.
	//
	// In multiples, not units, because 250-unit and 3,600-unit payloads share this
	// page: −300 is more than a full carrier of one and a twelfth of the other.
	OverpackBinloads float64 `yaml:"overpack_binloads"`
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
	{
		Path: "display.cycle_min_samples",
		Kind: ProvenanceSimDerived,
		Note: "CHOSEN WITHOUT PLANT DATA, and by arithmetic rather than measurement. The " +
			"quantiles here are NEAREST RANK, so the p90 of n samples is the element at " +
			"index ceil(0.9n). For every n up to 9 that index IS n — the p90 and the " +
			"maximum are the same observation, and a column headed p90 would be printing " +
			"the largest gap in the window under a label saying nine in ten are below it. " +
			"At n = 10 the index is 9 of 10 and the two separate for the first time. So the " +
			"floor is the smallest n at which the statistic stops being a synonym for the " +
			"maximum, and nothing more is claimed for it. WHAT A PLANT SHOULD MEASURE " +
			"INSTEAD: the sampling distribution of the p90 across real keys — resample a " +
			"long-running key's gaps at increasing n and find where the p90 estimate stops " +
			"moving. That is a question about how noisy a real cell is, which no amount of " +
			"arithmetic answers and the simulator answers wrongly.",
	},
	{
		Path: "display.cycle_spread_multiple",
		Kind: ProvenanceSimDerived,
		Note: "CHOSEN WITHOUT PLANT DATA. Sets the tail cut at median + k × (p90 − median). " +
			"The SHAPE of that expression is defensible from measured Springfield data and " +
			"the value of k is not. Measured: dispersion INVERTS between the two payload " +
			"families — the slow family (70–80 s median) is the steadiest at CV 0.27–0.32 " +
			"and the fast family (20–31 s) is the noisiest at CV 0.42–0.86 — so any cut " +
			"expressed as a percentage of takt over-triggers on fast lines and " +
			"under-triggers on slow ones. Taking the spread from the key's own history " +
			"removes that failure without keying the constant per payload. The 3 is the " +
			"conventional three-sigma shape with (p90 − median) standing in for sigma; for a " +
			"normal that is about 3.8 sigma, and for a right-skewed real distribution it is " +
			"wider. THAT IS A SHAPE ASSUMPTION THE DATA DOES NOT CONFIRM. WHAT A PLANT " +
			"SHOULD MEASURE INSTEAD: the gap distribution per key ABOVE its own p90, for " +
			"the slow family in particular — the published Springfield percentiles give " +
			"p05/p50/p75/p95/p99 for the fast family only, so the slow family's tail shape " +
			"is entirely unmeasured and k cannot be derived for it. Then set k where the " +
			"count past the cut correlates with something a person on the floor recognises " +
			"as a stop.",
	},
	{
		Path: "display.cycle_band_width",
		Kind: ProvenanceSimDerived,
		Note: "CHOSEN WITHOUT PLANT DATA — derived from Springfield's measured MODES rather " +
			"than from any judgement about what a good cycle looks like. The distribution " +
			"is strongly trimodal: 25 s (30.3%), 30 s (13.9%) and 20 s (13.1%) hold roughly " +
			"57% of all intervals. Bands are centred on the key's own median and are " +
			"half-open float ranges, never equality and never integer buckets — only 29 of " +
			"219,465 intervals are exact multiples of five (the raw values are 24.995826 " +
			"and 29.999909), so anything that rounds or compares for equality finds " +
			"nothing. At 0.25 the three modes land in three DIFFERENT bands relative to a " +
			"25 s median: 0.8 in [0.625,0.875), 1.0 in [0.875,1.125), 1.2 in [1.125,1.375). " +
			"Widen it and the modes merge; narrow it and the picture becomes noise. WHAT A " +
			"PLANT SHOULD MEASURE INSTEAD: the modal structure per key rather than pooled. " +
			"The pooled figures above are a mixture over at least two families with " +
			"different takts, and a width that separates the pooled modes need not separate " +
			"any single key's own.",
	},
	{
		Path: "display.cycle_flush_interval",
		Kind: ProvenanceStructural,
		Note: "Not a measurement and not a judgement — it is a number that already exists " +
			"elsewhere in this repo, restated here because Core cannot import it. Edge " +
			"accumulates UOP deltas and flushes them on a timer: " +
			"defaultInventoryDeltaInterval in shingo-edge/uop/accumulator.go. That cadence " +
			"is the resolution of every interval this surface computes, which is why a " +
			"naive median over the un-partitioned audit stream reads 4.99 s at Springfield " +
			"— the flush, not a cycle. A plant cannot re-derive this because it is not a " +
			"property of the floor; if Edge's cadence changes, this follows. It is CHECKED " +
			"rather than validated: TestCycleFlushIntervalMatchesEdge reads the Edge source " +
			"and fails if the two disagree, which is what turns a structural claim into a " +
			"checkable one.",
	},
	{
		Path: "display.route_index_min_route_samples",
		Kind: ProvenanceSimDerived,
		Note: "CHOSEN WITHOUT PLANT DATA, and by arithmetic rather than measurement — the " +
			"sim could not have supplied it, because its route mix is a property of " +
			"demo.yaml's node graph rather than of any floor. The reasoning: this number " +
			"is the sample count behind a MEDIAN that is then used as a DENOMINATOR. At " +
			"n=2 the 'median' is the mean of the only two observations, so a robot's " +
			"index against it is that robot's duration divided by a number it half " +
			"determined — the ratio is partly self-referential and cannot be 1.0 by " +
			"accident. At n=3 one outlier still IS the median. Requiring the median to " +
			"survive the removal of any single observation needs n>=5, and requiring it " +
			"to survive any two needs n>=7; 8 is the first even value clearing that with " +
			"a margin, and even is worth having because percentile_cont interpolates " +
			"between the two central values rather than picking one. WHAT A PLANT SHOULD " +
			"MEASURE INSTEAD: the distribution of missions-per-route over a fortnight, " +
			"and the spread of each route's duration. The floor belongs wherever a " +
			"route's median stops moving materially when one mission is added — which is " +
			"a question about that route's own dispersion, and Springfield's route mix is " +
			"heavy-tailed enough that the answer will differ between the supermarket " +
			"hauls and the lineside hops. THE FAILURE MODE IF IT IS TOO HIGH IS SILENT " +
			"IN THE OTHER DIRECTION from the usual one: no route qualifies, the index " +
			"column DISAPPEARS, and the panel looks like it never had one.",
	},
	{
		Path: "display.route_index_min_robot_samples",
		Kind: ProvenanceSimDerived,
		Note: "CHOSEN WITHOUT PLANT DATA, same family as min_expected_orders and " +
			"min_bucket_orders and by the same arithmetic. The quantity is a median of " +
			"RATIOS, so the question is how many ratios before one bad trip stops " +
			"dominating it: at n=5 a single 3x trip moves the median a full step; " +
			"requiring the median ratio to be robust to any one observation gives n>=5 " +
			"and to any two gives n>=7. 10 is taken rather than 7 for a reason specific " +
			"to what this column ASSERTS — it is read as a claim about a robot, and a " +
			"claim about a robot that flips when it makes one slow trip will be " +
			"disbelieved after the first time it does. Ten also matches " +
			"cycle_min_samples, and two small-n floors on adjacent analytics surfaces " +
			"agreeing is worth more than each being individually optimal. WHAT A PLANT " +
			"SHOULD MEASURE INSTEAD: the per-robot ratio distribution, and put the floor " +
			"where the robot-to-robot variance stops being dominated by sample size — " +
			"concretely, where a robot's index computed on its first half of missions " +
			"agrees with the index on its second half. That is answerable at Springfield " +
			"and not answerable in the sim, whose robots are interchangeable by " +
			"construction so every real index is 1.0 and the floor cannot be exercised.",
	},
	{
		Path: "display.stale_binding_after",
		Kind: ProvenanceSimDerived,
		Note: "MEASURED AT ONE PLANT AND THEREFORE NOT AT YOURS — method (3) on " +
			"ProvenanceSimDerived. Source: the Springfield dump of 2026-07-27, " +
			"bin_uop_audit, 234,725 rows over 82.2 days (2026-05-04 to 2026-07-25) across " +
			"29 carriers, segmented into 531 completed bindings at the epoch-bumping " +
			"boundaries (audit.EpochBumpOps). That distribution: p50 = 2h 54m, p90 = 1.98 " +
			"days, p95 = 4.17 days, p99 = 20.98 days, longest = 22.99 days. 72 hours sits " +
			"between p90 and p95 and selects 37 of the 531 (7.0%) — roughly one candidate " +
			"every two days at Springfield's carrier count, which is the actual criterion: " +
			"a candidate list is only worth anything at a rate a cycle-count owner can work " +
			"through, and it must be neither empty nor a wall. " +
			"THE RATE IS PROPORTIONAL TO FLEET SIZE, so this number does not travel: a " +
			"plant with three times the carriers gets three times the rows from the same " +
			"threshold, and one running longer campaigns per carrier gets them from a " +
			"longer line. WHAT A PLANT SHOULD MEASURE INSTEAD: its own binding-duration " +
			"distribution over a fortnight, segmented the same way, then put the line where " +
			"the resulting candidate rate matches what the person doing the counting can " +
			"absorb — and re-check it after any change to campaign length or fleet size, " +
			"because both move it and neither is a change anyone would think to re-derive a " +
			"display constant for.",
	},
	{
		Path: "display.overpack_binloads",
		Kind: ProvenanceSimDerived,
		Note: "MEASURED AT ONE PLANT (method (3)) against a physical argument, and it " +
			"CLASSIFIES A READING RATHER THAN SELECTING A ROW — which is the more important " +
			"half of its provenance, because getting that backwards is the failure the " +
			"whole surface is built against. The argument for 1.0: an overpack is bounded " +
			"by what physically fits in one carrier, so a ledger at or past one full " +
			"capacity below zero cannot be a single overpacked binload however generous the " +
			"operator was. Springfield agrees that the line separates two populations — of " +
			"191 negative bindings, 148 (77%) are shallower than one binload with a median " +
			"duration of 0.10 days, and 43 are deeper with a median of 1.48 days. " +
			"BUT BOTH TAILS CROSS IT, WHICH IS WHY THIS SELECTS NOTHING: the longest " +
			"binding in the dump (bin 27, 22.99 days) reads only 0.17 binloads below zero, " +
			"and the deepest ledger in the dump (bin 39, -10,214 = 10.2 binloads) sat on a " +
			"binding 1.6 hours old. Depth against binding age correlates at Pearson 0.553 " +
			"over the negative population — related, and nowhere near the same axis. " +
			"WHAT A PLANT SHOULD MEASURE INSTEAD: its own depth-in-binloads distribution, " +
			"and specifically whether it is bimodal at all. The two-population reading " +
			"above is Springfield's; a plant whose overpacks are routinely two binloads " +
			"(deeper dunnage, a mis-set capacity) has its shallow mode on the other side of " +
			"this line and every overpack would read as something worse.",
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

		CycleMinSamples:     10,
		CycleSpreadMultiple: 3,
		CycleBandWidth:      0.25,
		CycleFlushInterval:  5 * time.Second,

		RouteIndexMinRouteSamples: 8,
		RouteIndexMinRobotSamples: 10,
		StaleBindingAfter:         72 * time.Hour,
		OverpackBinloads:          1.0,
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
	if d.CycleMinSamples <= 0 {
		d.CycleMinSamples = def.CycleMinSamples
	}
	if d.CycleSpreadMultiple <= 0 {
		d.CycleSpreadMultiple = def.CycleSpreadMultiple
	}
	if d.CycleBandWidth <= 0 {
		d.CycleBandWidth = def.CycleBandWidth
	}
	if d.CycleFlushInterval <= 0 {
		d.CycleFlushInterval = def.CycleFlushInterval
	}
	if d.RouteIndexMinRouteSamples <= 0 {
		d.RouteIndexMinRouteSamples = def.RouteIndexMinRouteSamples
	}
	if d.RouteIndexMinRobotSamples <= 0 {
		d.RouteIndexMinRobotSamples = def.RouteIndexMinRobotSamples
	}
	if d.StaleBindingAfter <= 0 {
		d.StaleBindingAfter = def.StaleBindingAfter
	}
	if d.OverpackBinloads <= 0 {
		d.OverpackBinloads = def.OverpackBinloads
	}
	return d
}

// Validate reports every display constant that is internally inconsistent.
//
// Only relationships a retune can genuinely break belong here, not range checks
// for their own sake. Two qualify.
//
// FIRST: WorryAfter must be strictly below ConcernAfter. Inverted, the bands
// become unreachable in an order no rendering code checks for — an episode would
// be "concern" before it was ever "worry", and the ramp would carry magnitude
// past the very line it is supposed to stop at.
//
// SECOND: CycleSpreadMultiple must be strictly above 1. At exactly 1 the tail cut
// IS the p90, so the tail count becomes 10% of every key's sample size BY
// CONSTRUCTION — the same number on a healthy line and a stopped one. That is
// precisely the quantile failure mode the spread-based cut exists to avoid, and
// it is reachable by typing a smaller number into a config file. Below 1 the cut
// sits between the median and the p90 and flags more than a tenth of everything.
//
// Returned rather than logged, and never applied as a silent correction: a plant
// that sets these has made a decision the surface cannot honour, and quietly
// substituting a value would render numbers nobody chose.
//
// STALE_BINDING_AFTER AND OVERPACK_BINLOADS ARE DELIBERATELY ABSENT, and the
// reasoning is recorded so nobody adds a range check for its own sake. Neither
// participates in a relationship with another constant: the binding age and the
// episode duration are independent axes on /material-flags and coupling them
// here would invent a dependency the surface does not have. Their degenerate
// values — zero or negative — are already unreachable, because DisplayConstants
// treats those as unset and falls back to the shipped default. A check that
// cannot fire is a check that reads as coverage.
//
// CALLED FROM handleMaterialFlags, AND BEFORE 2026-07-27 FROM NOWHERE. This
// function had no production caller at all — only two tests — so the
// worry/concern ordering it protects was unenforced against a real config file
// for as long as it has existed. It is reported on the page rather than refused
// at startup: a display constant must not be able to stop a plant's core from
// booting. /demand-episodes and /orphans still do not consult it; wiring them is
// a tail, named here rather than done in passing.
func (d DisplayConfig) Validate() error {
	if d.WorryAfter >= d.ConcernAfter {
		return fmt.Errorf("display: worry_after (%s) must be strictly less than concern_after (%s)",
			d.WorryAfter, d.ConcernAfter)
	}
	if d.CycleSpreadMultiple <= 1 {
		return fmt.Errorf("display: cycle_spread_multiple (%g) must be strictly above 1 — at 1 the "+
			"tail cut is the p90 itself, so the tail count is 10%% of every key by construction",
			d.CycleSpreadMultiple)
	}
	return nil
}
