package service

import (
	"fmt"
	"sort"
	"time"

	"shingocore/scenemap"
	"shingocore/store/robotconfidence"
)

// The localization board's payload.
//
// ASSEMBLED HERE, NOT IN THE HANDLER, and that is the depguard rule doing real
// work rather than being satisfied lexically: www may not reach into a store
// sub-package, so every type below is owned by this package and the handler
// never names robotconfidence or sceneversion. The one time that rule was bent
// on this branch — handlers_nodes.go importing store/sceneversion — two
// independent reviewers caught it.
//
// GEOMETRY IS NOT IN HERE. The page draws lanes from /api/map/edges, which
// already exists and which the kiosk map already consumes. Shipping coordinates
// again under a confidence URL would give the plant two queryable copies of one
// network with nothing saying which wins — the same objection that keeps the
// .smap's curves unparsed. This payload is STATE, keyed by lane, and the page
// joins it to geometry with the same laneKey() the renderer dedups with.

// BoardBand is the vendor's own four-way split, inherited so the page reads the
// way RoboShop reads.
type BoardBand string

const (
	BandGood  BoardBand = "good"  // >= 0.80
	BandFair  BoardBand = "fair"  // 0.30 - 0.80
	BandPoor  BoardBand = "poor"  // > 0
	BandBlind BoardBand = "blind" // exactly 0 — every reading was a no-estimate
	// BandNoData is NOT a band, it is the absence of one. Kept distinct because
	// a lane nobody drove and a lane that answered zero are opposite findings
	// and rendering them alike is the failure this whole design removes.
	BandNoData BoardBand = "nodata"
)

// BoardLane is one physical lane's state over the window.
type BoardLane struct {
	Area string `json:"area"`
	Lane string `json:"lane"`
	// P50 is an ESTIMATE from the summed daily histograms, accurate to one bin
	// width — not a reading any robot produced. The daily percentiles are
	// exact; this one cannot be, and the field name on the wire says
	// "estimate" so a reader cannot mistake the two.
	P50Estimate *float64  `json:"p50_estimate"`
	Band        BoardBand `json:"band"`
	Samples     int       `json:"samples"`
	SamplesGood int       `json:"samples_good"`
	// SentinelSamples is the no-estimate count. The annotation LEADS with this
	// rate, because routing into a reflector-declared zone makes the
	// conditioned average go UP.
	SentinelSamples int `json:"sentinel_samples"`
	Robots          int `json:"robots"`
	// Days is how many days of data the window actually found, and it travels
	// on every lane rather than once on the payload: "30 d" over two days of
	// data is the label-without-its-window failure the guide's own worked
	// example describes.
	Days int `json:"days"`
	// Versions > 1 means the lane was EDITED inside the window, so a single
	// number over it is averaging across a change.
	Versions int `json:"versions"`
	// Changed is the only mark that answers "what did I touch".
	Changed bool `json:"changed"`
	// BelowMinN is true when the window holds too few readings to say
	// anything. The page greys these rather than hiding them — absence reads
	// as fine.
	BelowMinN bool `json:"below_min_n"`
	// Hist is the window's distribution, for the selected-lane panel. It is
	// the only mark that can separate "consistently mediocre at 0.5" from
	// "excellent half the time, blind the rest" — two plants with the same
	// p50 and opposite remedies.
	Hist []int32 `json:"hist,omitempty"`
	// HistIncomplete says the distribution covers less than the window claims,
	// because some day carried no histogram.
	HistIncomplete bool `json:"hist_incomplete,omitempty"`
}

// BoardArea is one declared zone: its shape if we have it, its numbers if we
// have them, and honestly nothing where we have neither.
//
// A ZONE'S STATISTICS AND ITS SHAPE COME FROM DIFFERENT TRANSPORTS. The numbers
// are rolled up from readings attributed by the id the robot reports; the
// outline is parsed from the .smap, fetched on its own gate. So a zone can have
// numbers and no polygon — which is every plant between the confidence
// collection starting and the first successful map fetch — or a polygon and no
// numbers, which is a zone nobody drove through.
//
// Both appear. Keying this list to geometry would have left the numbers written
// and unread, which is the defect this whole line of work exists to remove, and
// an earlier cut of this payload did exactly that.
type BoardArea struct {
	Name string `json:"name"`
	// Class is what predicts a miss — every ReflectorArea carrying traffic
	// loses 23-71% of its readings and neither LocConfigArea loses any. Empty
	// when the map sync has not run.
	Class string `json:"class"`
	// Polygon is nil when the zone has readings but no geometry yet. The page
	// must render such a zone in the panel and simply not draw it.
	Polygon []scenemap.Point `json:"polygon,omitempty"`
	// ReflectorCount travels as PROVENANCE and must not drive a mark, a badge
	// or a band: measured, it has no predictive power over the no-estimate
	// rate and the sign runs backwards. It is here because "this declared
	// reflector zone contains zero reflectors" is the most actionable sentence
	// this work produced.
	ReflectorCount int `json:"reflector_count"`

	// HasStats separates "no readings in this window" from "we did not roll
	// this zone up", which absence alone cannot.
	HasStats bool `json:"has_stats"`
	// P50Estimate is over every tick with a no-estimate counted as the zero it
	// is — an ESTIMATE from summed histograms, not a reading any robot
	// produced.
	P50Estimate     *float64 `json:"p50_estimate,omitempty"`
	Samples         int      `json:"samples"`
	SamplesGood     int      `json:"samples_good"`
	SentinelSamples int      `json:"sentinel_samples"`
	Robots          int      `json:"robots"`
	Days            int      `json:"days"`
}

// BoardReflector is a reference mark: present, and not predictive.
type BoardReflector struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// BoardPlant is the baseline every change annotation is required to carry.
//
// NOT A HEALTH SCORE. A single plant-level figure that stood alone would hide
// exactly the local problem the page exists to find. It is here because the
// annotation must always show "plant +0.06 same period" — a lane that rose 0.09
// while the plant rose 0.08 has not improved — so this is where that number
// comes from rather than a new concept.
type BoardPlant struct {
	P50Estimate *float64 `json:"p50_estimate"`
	Samples     int      `json:"samples"`
	// Hist is the plant distribution. Its SHAPE is the finding: a spike at zero
	// beside a spike at 0.9 is a plant with dead zones, and a broad hump at 0.5
	// is a plant that is uniformly marginal. They share a p50 and need opposite
	// work.
	Hist []int32 `json:"hist"`
	// Bands counts LANES per band, which is a different question from the
	// distribution of readings above and the one triage actually asks: how much
	// of the map is red today.
	Bands map[BoardBand]int `json:"bands"`
}

// BoardWindow says what was asked for and what could actually be answered.
type BoardWindow struct {
	Label string    `json:"label"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	// RequestedDays and DataDays are both here on purpose. A control whose
	// label promises a window the record cannot cover is the failure this
	// project has spent three rounds removing, so the gap is reported rather
	// than left for a reader to infer from thin numbers.
	RequestedDays int `json:"requested_days"`
	DataDays      int `json:"data_days"`
}

// LocalizationBoard is the whole payload.
type LocalizationBoard struct {
	Window     BoardWindow      `json:"window"`
	Lanes      []BoardLane      `json:"lanes"`
	Areas      []BoardArea      `json:"areas"`
	Reflectors []BoardReflector `json:"reflectors"`
	Diffs      []SceneDiff      `json:"diffs"`
	Plant      BoardPlant       `json:"plant"`
	MinSamples int              `json:"min_samples"`
}

// BoardMinSamples is the threshold below which a lane is greyed rather than
// banded.
//
// 20, from the measured time-to-threshold: p25 3.6 h, median 6.7 h, p75 20.0 h.
// 79% of lanes reach it in a day. The number is carried on the payload so the
// page renders the same threshold the server applied, rather than a second
// copy that can drift.
const BoardMinSamples = 20

// LocalizationBoardAt assembles the board for a window ending at `to`.
//
// The window is day-grained and `to` is exclusive, matching the roll-up's own
// day boundaries — a board that sliced days differently from the table it reads
// would report partial days as short ones.
func (s *NodeService) LocalizationBoardAt(label string, days int, to time.Time) (LocalizationBoard, error) {
	var out LocalizationBoard
	if days <= 0 {
		return out, fmt.Errorf("localization board: window must be at least one day, got %d", days)
	}
	toDay := to.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	fromDay := toDay.AddDate(0, 0, -days)
	out.Window = BoardWindow{Label: label, From: fromDay, To: toDay, RequestedDays: days}
	out.MinSamples = BoardMinSamples

	windows, err := s.db.LaneWindows(fromDay, toDay)
	if err != nil {
		return out, err
	}
	changed, err := s.db.LanesChangedIn(fromDay, toDay)
	if err != nil {
		return out, err
	}

	// The zone and reflector geometry is read AT THE END of the window, not at
	// now: a board showing last month must draw the walls that were there,
	// which is the whole reason every scene query takes an instant.
	areas, err := s.db.SceneAreasAt(toDay)
	if err != nil {
		return out, err
	}
	refl, err := s.db.SceneReflectorsAt(toDay)
	if err != nil {
		return out, err
	}
	diffs, err := s.RecentSceneDiffsWithLanes(50)
	if err != nil {
		return out, err
	}

	var plantHist robotconfidence.Hist
	bands := map[BoardBand]int{}
	maxDays := 0

	out.Lanes = make([]BoardLane, 0, len(windows))
	for key, w := range windows {
		lane := BoardLane{
			Area:            w.Area,
			Lane:            w.Lane,
			Samples:         w.Samples,
			SamplesGood:     w.SamplesGood,
			SentinelSamples: w.SentinelSamples,
			Days:            w.Days,
			Versions:        w.Versions,
			Changed:         changed[key],
			HistIncomplete:  w.HistIncomplete,
			Hist:            w.Hist.Slice(),
		}
		if w.Days > maxDays {
			maxDays = w.Days
		}
		plantHist.Merge(w.Hist)

		if v, ok := w.PercentileEstimate(0.50); ok {
			lane.P50Estimate = &v
		}
		lane.BelowMinN = w.Samples < BoardMinSamples
		lane.Band = bandFor(lane.P50Estimate, w.Samples)
		bands[lane.Band]++
		out.Lanes = append(out.Lanes, lane)
	}
	out.Window.DataDays = maxDays

	out.Plant = BoardPlant{
		Samples: plantHist.Total(),
		Hist:    plantHist.Slice(),
		Bands:   bands,
	}
	if v, ok := plantHist.PercentileEstimate(0.50); ok {
		out.Plant.P50Estimate = &v
	}

	// Zones are the UNION of what has a shape and what has numbers, so neither
	// half can hide the other. Ordered by id for a stable panel.
	zones, err := s.db.AreaWindows(fromDay, toDay)
	if err != nil {
		return out, err
	}
	byName := map[string]*BoardArea{}
	order := []string{}
	for _, a := range areas {
		n := scenemap.NormalizeAreaID(a.Name)
		byName[n] = &BoardArea{
			Name: n, Class: a.Class, Polygon: a.Polygon,
			ReflectorCount: a.ReflectorCount,
		}
		order = append(order, n)
	}
	for n, w := range zones {
		z := byName[n]
		if z == nil {
			// Numbers with no geometry: the zone is real, we just cannot draw
			// it yet. It belongs in the panel, not in the bin.
			z = &BoardArea{Name: n}
			byName[n] = z
			order = append(order, n)
		}
		z.HasStats = true
		z.Samples, z.SamplesGood = w.Samples, w.SamplesGood
		z.SentinelSamples, z.Robots, z.Days = w.SentinelSamples, w.Robots, w.Days
		if z.Class == "" {
			z.Class = w.Class
		}
		if v, ok := w.PercentileEstimate(0.50); ok {
			z.P50Estimate = &v
		}
	}
	sort.Strings(order)
	out.Areas = make([]BoardArea, 0, len(order))
	for _, n := range order {
		out.Areas = append(out.Areas, *byName[n])
	}
	out.Reflectors = make([]BoardReflector, 0, len(refl))
	for _, r := range refl {
		out.Reflectors = append(out.Reflectors, BoardReflector{X: r.X, Y: r.Y})
	}
	out.Diffs = diffs
	return out, nil
}

// bandFor maps an estimate onto the vendor's bands.
//
// NO DATA IS NOT A BAND. A lane nobody drove and a lane that answered zero are
// opposite findings, and the whole design exists because rendering them alike
// reads as fine. Below the minimum n the lane is still banded nodata rather
// than dropped — absence reads as fine too.
func bandFor(p50 *float64, samples int) BoardBand {
	if p50 == nil || samples < BoardMinSamples {
		return BandNoData
	}
	switch v := *p50; {
	case v >= 0.80:
		return BandGood
	case v >= 0.30:
		return BandFair
	case v > 0:
		return BandPoor
	default:
		// Exactly zero. Every reading was a no-estimate — the lane is blind,
		// and the sentinel bin is never interpolated precisely so this stays
		// distinguishable from "very poor".
		return BandBlind
	}
}
