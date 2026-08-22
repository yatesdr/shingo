package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// TermCode is a PERSISTED, COMPARED contract: the strings land in
// order_history.code and are compared by readers. Renaming a constant is safe;
// changing the string it holds silently reclassifies history.

func TestAllTermCodes_NoDuplicatesNoEmpty(t *testing.T) {
	seen := map[TermCode]bool{}
	for _, c := range AllTermCodes() {
		if c == "" {
			t.Fatal(`"" must not be a member — it means "uncoded", not a category`)
		}
		if seen[c] {
			t.Fatalf("duplicate term code %q", c)
		}
		seen[c] = true
	}
}

// Exhaustiveness: every declared code must classify, so adding one without
// deciding its outcome bucket is a test failure rather than a silent
// default-to-failure.
func TestAllTermCodes_EveryCodeClassifies(t *testing.T) {
	for _, c := range AllTermCodes() {
		if !ValidTermCode(c) {
			t.Errorf("%q is declared but ValidTermCode rejects it", c)
		}
	}
	if !ValidTermCode("") {
		t.Error(`"" must be valid — every pre-migration row carries it`)
	}
	if ValidTermCode("definitely_not_a_code") {
		t.Error("unknown codes must not validate")
	}
}

// The two values that actually occur at Springfield. If either string changes,
// every historical row silently stops matching.
func TestTermCodes_LiveValuesArePinned(t *testing.T) {
	if TermNoSourceBin != "no_source_bin" {
		t.Errorf("TermNoSourceBin = %q, want no_source_bin", TermNoSourceBin)
	}
	if TermGraceTimeout != "grace_timeout" {
		t.Errorf("TermGraceTimeout = %q, want grace_timeout", TermGraceTimeout)
	}
}

// TermRef renders the way the design writes it, because that string goes in a
// log line next to the code and someone reads it at 3am.
func TestTermRef_String(t *testing.T) {
	r := TermRef{Node: "PLN_01.R1", Payload: "74577-6SA0A.06"}
	if got, want := r.String(), "node=PLN_01.R1, payload=74577-6SA0A.06"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if (TermRef{}).String() != "" {
		t.Error("an empty reference renders as nothing, not as punctuation")
	}
	if got := (TermRef{Peer: 4812}).String(); got != "peer=4812" {
		t.Errorf("peer render = %q", got)
	}
	// The vendor pair renders too, or a faulted row logs as if the fleet said
	// nothing — which is the state this field was added to end.
	got := TermRef{Node: "ALN_003", VendorCode: 60011, VendorDesc: "cannot replan"}.String()
	if want := "node=ALN_003, vendor=60011, vendor_desc=cannot replan"; got != want {
		t.Errorf("vendor render = %q, want %q", got, want)
	}
}

func TestTermRef_Empty(t *testing.T) {
	if !(TermRef{}).Empty() {
		t.Error("zero value is empty")
	}
	// A ref carrying ONLY a vendor code is the shape a faulted row takes when
	// the fleet named a reason but the order had no node or payload on it. If
	// Empty() missed the vendor fields, refJSON would store that row's ref as
	// NULL and the one thing it knew would be the one thing it dropped.
	for _, r := range []TermRef{
		{Node: "n"}, {Payload: "p"}, {Peer: 1}, {Detail: "d"},
		{VendorCode: 60011}, {VendorDesc: "cannot replan"},
	} {
		if r.Empty() {
			t.Errorf("%+v should not be empty", r)
		}
	}
}

// Empty fields are omitted so the JSONB column holds only what is known — a
// ref of {"node":"","payload":"","peer":0} would make ref->>'payload' return
// "" instead of NULL and quietly join the empty string into every GROUP BY.
func TestTermRef_JSONOmitsEmptyFields(t *testing.T) {
	b, err := json.Marshal(TermRef{Payload: "74577-6SA0A.06"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if got != `{"payload":"74577-6SA0A.06"}` {
		t.Fatalf("marshal = %s, want only the set field", got)
	}
	for _, absent := range []string{"node", "peer", "detail", "vendor_code", "vendor_desc"} {
		if strings.Contains(got, absent) {
			t.Errorf("empty %q should be omitted, got %s", absent, got)
		}
	}
	// vendor_code is the one that matters here: ~94% of faulted orders have no
	// fleet reason, so a non-omitted zero would make ref->>'vendor_code' return
	// "0" on nine rows in ten and turn "the fleet said nothing" into a code.
	b, err = json.Marshal(TermRef{VendorCode: 60011, VendorDesc: "cannot replan"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(b); got != `{"vendor_code":60011,"vendor_desc":"cannot replan"}` {
		t.Fatalf("vendor marshal = %s", got)
	}
}

// Round-trip: the column is written by Core and read back by Core, so a
// symmetric encode/decode is the whole contract.
func TestTermRef_RoundTrip(t *testing.T) {
	want := TermRef{Node: "ALN_003", Payload: "74577-6SA0A.06", Peer: 991,
		VendorCode: 60011, VendorDesc: "cannot replan", Detail: "evac sibling terminal"}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got TermRef
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip lost data: %+v -> %+v", want, got)
	}
}
