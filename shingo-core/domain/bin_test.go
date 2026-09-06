package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// strPtr returns a pointer to s. Local helper to keep the table-driven
// tests below from sprouting one-off temporaries.
func strPtr(s string) *string { return &s }

func TestBin_ParseManifest_Nil(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if got.PartNumber != "A" {
		t.Errorf("PartNumber = %q, want %q", got.PartNumber, "A")
	}
	if got.LotCode != "" {
		t.Errorf("LotCode = %q, want empty", got.LotCode)
	}
	if got.Notes != "" {
		t.Errorf("Notes = %q, want empty", got.Notes)
	}
}

func TestBin_ParseManifest_Multi(t *testing.T) {
	t.Parallel()
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

	wantParts := []string{"A", "B", "C"}
	for i, it := range m.Items {
		if it.PartNumber != wantParts[i] {
			t.Errorf("Items[%d].PartNumber = %q, want %q", i, it.PartNumber, wantParts[i])
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
	t.Parallel()
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

// TestBin_ParseManifest_IgnoresHistoricalQty pins the read side of deleting
// ManifestEntry.Quantity. Production bins written before the deletion still
// carry a "qty" key, and a decode that CHOKED on it would strand every one of
// them — encoding/json ignores unknown keys, and this says so out loud so the
// next person does not add a field back to "handle" it.
func TestBin_ParseManifest_IgnoresHistoricalQty(t *testing.T) {
	t.Parallel()
	raw := `{"items":[{"catid":"A","qty":5,"lot_code":"LOT-9"}]}`
	b := &Bin{Manifest: &raw}

	m, err := b.ParseManifest()
	if err != nil {
		t.Fatalf("a historical manifest with a qty key must still parse: %v", err)
	}
	if len(m.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(m.Items))
	}
	if m.Items[0].PartNumber != "A" || m.Items[0].LotCode != "LOT-9" {
		t.Errorf("Items[0] = %+v, want PartNumber A and LotCode LOT-9", m.Items[0])
	}
}

// TestManifestEntry_MarshalsNoQuantity is the write side. New manifests must
// not carry a count at all: a stored count is the staleness this deletion
// removes, and it would round-trip back into every reader that looks for one.
func TestManifestEntry_MarshalsNoQuantity(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(Manifest{Items: []ManifestEntry{{PartNumber: "A", LotCode: "L"}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), `"qty"`) {
		t.Errorf("manifest JSON carries a qty key: %s", body)
	}
}

// TestManifestEntry_ReadsEitherKey pins the compatibility window. Every bin
// standing on a plant floor when this ships carries `catid`; nothing rewrites a
// jsonb column under a running plant, so those bins have to keep reading
// correctly for as long as they take to drain. New writes emit `part_number`,
// and when a line somehow carries both, the new key wins — a stale `catid`
// alongside a fresh `part_number` is the one shape where they can disagree, and
// the fresher writer is the one that knew about both.
func TestManifestEntry_ReadsEitherKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"legacy catid", `{"items":[{"catid":"OLD-1"}]}`, "OLD-1"},
		{"current part_number", `{"items":[{"part_number":"NEW-1"}]}`, "NEW-1"},
		{"both, part_number wins", `{"items":[{"part_number":"NEW-1","catid":"OLD-1"}]}`, "NEW-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw
			b := &Bin{Manifest: &raw}
			m, err := b.ParseManifest()
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if len(m.Items) != 1 || m.Items[0].PartNumber != tc.want {
				t.Errorf("parsed %+v, want one line naming %q", m.Items, tc.want)
			}
		})
	}
}

// TestManifestEntry_MarshalsPartNumberOnly: the write side emits one key. A
// writer that emitted both would keep the old spelling alive forever and give
// the two copies a way to disagree.
func TestManifestEntry_MarshalsPartNumberOnly(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(Manifest{Items: []ManifestEntry{{PartNumber: "A"}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), `"catid"`) {
		t.Errorf("manifest JSON still writes the old key: %s", body)
	}
	if !strings.Contains(string(body), `"part_number":"A"`) {
		t.Errorf("manifest JSON does not name the part: %s", body)
	}
}
