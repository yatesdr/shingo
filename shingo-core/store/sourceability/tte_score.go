package sourceability

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"
)

// Scoring the kept forecast against the demand it predicted.
//
// A sample says "this line runs dry in N seconds". A demand episode opening at
// that place is the plant saying "it is empty now". The distance between the
// two is the forecast's error, and it is the only thing that can say whether
// the near tier is worth building on.
//
// THE READER EXISTS IN THE SAME CHANGE AS THE SAMPLE. A recorded number with no
// reader is a table that grows for a quarter and answers nothing; the point of
// keeping the projection is to grade it.

// TTEScore is one demand episode set beside the last forecast made about its
// place before it opened.
type TTEScore struct {
	OriginID     string
	EpisodeKey   string
	Kind         string
	ProcessID    string
	CoreNodeName string
	PayloadCode  string
	OpenedAt     time.Time
	// TriggerKind distinguishes the level predicate firing (autoreorder) from
	// an operator pressing the button. They are not the same event to score
	// against: an operator can request on a node the system considers fine, so
	// a large error there may be a correct forecast and a discretionary call.
	TriggerKind string

	// HasSample is false when no projection was on record for this place before
	// the episode opened. These are the BLIND SPOTS and they are the first
	// number to read — a forecast that is accurate on the third of episodes it
	// covers is not a forecast the plant can run on.
	HasSample bool
	// PredictedAt is the pass that made the surviving forecast.
	PredictedAt time.Time
	// PredictedEmptyAt is PredictedAt + the projected time-to-empty.
	PredictedEmptyAt time.Time
	// ErrorSeconds = PredictedEmptyAt - OpenedAt. POSITIVE means the guess was
	// LATE: the forecast still had time on the clock when the plant said it was
	// empty. Negative means it called the line dry early.
	ErrorSeconds float64
	// StyleStatus is the verdict the predicting pass reached — the state of the
	// world the forecast was made in.
	StyleStatus string
}

// ScoreTTE pairs every cell and threshold episode opened since `since` with the
// last projection made about its place before it opened.
//
// TWO ARMS, NOT ONE JOIN WITH AN OR, because the two kinds are keyed on
// different things and an OR across them defeats both indexes on tte_samples.
//
//   - THRESHOLD episodes name a Core node: joined on (core_node_name,
//     payload_code).
//   - CELL episodes name a PROCESS and no node at all — the grain of a cell
//     episode is the process, see protocol.CellEpisodeKey — so they join on
//     (process_id, payload_code).
//
// THE PROCESS JOIN IS A PLAIN EQUALITY AND NEEDS NO TRANSLATION.
// demand_origins.process_id holds the Edge process NAME, the same string as
// style_claims.process_id, which is what a sample's ProcessID is copied from.
// Migration v63 retyped the column from the Edge SQLite row id to the name for
// exactly this reason and says so in its header; protocol.CellEpisodeKey
// documents the same identity from the other side. Rows written before v63
// would hold a number-as-string that joins nothing, and v63 records that no
// plant had run v59 — so only a dev box or the sim can hold one.
//
// A LEFT JOIN, deliberately: an episode with no prior sample is a row in the
// output with HasSample false, not a row that vanishes. Dropping them would
// make the forecast look better the less of the plant it covered.
func ScoreTTE(db *sql.DB, since time.Time) ([]TTEScore, error) {
	const arm = `
		SELECT o.origin_id, o.episode_key, o.kind, o.process_id, o.core_node_name,
		       o.payload_code, o.opened_at, o.trigger_kind,
		       s.computed_at, s.tte_seconds, s.style_status
		FROM demand_origins o
		LEFT JOIN LATERAL (
			SELECT t.computed_at, t.tte_seconds, t.style_status
			FROM tte_samples t
			WHERE %s
			  AND t.payload_code = o.payload_code
			  AND t.computed_at < o.opened_at
			  AND t.tte_seconds IS NOT NULL
			ORDER BY t.computed_at DESC
			LIMIT 1
		) s ON TRUE
		WHERE o.opened_at >= $1 AND o.kind = '%s'`

	q := fmt.Sprintf(arm, "t.core_node_name = o.core_node_name", "threshold") +
		"\nUNION ALL\n" +
		fmt.Sprintf(arm, "t.process_id = o.process_id", "cell") +
		"\nORDER BY opened_at"

	rows, err := db.Query(q, since)
	if err != nil {
		return nil, fmt.Errorf("sourceability: score tte: %w", err)
	}
	defer rows.Close()

	var out []TTEScore
	for rows.Next() {
		var (
			sc          TTEScore
			computedAt  sql.NullTime
			tteSeconds  sql.NullFloat64
			styleStatus sql.NullString
		)
		if err := rows.Scan(&sc.OriginID, &sc.EpisodeKey, &sc.Kind, &sc.ProcessID,
			&sc.CoreNodeName, &sc.PayloadCode, &sc.OpenedAt, &sc.TriggerKind,
			&computedAt, &tteSeconds, &styleStatus); err != nil {
			return nil, fmt.Errorf("sourceability: scan tte score: %w", err)
		}
		if computedAt.Valid && tteSeconds.Valid {
			sc.HasSample = true
			sc.PredictedAt = computedAt.Time
			sc.PredictedEmptyAt = computedAt.Time.Add(
				time.Duration(tteSeconds.Float64 * float64(time.Second)))
			sc.ErrorSeconds = sc.PredictedEmptyAt.Sub(sc.OpenedAt).Seconds()
			sc.StyleStatus = styleStatus.String
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// TTEScoreAggregate is the per-place summary the owner reads.
type TTEScoreAggregate struct {
	// Place is the node for a threshold episode and the process for a cell one
	// — whichever the episode's kind actually identifies.
	//
	// A CELL EPISODE MAY CARRY NO NODE. core_node_name on demand_origins
	// defaults to '' and a cell episode names a process, so grouping every kind
	// on the node alone would collapse every cell episode in the plant into one
	// nameless bucket. Keyed on what the kind identifies instead.
	Place       string
	Kind        string
	PayloadCode string

	Episodes int
	// Blind is the count with no prior projection, and BlindFraction the share.
	// READ THIS FIRST: the error figures describe only the covered episodes,
	// and a low error over a third of the plant is not a usable forecast.
	Blind         int
	BlindFraction float64
	// MedianErrorSeconds and P90ErrorSeconds are over the COVERED episodes
	// only. P90 is taken on the ABSOLUTE error — the tail worth knowing is "how
	// wrong does it get", in either direction, and signed p90 would hide a
	// large early call behind a large late one.
	MedianErrorSeconds float64
	P90AbsErrorSeconds float64
}

// AggregateTTEScores summarises per (place, kind, payload).
//
// PURE, so it is fixture-tested without a database — the same split this
// package already keeps between Compute and read.go. Percentiles are taken in
// Go rather than SQL for the same reason.
func AggregateTTEScores(scores []TTEScore) []TTEScoreAggregate {
	type bucket struct {
		agg  TTEScoreAggregate
		errs []float64
	}
	byPlace := map[string]*bucket{}
	var order []string

	for _, s := range scores {
		place := s.CoreNodeName
		if s.Kind == "cell" {
			place = s.ProcessID
		}
		k := place + "\x00" + s.Kind + "\x00" + s.PayloadCode
		b, ok := byPlace[k]
		if !ok {
			b = &bucket{agg: TTEScoreAggregate{Place: place, Kind: s.Kind, PayloadCode: s.PayloadCode}}
			byPlace[k] = b
			order = append(order, k)
		}
		b.agg.Episodes++
		if s.HasSample {
			b.errs = append(b.errs, s.ErrorSeconds)
		} else {
			b.agg.Blind++
		}
	}

	out := make([]TTEScoreAggregate, 0, len(order))
	for _, k := range order {
		b := byPlace[k]
		if b.agg.Episodes > 0 {
			b.agg.BlindFraction = float64(b.agg.Blind) / float64(b.agg.Episodes)
		}
		b.agg.MedianErrorSeconds = median(b.errs)
		abs := make([]float64, len(b.errs))
		for i, e := range b.errs {
			abs[i] = math.Abs(e)
		}
		b.agg.P90AbsErrorSeconds = percentile(abs, 0.90)
		out = append(out, b.agg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Place != out[j].Place {
			return out[i].Place < out[j].Place
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].PayloadCode < out[j].PayloadCode
	})
	return out
}

// median returns 0 for an empty set — the caller reads Episodes and Blind to
// tell "no error" from "nothing to measure", and those are different questions
// a single float cannot answer.
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// percentile takes the NEAREST RANK, not an interpolation. At the handful of
// episodes one place produces in a rate window, interpolating invents a value
// between two real observations and reads as more precision than the sample
// size carries.
func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	rank := min(max(int(math.Ceil(p*float64(len(s))))-1, 0), len(s)-1)
	return s[rank]
}
