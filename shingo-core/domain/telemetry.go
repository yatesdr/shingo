package domain

import (
	"encoding/json"
	"strings"
	"time"

	"shingo/protocol"
)

// TelemetryEvent records a single state transition during a mission,
// including a robot position snapshot at that moment. Persisted via
// shingo-core/store/telemetry; lives in domain/ so handlers building
// mission-detail responses don't have to import the telemetry
// sub-package.
//
// Stage 2A.2 relocation. The store/telemetry package re-exports this
// type via `type Event = domain.TelemetryEvent`.
type TelemetryEvent struct {
	ID            int64     `json:"id"`
	OrderID       int64     `json:"order_id"`
	VendorOrderID string    `json:"vendor_order_id"`
	OldState      string    `json:"old_state"`
	NewState      string    `json:"new_state"`
	RobotID       string    `json:"robot_id"`
	RobotX        *float64  `json:"robot_x,omitempty"`
	RobotY        *float64  `json:"robot_y,omitempty"`
	RobotAngle    *float64  `json:"robot_angle,omitempty"`
	RobotBattery  *float64  `json:"robot_battery,omitempty"`
	RobotStation  string    `json:"robot_station"`
	BlocksJSON    string    `json:"blocks_json"`
	ErrorsJSON    string    `json:"errors_json"`
	Detail        string    `json:"detail"`
	CreatedAt     time.Time `json:"created_at"`
}

// TelemetryMission is the summary row for a completed mission. One
// row per completed Order, stamped at terminal state with vendor +
// core durations and serialised block / error / warning / notice
// payloads for post-hoc inspection.
//
// Stage 2A.2 relocation. The store/telemetry package re-exports this
// type via `type Mission = domain.TelemetryMission`.
type TelemetryMission struct {
	ID               int64              `json:"id"`
	OrderID          int64              `json:"order_id"`
	VendorOrderID    string             `json:"vendor_order_id"`
	RobotID          string             `json:"robot_id"`
	StationID        string             `json:"station_id"`
	OrderType        protocol.OrderType `json:"order_type"`
	SourceNode       string             `json:"source_node"`
	DeliveryNode     string             `json:"delivery_node"`
	TerminalState    string             `json:"terminal_state"`
	VendorCreated    *time.Time         `json:"vendor_created,omitempty"`
	VendorCompleted  *time.Time         `json:"vendor_completed,omitempty"`
	CoreCreated      *time.Time         `json:"core_created,omitempty"`
	CoreCompleted    *time.Time         `json:"core_completed,omitempty"`
	DurationMS       int64              `json:"duration_ms"`
	VendorDurationMS int64              `json:"vendor_duration_ms"`
	BlocksJSON       string             `json:"blocks_json"`
	ErrorsJSON       string             `json:"errors_json"`
	WarningsJSON     string             `json:"warnings_json"`
	NoticesJSON      string             `json:"notices_json"`
	RobotAlarmsJSON  string             `json:"robot_alarms_json"`
	CreatedAt        time.Time          `json:"created_at"`
}

// TelemetryFilter is the query DSL for mission-telemetry lookups. Lives
// in domain/ rather than store/telemetry because handlers parse HTTP
// query parameters into a filter and pass it through the service
// layer — the type is the contract between handler and service, not a
// persistence detail.
//
// Stage 2A.2 relocation. The store/telemetry package re-exports this
// type via `type Filter = domain.TelemetryFilter`.
type TelemetryFilter struct {
	StationID string
	RobotID   string
	State     string
	Since     *time.Time
	Until     *time.Time
	Limit     int
	Offset    int
}

// TelemetryStats provides aggregated mission metrics over a
// (typically) date-bounded set of TelemetryMission rows.
//
// Stage 2A.2 relocation. The store/telemetry package re-exports this
// type via `type Stats = domain.TelemetryStats`.
type TelemetryStats struct {
	TotalMissions int64   `json:"total_missions"`
	Completed     int64   `json:"completed"`
	Failed        int64   `json:"failed"`
	Cancelled     int64   `json:"cancelled"`
	AvgDurationMS int64   `json:"avg_duration_ms"`
	P50DurationMS int64   `json:"p50_duration_ms"`
	P95DurationMS int64   `json:"p95_duration_ms"`
	SuccessRate   float64 `json:"success_rate"`
}

// TelemetryStatsV2 is the corrected mission-stats shape for the dashboards
// (plan §3.A, §8 #5). The headline difference from TelemetryStats is the
// success-rate definition:
//
//	success_rate = Confirmed / (Confirmed + Failed) * 100
//
// Cancelled and Skipped are excluded from the denominator (a missing source
// bin the operator cancels is not a robot failure). System-initiated stops
// (grace/timeout/structural) count as failures, distinguished from
// operator cancels by ClassifyTermination on the order_history detail.
//
// Served by /api/missions/stats/v2 as a sibling of the legacy
// /api/missions/stats so existing consumers (current missions.js) keep
// seeing the old number until they migrate.
type TelemetryStatsV2 struct {
	Total     int64 `json:"total"`     // all terminal rows in the window
	Confirmed int64 `json:"confirmed"` // FINISHED / delivered / confirmed
	Failed    int64 `json:"failed"`    // hard failures + system-initiated stops
	Cancelled int64 `json:"cancelled"` // all cancels (shingo + rds + unclassified)
	// Cancelled origin split (Q-030); sum to Cancelled. Shown as the Cancelled
	// tile sub-stat so an anonymous "fleet order stopped" wedge is visible.
	CancelledShingo   int64   `json:"cancelled_shingo"`   // "cancelled by …" / "aborted by …"
	CancelledRDS      int64   `json:"cancelled_rds"`      // "fleet order stopped" (vendor-side)
	UnclassifiedStops int64   `json:"unclassified_stops"` // cancel detail matched no pattern
	Skipped           int64   `json:"skipped"`            // excluded from everything; reported for visibility
	SuccessRate       float64 `json:"success_rate"`       // Confirmed/(Confirmed+Failed)*100, 0 when denom 0
	// AvgExecutionMS is assignment→terminal (what the robot actually spent);
	// AvgDurationMS is created→terminal (lead time, includes queue/sourcing). The
	// Avg Duration tile headlines execution and shows lead as the sub-stat (Q-031).
	AvgExecutionMS int64 `json:"avg_execution_ms"`
	AvgDurationMS  int64 `json:"avg_duration_ms"`
	P50DurationMS  int64 `json:"p50_duration_ms"`
	P95DurationMS  int64 `json:"p95_duration_ms"`
}

// TelemetryBucket is one time-bucket of mission metrics for the trend charts
// (plan §3.B / §15.B). One endpoint returns every metric per bucket so the
// 2×2 grid (throughput, success rate, P50/P95 duration, cancellation rate)
// and the hero sparklines are all served by a single fetch ("fetched once",
// §3.B / §3.A). SuccessRate here is a bucket-level approximation —
// Confirmed/(Confirmed+Failed) using hard failures only, without the
// order_history stop reclassification GetStatsV2 does (too expensive
// per-bucket); the precise number lives on the hero.
type TelemetryBucket struct {
	BucketStart   time.Time `json:"bucket_start"`
	Total         int64     `json:"total"`
	Confirmed     int64     `json:"confirmed"`
	Failed        int64     `json:"failed"`
	Cancelled     int64     `json:"cancelled"`
	SuccessRate   float64   `json:"success_rate"`
	P50DurationMS int64     `json:"p50_ms"`
	P95DurationMS int64     `json:"p95_ms"`
}

// TelemetryBreakdownRow is one grouped slice of missions (by robot or route)
// for the §3.F breakdown panels: a label, its mission count, and average
// duration.
type TelemetryBreakdownRow struct {
	Label         string `json:"label"`
	Count         int64  `json:"count"`
	AvgDurationMS int64  `json:"avg_duration_ms"`
}

// Termination outcome buckets returned by ClassifyTermination.
const (
	OutcomeConfirmed = "confirmed"
	OutcomeFailed    = "failed"
	OutcomeCancelled = "cancelled"
	OutcomeSkipped   = "skipped"
	OutcomeOther     = "other"
)

// FailureReason is one row of the §3.G failure Pareto: a categorical reason,
// its count, and a few sample order IDs for the hover drill.
type FailureReason struct {
	Reason         string  `json:"reason"`
	Count          int64   `json:"count"`
	SampleOrderIDs []int64 `json:"sample_order_ids"`
}

// Failure-Pareto categories (plan §3.G). Stable display strings — the Pareto
// groups on them.
const (
	FailEmergency   = "Emergency stop"
	FailMotor       = "Motor fault"    // 5213x GVDD/FET/VDD motor faults (Q-026)
	FailBattery     = "Battery"        // 524xx/525xx connect/disconnect/low/temp
	FailHardware    = "Hardware fault" // PGV/fork-encoder/fork-pick — mechanical, distinct from blocked
	FailComms       = "Comms"          // laser data invalid / network
	FailPathPlan    = "Path planning"  // path-plan / charger faults
	FailBlocked     = "Robot blocked"  // out-of-path / slipping / skid
	FailSourceEmpty = "Source bin empty"
	FailManifest    = "Manifest mismatch"
	FailTimeout     = "Timeout"
	FailVendor      = "Vendor error"
	FailOther       = "Other"
)

// seerErr / seerBlock are the parsed shapes of mission_telemetry.errors_json
// and blocks_json. We decode them rather than substring-matching the raw text:
// the prior implementation lowercased the concatenated JSON and matched the
// literal "block", which hit the "block_id" *field name* present in every
// blocks_json row — classifying 100% of failures as "Robot blocked"
// (plant-data-findings.md Q-013). Matching only against parsed `code` values
// and parsed `desc` strings closes that false-positive.
type seerErr struct {
	Code int    `json:"code"`
	Desc string `json:"desc"`
}

type seerBlock struct {
	State    string `json:"state"`
	BlockID  string `json:"block_id"`
	Location string `json:"location"`
}

// seerRobotAlarm is one entry of mission_telemetry.robot_alarms_json — the
// per-mission snapshot of active robot alarms (5xxxx codes) projected from
// robot_alarm_log when a mission ends FAILED (Q-026). severity is
// fatal|error|warning|notice.
type seerRobotAlarm struct {
	Code     int    `json:"code"`
	Severity string `json:"severity"`
	Desc     string `json:"desc"`
}

// PrimaryFailureReason classifies a failed mission into one §3.G category by
// parsing its vendor error/block snapshots. Priority (highest first):
// emergency > blocked > source-empty > manifest > timeout > vendor > other.
//
// errors_json is a SEER array of {code, desc, …}; blocks_json an array of
// {state, block_id, location}. We classify on the numeric `code`
// (seerCodeCategory) and on parsed `desc` keywords (descCategory) — never on
// raw field names. On Springfield's live data this maps the dominant fleet
// code 60011 ("task failed. cannot replan!") to Vendor error rather than the
// false "Robot blocked"; the richer "why" (source-bin starvation, RDS down)
// lives in order_history.detail, not here (Q-016 / Q-013).
func PrimaryFailureReason(robotAlarmsJSON, blocksJSON, errorsJSON string) string {
	// Priority-1 source (Q-026): the robot's own alarm snapshot carries the
	// real hardware fault (5xxxx) — the rich signal errors_json (fleet 60011)
	// lacks. Pick the most severe alarm and map its code. Empty until the
	// robot_alarm_log ingestion + mission snapshot land (write side of Q-026).
	if cat := robotAlarmCategory(robotAlarmsJSON); cat != "" {
		return cat
	}
	best := ""
	var errs []seerErr
	if parseJSONArray(errorsJSON, &errs) {
		for _, e := range errs {
			if cat, ok := seerCodeCategory(e.Code); ok {
				best = higherFailPriority(best, cat)
			}
			if cat := descCategory(e.Desc); cat != "" {
				best = higherFailPriority(best, cat)
			}
		}
	}
	// Blocks carry the failing leg; its desc/location occasionally names a
	// category, but usually only "where" (a node name), not "why". Use it only
	// to refine when errors gave nothing.
	if best == "" {
		var blocks []seerBlock
		if parseJSONArray(blocksJSON, &blocks) {
			for _, b := range blocks {
				if !strings.EqualFold(b.State, "FAILED") {
					continue
				}
				if cat := descCategory(b.Location); cat != "" {
					best = higherFailPriority(best, cat)
				}
			}
		}
	}
	if best != "" {
		return best
	}
	// Some vendor failure with no category signal (the 60011 case once its
	// code mapping is absent) → Vendor error; truly empty → Other.
	if jsonArrayHasContent(errorsJSON) || jsonArrayHasContent(blocksJSON) {
		return FailVendor
	}
	return FailOther
}

// seerCodeCategory maps a SEER code to a category. Seeded from
// reference/Alarm Code_20260227.pdf (robot-level 5xxxx alarms) and Springfield
// live data (fleet 6xxxx codes — 60011 is 100% of FAILED missions today).
// Robot-alarm descriptions are also caught by descCategory; this map covers
// codes whose desc lacks a clean category keyword (notably the fleet codes).
// Extend as new codes are observed on a second plant (cross-plant drift is
// unverified — Hopkinsville was unreachable, see plant-data-findings.md).
func seerCodeCategory(code int) (string, bool) {
	// Specific codes take precedence over the ranges below (Q-026 Step 6 map).
	switch code {
	case 52200, 52201, 52313, 52718: // out-of-path / slipping / skid stop
		return FailBlocked, true
	case 52127, 52138: // PGV connection / fork encoder
		return FailHardware, true
	case 52116: // network
		return FailComms, true
	case 52716, 52717: // charger issues
		return FailPathPlan, true
	case 52503: // battery temperature
		return FailBattery, true
	}
	switch {
	case code >= 52130 && code <= 52139: // 5213x motor faults (GVDD/FET/VDD)
		return FailMotor, true
	case code >= 52160 && code <= 52169: // 5216x fork pick errors
		return FailHardware, true
	case code >= 52400 && code <= 52599: // 524xx/525xx battery connect/disconnect/low
		return FailBattery, true
	case code >= 52700 && code <= 52709: // 5270x path planning (52702 path plan failed)
		return FailPathPlan, true
	case code >= 52100 && code <= 52109: // 5210x laser data invalid
		return FailComms, true
	case (code >= 52000 && code <= 52099) || (code >= 55000 && code <= 55009): // e-stop range
		return FailEmergency, true
	case code >= 60000 && code < 70000:
		// Fleet-orchestration range (e.g. 60011 "task failed. cannot replan!").
		// No finer signal in errors_json — the real reason is in
		// order_history.detail. Honest bucket: vendor error.
		return FailVendor, true
	}
	return "", false
}

// robotAlarmCategory picks the most severe alarm from a robot_alarms_json
// snapshot and maps its code to a category (Q-026). Returns "" when absent or
// unmappable. Severity order: fatal > error > warning > notice.
func robotAlarmCategory(robotAlarmsJSON string) string {
	var alarms []seerRobotAlarm
	if !parseJSONArray(robotAlarmsJSON, &alarms) {
		return ""
	}
	best, bestSev := "", -1
	for _, a := range alarms {
		cat, ok := seerCodeCategory(a.Code)
		if !ok {
			continue
		}
		if sev := alarmSeverityRank(a.Severity); sev > bestSev {
			bestSev, best = sev, cat
		}
	}
	return best
}

func alarmSeverityRank(s string) int {
	switch strings.ToLower(s) {
	case "fatal":
		return 3
	case "error":
		return 2
	case "warning":
		return 1
	default: // notice / unknown
		return 0
	}
}

// descCategory matches category keywords against a parsed `desc`/`location`
// string only — never raw JSON. Keyword vocabulary aligns with the SEER alarm
// descriptions in the reference PDF (obstacle/blocked/emergency stop/timeout).
func descCategory(s string) string {
	d := strings.ToLower(s)
	switch {
	case strings.Contains(d, "emergency") || strings.Contains(d, "e-stop") || strings.Contains(d, "estop"):
		return FailEmergency
	case strings.Contains(d, "blocked") || strings.Contains(d, "obstacle"):
		return FailBlocked
	case strings.Contains(d, "no bin") || strings.Contains(d, "no unclaimed") ||
		strings.Contains(d, "no available bin") || strings.Contains(d, "starv") ||
		strings.Contains(d, "empty payload") || strings.Contains(d, "source bin"):
		return FailSourceEmpty
	case strings.Contains(d, "manifest") || strings.Contains(d, "mismatch"):
		return FailManifest
	case strings.Contains(d, "timeout") || strings.Contains(d, "timed out"):
		return FailTimeout
	}
	return ""
}

// failRank orders categories for the §3.G priority when more than one signal
// is present. Robot-hardware faults (Q-026) rank just below emergency and above
// the material-flow categories — a motor/battery/comms fault is a more
// definitive root cause than "blocked". The relative order of the original
// categories (emergency > blocked > source-empty > manifest > timeout > vendor
// > other) is preserved.
var failRank = map[string]int{
	FailEmergency: 12, FailMotor: 11, FailBattery: 10, FailHardware: 9,
	FailComms: 8, FailPathPlan: 7, FailBlocked: 6, FailSourceEmpty: 5,
	FailManifest: 4, FailTimeout: 3, FailVendor: 2, FailOther: 0,
}

// higherFailPriority returns whichever category ranks higher in the §3.G
// priority list (empty string ranks below everything).
func higherFailPriority(a, b string) string {
	if a == "" {
		return b
	}
	if failRank[b] > failRank[a] {
		return b
	}
	return a
}

// parseJSONArray unmarshals a JSON array string into v, returning false for
// empty/null/invalid input.
func parseJSONArray(s string, v any) bool {
	t := strings.TrimSpace(s)
	if t == "" || t == "null" || t == "[]" {
		return false
	}
	return json.Unmarshal([]byte(t), v) == nil
}

// jsonArrayHasContent reports whether a serialized JSON array string carries
// any entries (i.e. not "", "[]", or whitespace/null).
func jsonArrayHasContent(j string) bool {
	t := strings.TrimSpace(j)
	return t != "" && t != "[]" && t != "null"
}

// ClassifyTermination maps a mission's terminal_state — plus the detail of
// its terminal order_history transition — to a coarse outcome for the v2
// success-rate math (plan §8 #5).
//
// FINISHED/delivered/confirmed → confirmed; FAILED → failed; SKIPPED →
// skipped. STOPPED/cancelled is the ambiguous bucket: system-initiated stops
// (detail mentions grace/timeout/structural/abandon) are failures; operator
// cancels ("cancelled by …") and any unrecognised detail default to
// cancelled. The default is deliberately conservative — an unknown detail
// string should not silently inflate the failure rate. The exact detail
// substrings still need validation against live order_history data
// (slice-implementation-questions Q-005).
func ClassifyTermination(terminalState, detail string) string {
	switch strings.ToLower(strings.TrimSpace(terminalState)) {
	case "finished", "delivered", "confirmed":
		return OutcomeConfirmed
	case "failed":
		return OutcomeFailed
	case "skipped":
		return OutcomeSkipped
	case "stopped", "cancelled", "canceled":
		d := strings.ToLower(detail)
		// Actor-attributed cancels are deliberate cancels (Q-030), even if the
		// detail also mentions a failure-ish word — attribution wins. "aborted by"
		// was previously only caught by the conservative default.
		if strings.Contains(d, "cancelled by") || strings.Contains(d, "canceled by") ||
			strings.Contains(d, "aborted by") {
			return OutcomeCancelled
		}
		if strings.Contains(d, "grace") || strings.Contains(d, "timeout") ||
			strings.Contains(d, "structural") || strings.Contains(d, "abandon") {
			return OutcomeFailed
		}
		return OutcomeCancelled
	default:
		return OutcomeOther
	}
}

// SystemStopReason derives a categorical failure reason from a terminal
// order_history detail for system-initiated stops (grace timeouts, abandons,
// structural faults) that carry no robot_alarms/blocks/errors signal — the case
// where PrimaryFailureReason falls through to FailOther. Returns "" when the
// detail names no known system-stop category, so callers keep the generic Other
// bucket. Mirrors the failure keywords in ClassifyTermination (Q-013 Pareto
// fold-in). "grace" is checked before "timeout" because a grace-timeout detail
// usually contains both and "Grace timeout" is the more specific bucket.
func SystemStopReason(detail string) string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "grace"):
		return "Grace timeout"
	case strings.Contains(d, "abandon"):
		return "Abandoned"
	case strings.Contains(d, "structural"):
		return "Structural fault"
	case strings.Contains(d, "timeout"):
		return "Timeout"
	default:
		return ""
	}
}

// Cancel-origin buckets for the v2 stats split (Q-030). Only meaningful for
// rows ClassifyTermination placed in the cancelled bucket.
const (
	CancelOriginShingo       = "shingo"       // deliberate cancel via shingo: "cancelled by …" / "aborted by …"
	CancelOriginRDS          = "rds"          // vendor-side stop, unattributed: "fleet order stopped"
	CancelOriginUnclassified = "unclassified" // detail matched no known pattern — surfaced so unknowns don't hide
)

// ClassifyCancelOrigin splits a cancelled mission's terminal detail by origin
// (Q-030). The decision was to keep these as cancels (not reclassify the RDS
// stops as failures) but show shingo-origin vs RDS-origin separately, with an
// unclassified count so unknown detail strings stay visible rather than
// silently defaulting.
func ClassifyCancelOrigin(detail string) string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "cancelled by") || strings.Contains(d, "canceled by") ||
		strings.Contains(d, "aborted by"):
		return CancelOriginShingo
	case strings.Contains(d, "fleet order stopped"):
		return CancelOriginRDS
	default:
		return CancelOriginUnclassified
	}
}
