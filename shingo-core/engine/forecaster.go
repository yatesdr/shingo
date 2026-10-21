package engine

import (
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/store/demands"
	"shingocore/store/plantclaims"
	"shingocore/store/sourceability"
)

// forecaster.go — the forecast, written on the two-minute pass (D2, DESIGN-D2
// §2–§4; v4 §2.2–2.4, §2.9).
//
// A PLACE is the grain of one demand record: a cell's (process, payload, role),
// a loader binding's (node, payload). For each place that has no open demand the
// pass keeps one forecast record: the pick the firing door would make (with no
// order, holding nothing), the planner's verdict, and when the place will need
// it. The forecast creates no order and holds no bin (law 1, law 2): its plan is
// a belief, re-asked by predicate each pass (PlannedPickStands, asked as Anyone,
// so a live order's soft hold makes it stale at once).
//
// COST. One read of the unsettled records; per place with a standing plan one
// stale check; a re-plan only when the plan went stale, or when there is none
// and the payload has stock; a write only when the plan's bin or node, the
// verdict or the projection beyond its band changed. At rest: 1 + S reads and no
// write. Never on the debounced path.
//
// Keeper places are not forecast (F4): there is no empties drain rate, so their
// projection could never expire, and a forecast that cannot be scored is a
// rumour.

// forecastBand is the write predicate for a projection (F5, provisional, one
// constant, measured on the sim and then ruled): a moved need_at is written
// when it moved by more than this share of the time remaining, or a minute,
// whichever is more.
const forecastBand = 0.10

// forecastPlace is one place the pass forecasts.
type forecastPlace struct {
	birth  domain.ForecastBirth
	need   dispatch.SourceNeed
	gateBy string // the payload whose pool gates a re-plan when there is no plan; "" re-plans every pass
}

// forecast keeps every place's forecast record current. Best-effort: a failed
// read or write costs this pass's forecast and nothing else.
func (m *SourceabilityMonitor) forecast(in sourceability.Inputs, stations map[string]string,
	bindings []demands.RegistryEntry, totals map[string]int) {
	// WALL TIME, NOT THE PROCESS CLOCK. The rates are measured in it (the
	// ledger's applied_at is Postgres NOW()), the Edges stamp opens in it, and
	// the rest of this pass reads time.Now. Under the sim clock a projection
	// added a wall-measured distance to a sim-time now, and the score compared
	// an Edge's wall open with a sim-time prediction.
	now := time.Now().UTC()
	places := forecastPlaces(in, stations, bindings, totals, now)
	records, err := m.eng.db.ListUnsettledDemandRecords()
	if err != nil {
		m.eng.logFn("forecast: read the demand records: %v — no forecast this pass", err)
		return
	}
	byKey := make(map[string]domain.ForecastRecord, len(records))
	for _, r := range records {
		byKey[r.EpisodeKey] = r
	}
	inPopulation := make(map[string]bool, len(places))
	for _, p := range places {
		inPopulation[p.birth.EpisodeKey] = true
		rec, has := byKey[p.birth.EpisodeKey]
		if has && rec.Open {
			continue // an open demand renders as a demand; its orders carry the plan
		}
		// A DUE PLACE KEEPS THE MOMENT IT FELL DUE. At or below its level the
		// projection is now, and now slides every pass while the supply is in
		// flight; rewriting it would read as stock rising and churn the record.
		if has && rec.NeedAt != nil && p.birth.NeedAt != nil && !p.birth.NeedAt.After(now) {
			p.birth.NeedAt = rec.NeedAt
		}
		// A FORECAST THAT WILL NOT OPEN IS CLOSED BY THE PASS THAT WROTE IT
		// (forecaster_close.go). The next pass births the place's next one.
		if has {
			if reason := forecastClose(rec, p.birth.NeedAt, now); reason != "" {
				m.closeForecast(rec, reason, now)
				continue
			}
		}
		plan, verdict := (*domain.PlannedPick)(nil), ""
		if has {
			plan, verdict = rec.Plan, rec.PlanVerdict
		}
		replan := !has
		if has && rec.Plan != nil {
			stands, err := m.eng.db.PlannedPickStands(rec.Plan, domain.Anyone)
			if err != nil {
				m.eng.logFn("forecast: %s: check the plan: %v", p.birth.EpisodeKey, err)
				continue
			}
			replan = stands == nil
		} else if has {
			replan = p.gateBy == "" || in.Pool[p.gateBy] > 0
		}
		if replan {
			plan, verdict = m.forecastPick(p.need)
		}
		if !has {
			p.birth.OriginID = uuid.New().String()
			p.birth.Plan, p.birth.PlanVerdict = plan, verdict
			if _, err := m.eng.db.BirthForecast(p.birth); err != nil {
				m.eng.logFn("forecast: %s: birth: %v", p.birth.EpisodeKey, err)
			}
			continue
		}
		if samePick(plan, rec.Plan) && verdict == rec.PlanVerdict && !needAtMoved(rec.NeedAt, p.birth.NeedAt, now) {
			continue
		}
		if err := m.eng.db.UpdateForecast(rec.OriginID, plan, verdict, p.birth.NeedAt); err != nil {
			m.eng.logFn("forecast: %s: write: %v", p.birth.EpisodeKey, err)
		}
	}
	m.withdrawForecasts(records, inPopulation, now)
}

// forecastPick asks the planner the firing door's question and returns the
// plan and the verdict: a found bin is the plan and the verdict is ”; anything
// else has no plan and its cause is the verdict.
func (m *SourceabilityMonitor) forecastPick(need dispatch.SourceNeed) (*domain.PlannedPick, string) {
	if m.eng.dispatcher == nil {
		return nil, "no-planner" // the startup pass can beat Start's dispatcher
	}
	res := m.eng.dispatcher.ForecastPick(need)
	if dispatch.MapFinderOutcome(res) == dispatch.OutcomeFound && res.Bin != nil && res.Node != nil {
		return &domain.PlannedPick{BinID: res.Bin.ID, Node: res.Node.Name, Payload: res.Bin.PayloadCode,
			Status: string(res.Bin.Status), Confirmed: res.Bin.ManifestConfirmed}, ""
	}
	switch {
	case res.QueueCause != "":
		return nil, string(res.QueueCause)
	case res.QueueCode != "":
		return nil, string(res.QueueCode)
	case res.TermCode != "":
		return nil, res.TermCode
	}
	return nil, "no-source"
}

// forecastPlaces is the population: every running style's cell claim that is
// not a loader position and has a level (F1 b), one place per cell key, its
// first claim by sequence; and every monitored loader binding, one per
// threshold key. Read from what the pass already holds: no statement.
func forecastPlaces(in sourceability.Inputs, stations map[string]string, bindings []demands.RegistryEntry,
	totals map[string]int, now time.Time) []forecastPlace {
	var out []forecastPlace
	seen := map[string]bool{}
	keys := make([]plantclaims.ProcessKey, 0, len(in.Claims))
	for k := range in.Claims {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ProcessID != keys[j].ProcessID {
			return keys[i].ProcessID < keys[j].ProcessID
		}
		return keys[i].StyleID < keys[j].StyleID
	})
	for _, k := range keys {
		if k.StyleID == "" || in.ActiveStyles[k.ProcessID] != k.StyleID {
			continue
		}
		claims := append([]plantclaims.ClaimRow(nil), in.Claims[k]...)
		sort.SliceStable(claims, func(i, j int) bool { return claims[i].Seq < claims[j].Seq })
		for _, c := range claims {
			level := c.DemandLevel()
			if c.IsLoader() || level <= 0 || c.PayloadCode == "" || c.InboundSource == "" {
				continue
			}
			key := protocol.CellEpisodeKey(c.ProcessID, c.PayloadCode, c.Role)
			if seen[key] {
				continue
			}
			seen[key] = true
			p := forecastPlace{birth: domain.ForecastBirth{
				EpisodeKey: key, Kind: "cell", Direction: c.Role, StationID: stations[c.ProcessID],
				ProcessID: c.ProcessID, CoreNodeName: c.CoreNodeName, PayloadCode: c.PayloadCode,
				Threshold: level,
			}}
			if c.Role == protocol.ClaimRoleProduce {
				// A produce cell's supply is an empty carrier, needed when the
				// bin at the line fills to the level (F6 a: the produce rate).
				p.need = dispatch.SourceNeed{SourceNode: c.InboundSource, PayloadCode: c.PayloadCode,
					DeliveryNode: c.CoreNodeName, ProcessNode: c.CoreNodeName, SetDownNode: c.CoreNodeName,
					Intent: dispatch.IntentEmpty}
				p.birth.NeedAt = cellFillAt(c, level, in, now)
			} else {
				p.need = dispatch.SourceNeed{SourceNode: c.InboundSource, PayloadCode: c.PayloadCode,
					DeliveryNode: c.CoreNodeName, Intent: dispatch.IntentFull}
				p.gateBy = c.PayloadCode
				p.birth.NeedAt = cellNeedAt(c, level, in, now)
			}
			out = append(out, p)
		}
	}
	for _, b := range bindings {
		if b.PayloadCode == "" || b.ReplenishUOPThreshold <= 0 || b.InboundSource == "" {
			continue
		}
		key := protocol.ThresholdEpisodeKey(b.CoreNodeName, b.PayloadCode)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, forecastPlace{
			birth: domain.ForecastBirth{
				EpisodeKey: key, Kind: "threshold", StationID: b.StationID, CoreNodeName: b.CoreNodeName,
				PayloadCode: b.PayloadCode, Threshold: b.ReplenishUOPThreshold,
				NeedAt: projectNeedAt(totals[b.PayloadCode], b.ReplenishUOPThreshold, in.RatePerSec[b.PayloadCode], now),
			},
			need: dispatch.SourceNeed{SourceNode: b.InboundSource, PayloadCode: b.PayloadCode,
				DeliveryNode: b.CoreNodeName, ProcessNode: b.CoreNodeName, SetDownNode: b.CoreNodeName,
				Intent: dispatch.IntentEmpty},
		})
	}
	return out
}

// cellNeedAt projects a consume cell's need: the bin staged at the line, at the
// rate the line burns (its node's, else the payload's), down to the level.
func cellNeedAt(c plantclaims.ClaimRow, level int, in sourceability.Inputs, now time.Time) *time.Time {
	uop, staged := in.LineUOP[c.CoreNodeName]
	if !staged {
		return nil
	}
	return projectNeedAt(uop, level, sourceability.LineRate(c, in), now)
}

// cellFillAt projects a produce cell's need: the bin at the line, at the rate
// the line fills (its node's, else the payload's), up to the level. The same
// arithmetic as a consume cell's with the direction turned round: the distance
// is the level less what the bin holds.
func cellFillAt(c plantclaims.ClaimRow, level int, in sourceability.Inputs, now time.Time) *time.Time {
	uop, staged := in.LineUOP[c.CoreNodeName]
	if !staged {
		return nil
	}
	return projectNeedAt(level, uop, sourceability.ProduceLineRate(c, in), now)
}

// projectNeedAt is now + max(0, remaining − level) / rate (v3.1 §4), arithmetic
// only. nil when there is no rate: unprojectable, never "now".
func projectNeedAt(remaining, level int, ratePerSec float64, now time.Time) *time.Time {
	if ratePerSec <= 0 {
		return nil
	}
	ahead := math.Max(0, float64(remaining-level)) / ratePerSec
	t := now.Add(time.Duration(ahead * float64(time.Second))).Truncate(time.Second)
	return &t
}

// samePick: the same bin at the same node, or both none.
func samePick(a, b *domain.PlannedPick) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.BinID == b.BinID && a.Node == b.Node
}

// needAtMoved reports a projection that moved past the band (F5): by more than
// forecastBand of the time remaining to the old projection, or a minute,
// whichever is more; or that became known or unknown.
func needAtMoved(old, cur *time.Time, now time.Time) bool {
	if old == nil || cur == nil {
		return (old == nil) != (cur == nil)
	}
	band := time.Duration(forecastBand * float64(old.Sub(now)))
	if band < time.Minute {
		band = time.Minute
	}
	d := cur.Sub(*old)
	if d < 0 {
		d = -d
	}
	return d > band
}
