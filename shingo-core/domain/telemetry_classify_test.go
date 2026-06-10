package domain

import "testing"

// TestClassifyTermination pins the v2 success-rate classifier (plan §8 #5):
// confirmed states, hard failures, skipped, and the ambiguous
// STOPPED/cancelled split (operator cancel vs system stop). No DB needed —
// this is the pure logic the success-rate math depends on.
func TestClassifyTermination(t *testing.T) {
	cases := []struct {
		state  string
		detail string
		want   string
	}{
		// Confirmed family.
		{"FINISHED", "", OutcomeConfirmed},
		{"delivered", "", OutcomeConfirmed},
		{"confirmed", "", OutcomeConfirmed},
		{"finished", "anything", OutcomeConfirmed}, // case-insensitive

		// Hard failures.
		{"FAILED", "robot blocked", OutcomeFailed},
		{"failed", "", OutcomeFailed},

		// Skipped is excluded from everything but still classified.
		{"SKIPPED", "", OutcomeSkipped},
		{"skipped", "duplicate", OutcomeSkipped},

		// STOPPED/cancelled — system-initiated stops count as failures.
		{"STOPPED", "grace period expired", OutcomeFailed},
		{"STOPPED", "heartbeat timeout — marked stale", OutcomeFailed},
		{"STOPPED", "structural: no path to source", OutcomeFailed},
		{"cancelled", "abandoned: stuck queued past TTL", OutcomeFailed},
		{"Stopped", "GRACE", OutcomeFailed}, // case-insensitive on both fields

		// STOPPED/cancelled — operator cancels and unknown details default to
		// cancelled (conservative: don't inflate the failure rate).
		{"STOPPED", "cancelled by operator jdoe", OutcomeCancelled},
		{"cancelled", "cancelled by admin", OutcomeCancelled},
		{"STOPPED", "", OutcomeCancelled},
		{"canceled", "some unrecognised reason", OutcomeCancelled},
		// "aborted by" is an operator cancel (Q-030) — previously only the default.
		{"STOPPED", "aborted by operator", OutcomeCancelled},
		// Attribution wins over a failure-ish word in the same detail.
		{"cancelled", "aborted by operator (timeout while staging)", OutcomeCancelled},
		// "fleet order stopped" (RDS-origin) stays a cancel — no failure pattern.
		{"STOPPED", "fleet order stopped", OutcomeCancelled},

		// Anything else.
		{"weird-state", "", OutcomeOther},
		{"", "", OutcomeOther},
	}
	for _, c := range cases {
		if got := ClassifyTermination(c.state, c.detail); got != c.want {
			t.Errorf("ClassifyTermination(%q, %q) = %q, want %q", c.state, c.detail, got, c.want)
		}
	}
}

// TestClassifyCancelOrigin pins the Q-030 cancel-origin split: shingo-origin
// (actor-attributed cancel/abort), RDS-origin ("fleet order stopped"), and the
// unclassified fallback that keeps unknown detail strings visible.
func TestClassifyCancelOrigin(t *testing.T) {
	cases := []struct {
		detail string
		want   string
	}{
		{"cancelled by admin", CancelOriginShingo},
		{"canceled by jdoe", CancelOriginShingo},
		{"aborted by operator", CancelOriginShingo},
		{"Cancelled By Operator", CancelOriginShingo}, // case-insensitive
		{"fleet order stopped", CancelOriginRDS},
		{"FLEET ORDER STOPPED", CancelOriginRDS},
		{"", CancelOriginUnclassified},
		{"some reason we've never seen", CancelOriginUnclassified},
	}
	for _, c := range cases {
		if got := ClassifyCancelOrigin(c.detail); got != c.want {
			t.Errorf("ClassifyCancelOrigin(%q) = %q, want %q", c.detail, got, c.want)
		}
	}
}

// TestPrimaryFailureReason pins the §3.G failure classifier against the REAL
// vendor JSON shapes from Springfield (plant-data-findings.md Q-013): errors_json
// is [{code,desc,times,…}], blocks_json is [{state,block_id,location}]. The
// load-bearing case is the regression: every blocks_json row carries a
// "block_id" field, and code 60011 is 100% of live failures — the classifier
// must return Vendor error, NOT the old false-positive "Robot blocked".
func TestPrimaryFailureReason(t *testing.T) {
	cases := []struct {
		name, blocks, errors, want string
	}{
		{
			"regression: 60011 + block_id field is NOT Robot blocked",
			`[{"state":"FAILED","block_id":"SMN_001_unload","location":"SMN_001"}]`,
			`[{"code":60011,"desc":"task failed. cannot replan!","times":1}]`,
			FailVendor,
		},
		{"robot-blocked code (52200) + desc", `[]`, `[{"code":52200,"desc":"robot is blocked"}]`, FailBlocked},
		{"blocked via obstacle desc", `[]`, `[{"code":0,"desc":"dynamic obstacle blocked the robot field of view"}]`, FailBlocked},
		{"emergency stop desc", `[]`, `[{"code":52050,"desc":"emergency stop button pressed"}]`, FailEmergency},
		{"source-empty desc", `[]`, `[{"code":0,"desc":"no unclaimed PART bin at SMN_001"}]`, FailSourceEmpty},
		{"manifest mismatch desc", `[]`, `[{"code":0,"desc":"manifest mismatch: expected 6 got 4"}]`, FailManifest},
		{"loading timeout desc", `[]`, `[{"code":52301,"desc":"Loading timeout (30s)"}]`, FailTimeout},
		{"priority: emergency beats blocked", `[]`,
			`[{"code":52200,"desc":"robot is blocked"},{"code":0,"desc":"emergency stop pressed"}]`, FailEmergency},
		{"FAILED block, no error signal → vendor", `[{"state":"FAILED","block_id":"x","location":"SMN_002"}]`, `[]`, FailVendor},
		{"unknown vendor code only → vendor", `[]`, `[{"code":12345,"desc":"weird thing"}]`, FailVendor},
		{"empty arrays → other", `[]`, `[]`, FailOther},
		{"blank strings → other", ``, ``, FailOther},
		{"null → other", `null`, `null`, FailOther},
		// Springfield stores JSON `null` literal (not []) in errors_json on most
		// rows (S4 schema-reality note). The classifier must not choke on it:
		// parseJSONArray treats "null" as no-content and falls through cleanly.
		{"json-null errors literal + FAILED block → vendor (no crash)",
			`[{"state":"FAILED","block_id":"x","location":"SMN_001"}]`, `null`, FailVendor},
		{"json-null errors literal alone → other", `[]`, `null`, FailOther},
		{"non-FAILED block with block_id is NOT Robot blocked", `[{"state":"OK","block_id":"block-99","location":"x"}]`, `[]`, FailVendor},
	}
	for _, c := range cases {
		// No robot-alarm snapshot on these rows (the common case until the
		// Q-026 write side lands); classifier falls through to blocks/errors.
		if got := PrimaryFailureReason("", c.blocks, c.errors); got != c.want {
			t.Errorf("%s: PrimaryFailureReason(%q, %q) = %q, want %q", c.name, c.blocks, c.errors, got, c.want)
		}
	}
}

// TestPrimaryFailureReasonRobotAlarms pins the Q-026 priority-1 source: when a
// mission carries a robot_alarms_json snapshot (5xxxx codes projected from
// robot_alarm_log), the rich hardware fault wins over the fleet's coarse 60011
// in errors_json. Severity order fatal>error>warning>notice picks the alarm;
// an unmappable code falls through to the blocks/errors classifier.
func TestPrimaryFailureReasonRobotAlarms(t *testing.T) {
	cases := []struct {
		name, robotAlarms, blocks, errors, want string
	}{
		{"robot hardware alarm beats fleet 60011",
			`[{"code":52138,"severity":"error","desc":"fork encoder fault"}]`,
			`[{"state":"FAILED","block_id":"x","location":"SMN_001"}]`,
			`[{"code":60011,"desc":"task failed. cannot replan!","times":1}]`,
			FailHardware},
		{"most severe alarm wins (fatal battery beats warning blocked)",
			`[{"code":52200,"severity":"warning","desc":"out of path"},{"code":52503,"severity":"fatal","desc":"battery over-temp"}]`,
			`[]`, `[]`, FailBattery},
		{"motor-fault range 5213x", `[{"code":52131,"severity":"error","desc":"GVDD undervoltage"}]`, `[]`, `[]`, FailMotor},
		{"specific 52138 overrides motor range → hardware", `[{"code":52138,"severity":"error","desc":"fork encoder"}]`, `[]`, `[]`, FailHardware},
		{"e-stop range 5200x", `[{"code":52050,"severity":"fatal","desc":"emergency stop"}]`, `[]`, `[]`, FailEmergency},
		{"battery range 524xx", `[{"code":52450,"severity":"error","desc":"battery disconnect"}]`, `[]`, `[]`, FailBattery},
		{"path-plan range 5270x", `[{"code":52702,"severity":"error","desc":"path plan failed"}]`, `[]`, `[]`, FailPathPlan},
		{"comms code 52116", `[{"code":52116,"severity":"error","desc":"network down"}]`, `[]`, `[]`, FailComms},
		{"unmappable alarm code falls through to errors classifier",
			`[{"code":99999,"severity":"error","desc":"weird vendor thing"}]`,
			`[]`, `[{"code":52200,"desc":"robot is blocked"}]`, FailBlocked},
		{"empty alarm array falls through to errors", `[]`, `[]`, `[{"code":60011,"desc":"cannot replan!"}]`, FailVendor},
		{"json-null alarm literal falls through (no crash)", `null`, `[]`, `[{"code":60011,"desc":"cannot replan!"}]`, FailVendor},
	}
	for _, c := range cases {
		if got := PrimaryFailureReason(c.robotAlarms, c.blocks, c.errors); got != c.want {
			t.Errorf("%s: PrimaryFailureReason(%q, %q, %q) = %q, want %q", c.name, c.robotAlarms, c.blocks, c.errors, got, c.want)
		}
	}
}
