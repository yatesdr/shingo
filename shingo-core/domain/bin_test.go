package domain

import (
	"strings"
	"testing"
)

// strPtr returns a pointer to s. Local helper to keep the table-driven
// tests below from sprouting one-off temporaries.
func strPtr(s string) *string { return &s }

func TestBin_ParseManifest_Nil(t *testing.T) {
	b := &Bin{Manifest: nil}
	m, err := b.ParseManifest()
	if err != nil {
		t.Fatalf("ParseManifest nil manifest: unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("ParseManifest returned nil Manifest, want non-nil")
	}
	if len(m.Items) != 0 {
		t.Errorf("len(Items) = %d, want 0", len(m.Items))
	}
}

func TestBin_ParseManifest_Empty(t *testing.T) {
	b := &Bin{Manifest: strPtr("")}
	m, err := b.ParseManifest()
	if err != nil {
		t.Fatalf("ParseManifest empty manifest: unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("ParseManifest returned nil Manifest, want non-nil")
	}
	if len(m.Items) != 0 {
		t.Errorf("len(Items) = %d, want 0", len(m.Items))
	}
}

func TestBin_ParseManifest_Single(t *testing.T) {
	raw := `{"items":[{"catid":"A","qty":5}]}`
	b := &Bin{Manifest: &raw}

	m, err := b.ParseManifest()
	if err != nil {
		t.Fatalf("ParseManifest: unexpected error: %v", err)
	}
	if len(m.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(m.Items))
	}
	got := m.Items[0]
	if got.CatID != "A" {
		t.Errorf("CatID = %q, want %q", got.CatID, "A")
	}
	if got.Quantity != 5 {
		t.Errorf("Quantity = %d, want 5", got.Quantity)
	}
	if got.LotCode != "" {
		t.Errorf("LotCode = %q, want empty", got.LotCode)
	}
	if got.Notes != "" {
		t.Errorf("Notes = %q, want empty", got.Notes)
	}
}

func TestBin_ParseManifest_Multi(t *testing.T) {
	raw := `{
		"items": [
			{"catid":"A","qty":1},
			{"catid":"B","qty":2,"lot_code":"LOT-42"},
			{"catid":"C","qty":3,"lot_code":"LOT-7","notes":"keep dry"}
		]
	}`
	b := &Bin{Manifest: &raw}

	m, err := b.ParseManifest()
	if err != nil {
		t.Fatalf("ParseManifest: unexpected error: %v", err)
	}
	if len(m.Items) != 3 {
		t.Fatalf("len(Items) = %d, want 3", len(m.Items))
	}

	wantCatIDs := []string{"A", "B", "C"}
	wantQty := []int64{1, 2, 3}
	for i, it := range m.Items {
		if it.CatID != wantCatIDs[i] {
			t.Errorf("Items[%d].CatID = %q, want %q", i, it.CatID, wantCatIDs[i])
		}
		if it.Quantity != wantQty[i] {
			t.Errorf("Items[%d].Quantity = %d, want %d", i, it.Quantity, wantQty[i])
		}
	}

	if m.Items[1].LotCode != "LOT-42" {
		t.Errorf("Items[1].LotCode = %q, want %q", m.Items[1].LotCode, "LOT-42")
	}
	if m.Items[2].LotCode != "LOT-7" {
		t.Errorf("Items[2].LotCode = %q, want %q", m.Items[2].LotCode, "LOT-7")
	}
	if m.Items[2].Notes != "keep dry" {
		t.Errorf("Items[2].Notes = %q, want %q", m.Items[2].Notes, "keep dry")
	}
}

func TestBin_ParseManifest_Invalid(t *testing.T) {
	raw := "not json"
	b := &Bin{Manifest: &raw}

	m, err := b.ParseManifest()
	if err == nil {
		t.Fatalf("ParseManifest(%q) returned nil error; want error", raw)
	}
	if m != nil {
		t.Errorf("ParseManifest returned manifest %+v on error; want nil", m)
	}
	// The implementation wraps the error with a "parse manifest:" prefix.
	if !strings.Contains(err.Error(), "parse manifest") {
		t.Errorf("error %q missing %q prefix", err.Error(), "parse manifest")
	}
}
