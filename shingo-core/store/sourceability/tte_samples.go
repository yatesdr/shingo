package sourceability

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// TTERetention is how long a kept projection is worth keeping.
//
// Long enough to cover several consumption-rate windows plus the slack to
// notice a bad one and re-score against it; short enough that the table stays
// traffic rather than a ledger. Nothing reads a sample older than the window it
// is scoring, so a longer horizon would buy storage and no answer.
const TTERetention = 45 * 24 * time.Hour

// maxSampleParams keeps one INSERT inside Postgres' 65535-parameter ceiling.
// Ten placeholders per row, chunked well under it — a plant does not come
// close, but the failure if it ever did would be a whole pass's samples lost to
// a driver error on a path whose whole job is to be unobtrusive.
const sampleChunkRows = 1000

// RecordTTESamples writes one pass's time-to-empty projections and ages out the
// ones past retention.
//
// ONE MULTI-ROW INSERT AND ONE DELETE, per full pass — the two-minute cadence
// that already exists. Not per claim, and not on the 300 ms debounced path:
// see the call site in recomputeAll for why the debounce must stay clean.
//
// computed_at is POSTGRES' now(), not a Go timestamp, so every row of a pass
// shares one instant and "the samples from that pass" is expressible without a
// tolerance. It also keeps the sample clock and the demand_origins.opened_at
// clock the score compares it against on the same server — a workstation whose
// clock is hours out (this estate has one) would otherwise produce error
// figures that are really clock skew.
//
// BEST-EFFORT BY CONTRACT. The caller logs and continues: losing a history row
// must not stop the monitor telling Edge what it can source. Same rule
// persistChanges already follows.
func RecordTTESamples(db *sql.DB, samples []TTESample, retain time.Duration) error {
	if err := insertTTESamples(db, samples); err != nil {
		return err
	}
	return pruneTTESamples(db, retain)
}

func insertTTESamples(db *sql.DB, samples []TTESample) error {
	for start := 0; start < len(samples); start += sampleChunkRows {
		end := min(start+sampleChunkRows, len(samples))
		if err := insertTTEChunk(db, samples[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func insertTTEChunk(db *sql.DB, samples []TTESample) error {
	if len(samples) == 0 {
		return nil
	}
	var (
		rows = make([]string, 0, len(samples))
		args = make([]any, 0, len(samples)*10)
	)
	for _, s := range samples {
		n := len(args)
		rows = append(rows, fmt.Sprintf("(NOW(),$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9, n+10))
		// A projection that is not Known has NO time-to-empty — nothing staged,
		// or no consumption in the rate window. It stores as NULL rather than 0:
		// zero reads as "empty right now", which is the opposite of what an
		// unknown means, and the score counts NULLs as coverage blind spots.
		var tte any
		if s.Line.Known {
			tte = s.Line.TimeToEmpty.Seconds()
		}
		args = append(args,
			s.ProcessID, s.StyleID, s.Line.NodeName, s.Line.PayloadCode,
			s.Line.UOPRemaining, s.Line.RatePerSec, tte,
			string(s.StyleStatus), s.ReorderPoint, s.Line.RateGrain)
	}
	_, err := db.Exec(`INSERT INTO tte_samples
		(computed_at, process_id, style_id, core_node_name, payload_code,
		 uop_remaining, rate_per_sec, tte_seconds, style_status, reorder_point, rate_grain)
		VALUES `+strings.Join(rows, ","), args...)
	if err != nil {
		return fmt.Errorf("sourceability: insert tte samples: %w", err)
	}
	return nil
}

// pruneTTESamples drops samples past retention. Runs in the same pass as the
// insert rather than as its own sweep: the cadence is already right, and a
// separate timer would be a second thing to configure, start and fail silently.
func pruneTTESamples(db *sql.DB, retain time.Duration) error {
	secs := retain.Seconds()
	if secs <= 0 {
		return nil
	}
	_, err := db.Exec(
		`DELETE FROM tte_samples WHERE computed_at < NOW() - make_interval(secs => $1)`, secs)
	if err != nil {
		return fmt.Errorf("sourceability: prune tte samples: %w", err)
	}
	return nil
}
