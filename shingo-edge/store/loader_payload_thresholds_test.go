package store

import (
	"database/sql"
	"shingo/protocol/testutil"
	"testing"
)

// TestLoaderPayloadThresholds_UpsertAndKeying — the v6 re-key by
// core_node_name. Two different loaders for the same payload land as
// distinct rows; updates target the right row; delete is by string
// key.
func TestLoaderPayloadThresholds_UpsertAndKeying(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	if err := db.UpsertLoaderPayloadThreshold(LoaderPayloadThreshold{
		CoreNodeName:          "SPRING-LDR-01",
		PayloadCode:           "WIDGET-A",
		ReplenishUOPThreshold: 100,
		Source:                "manual",
		SafetyFactor:          1.5,
		UpdatedBy:             "alice",
	}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if err := db.UpsertLoaderPayloadThreshold(LoaderPayloadThreshold{
		CoreNodeName:          "SPRING-LDR-02",
		PayloadCode:           "WIDGET-A",
		ReplenishUOPThreshold: 50,
		Source:                "manual",
		SafetyFactor:          1.5,
		UpdatedBy:             "alice",
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	all, err := db.ListLoaderPayloadThresholds()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}

	// Update LDR-01 to 200; LDR-02 should be untouched.
	if err := db.UpsertLoaderPayloadThreshold(LoaderPayloadThreshold{
		CoreNodeName:          "SPRING-LDR-01",
		PayloadCode:           "WIDGET-A",
		ReplenishUOPThreshold: 200,
		Source:                "calculated",
		SafetyFactor:          1.5,
		UpdatedBy:             "alice",
	}); err != nil {
		t.Fatalf("upsert 1 again: %v", err)
	}
	got, err := db.GetLoaderPayloadThreshold("SPRING-LDR-01", "WIDGET-A")
	if err != nil || got == nil {
		t.Fatalf("get 1: %v / %v", got, err)
	}
	if got.ReplenishUOPThreshold != 200 || got.Source != "calculated" {
		t.Errorf("LDR-01 = %d/%s, want 200/calculated", got.ReplenishUOPThreshold, got.Source)
	}
	got2, err := db.GetLoaderPayloadThreshold("SPRING-LDR-02", "WIDGET-A")
	if err != nil || got2 == nil {
		t.Fatalf("get 2: %v / %v", got2, err)
	}
	if got2.ReplenishUOPThreshold != 50 {
		t.Errorf("LDR-02 changed unexpectedly: %d", got2.ReplenishUOPThreshold)
	}

	// ThresholdsByPayloadForLoader narrows to one loader.
	rows, err := db.ThresholdsByPayloadForLoader("SPRING-LDR-01")
	if err != nil {
		t.Fatalf("byLoader: %v", err)
	}
	if rows["WIDGET-A"] != 200 {
		t.Errorf("byLoader[WIDGET-A] = %d, want 200", rows["WIDGET-A"])
	}

	// Delete by composite key.
	testutil.MustNoErr(t, db.DeleteLoaderPayloadThreshold("SPRING-LDR-01", "WIDGET-A"), "delete")
	if got, _ := db.GetLoaderPayloadThreshold("SPRING-LDR-01", "WIDGET-A"); got != nil {
		t.Error("LDR-01 still present after delete")
	}
}

// TestLoaderPayloadThresholds_CalculatedMetadata — applying a
// calculated value must land threshold_calculated, threshold_
// calculated_at, and threshold_confidence on the threshold row so the
// UI badge and history readback don't have to join the audit table.
func TestLoaderPayloadThresholds_CalculatedMetadata(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	if err := db.UpsertLoaderPayloadThreshold(LoaderPayloadThreshold{
		CoreNodeName:          "SPRING-LDR-01",
		PayloadCode:           "WIDGET-A",
		ReplenishUOPThreshold: 118,
		Source:                "calculated",
		SafetyFactor:          1.5,
		ThresholdCalculated:   118,
		ThresholdCalculatedAt: sql.NullString{String: "2026-05-15 12:00:00", Valid: true},
		ThresholdConfidence:   "HIGH",
		UpdatedBy:             "alice",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := db.GetLoaderPayloadThreshold("SPRING-LDR-01", "WIDGET-A")
	if err != nil || got == nil {
		t.Fatalf("get: %v / %v", got, err)
	}
	if got.ThresholdCalculated != 118 {
		t.Errorf("threshold_calculated = %d, want 118", got.ThresholdCalculated)
	}
	if !got.ThresholdCalculatedAt.Valid || got.ThresholdCalculatedAt.String != "2026-05-15 12:00:00" {
		t.Errorf("threshold_calculated_at = %+v, want 2026-05-15 12:00:00", got.ThresholdCalculatedAt)
	}
	if got.ThresholdConfidence != "HIGH" {
		t.Errorf("threshold_confidence = %q, want HIGH", got.ThresholdConfidence)
	}
}

// TestLoaderPayloadThresholds_OverriddenInputs — round-trip the
// comma-separated override token list through upsert + scan + update.
// Order is preserved (UI consumes the list in storage order).
func TestLoaderPayloadThresholds_OverriddenInputs(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	if err := db.UpsertLoaderPayloadThreshold(LoaderPayloadThreshold{
		CoreNodeName:          "SPRING-LDR-01",
		PayloadCode:           "WIDGET-A",
		ReplenishUOPThreshold: 118,
		Source:                "calculated",
		SafetyFactor:          1.5,
		OverriddenInputs:      "l2_load_seconds,safety_factor",
		UpdatedBy:             "alice",
	}); err != nil {
		t.Fatalf("upsert with overrides: %v", err)
	}
	got, err := db.GetLoaderPayloadThreshold("SPRING-LDR-01", "WIDGET-A")
	if err != nil || got == nil {
		t.Fatalf("get: %v / %v", got, err)
	}
	if got.OverriddenInputs != "l2_load_seconds,safety_factor" {
		t.Errorf("overridden_inputs = %q, want %q", got.OverriddenInputs, "l2_load_seconds,safety_factor")
	}

	// Re-upsert with an empty override list — column should clear.
	if err := db.UpsertLoaderPayloadThreshold(LoaderPayloadThreshold{
		CoreNodeName:          "SPRING-LDR-01",
		PayloadCode:           "WIDGET-A",
		ReplenishUOPThreshold: 100,
		Source:                "manual",
		SafetyFactor:          1.5,
		OverriddenInputs:      "",
		UpdatedBy:             "bob",
	}); err != nil {
		t.Fatalf("upsert without overrides: %v", err)
	}
	got, err = db.GetLoaderPayloadThreshold("SPRING-LDR-01", "WIDGET-A")
	if err != nil || got == nil {
		t.Fatalf("get after clear: %v / %v", got, err)
	}
	if got.OverriddenInputs != "" {
		t.Errorf("overridden_inputs after clear = %q, want empty", got.OverriddenInputs)
	}
}
