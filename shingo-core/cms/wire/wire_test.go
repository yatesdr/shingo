package wire

import (
	"encoding/json"
	"reflect"
	"testing"

	"shingo/protocol/testutil"

	"shingocore/store/cms"
)

func testConfig() Config {
	return Config{
		ReasonCode:    "TEST-AMR",
		IncreaseType:  "I",
		DecreaseType:  "D",
		UnitOfMeasure: "EA",
		UserID:        "SHINGO",
	}
}

// txn builds a row the way the ledger does, for a bin of ONE part — the case
// where the payload code and the line's part number are the same string,
// because a bin of one part is a bin of that part. The two fields are set to
// different values in the kit test below, which is where they diverge and where
// binding the wrong one has arithmetic consequences.
func txn(id int64, partNumber, storeroom, binLabel, robot string, delta int64) *cms.Transaction {
	return &cms.Transaction{
		ID: id, PayloadCode: partNumber, CatID: partNumber,
		Storeroom: storeroom, BinLabel: binLabel,
		RobotID: robot, Delta: delta, SourceType: "movement",
	}
}

func TestBuild_MapsEveryFieldOfOneRow(t *testing.T) {
	t.Parallel()
	got := Build([]*cms.Transaction{
		txn(7, "7332B4-6RR0A.06", "SM01", "SHG:0042", "AMR-003", 16),
	}, testConfig())

	want := []MiddlewareTx{{
		TicketNumber:    1,
		EntryNumber:     1,
		PartNumber:      "7332B4-6RR0A.06",
		StockLocation:   "SM01",
		Bin:             "SHG:0042",
		Quantity:        16,
		TransactionType: "I",
		Resource:        "AMR-003",
		ReasonCode:      "TEST-AMR",
		UnitOfMeasure:   "EA",
		UserID:          "SHINGO",
		Department:      "",
		Operation:       "",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Build() = %+v\nwant %+v", got, want)
	}
}

// TestBuild_DirectionIsTypeAndQuantityIsUnsigned: the vendor's model puts
// direction in TransactionType, so a decrease is a POSITIVE quantity of a
// decrease. Passing the signed delta through would double the sign and take a
// hundred parts off a storeroom that only lost a hundred once.
func TestBuild_DirectionIsTypeAndQuantityIsUnsigned(t *testing.T) {
	t.Parallel()
	got := Build([]*cms.Transaction{
		txn(1, "A", "SM01", "B1", "AMR-1", -16),
		txn(2, "A", "MAN", "B1", "AMR-1", 16),
	}, testConfig())

	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	if got[0].TransactionType != "D" || got[0].Quantity != 16 {
		t.Errorf("departure = %s %d, want D 16 (unsigned)", got[0].TransactionType, got[0].Quantity)
	}
	if got[1].TransactionType != "I" || got[1].Quantity != 16 {
		t.Errorf("arrival = %s %d, want I 16", got[1].TransactionType, got[1].Quantity)
	}
}

// TestBuild_EntryNumberFollowsRowIDNotSlicePosition. EntryNumber is assigned
// over a sort by id, so the same rows serialise identically however they arrive.
// That is what makes body_sha a usable dedup key and what stops a retry sending
// a permutation of the body the middleware may already hold.
func TestBuild_EntryNumberFollowsRowIDNotSlicePosition(t *testing.T) {
	t.Parallel()
	rows := []*cms.Transaction{
		txn(30, "C", "SM01", "B", "R", 3),
		txn(10, "A", "SM01", "B", "R", 1),
		txn(20, "B", "SM01", "B", "R", 2),
	}
	got := Build(rows, testConfig())

	wantParts := []string{"A", "B", "C"}
	for i, w := range wantParts {
		if got[i].PartNumber != w {
			t.Errorf("row %d part = %q, want %q — output must be ordered by row id", i, got[i].PartNumber, w)
		}
		if got[i].EntryNumber != i+1 {
			t.Errorf("row %d EntryNumber = %d, want %d", i, got[i].EntryNumber, i+1)
		}
	}

	// The caller's slice must come back untouched: Build sorts a copy. A
	// caller that goes on to attach these rows to a posting by index would
	// otherwise attach the wrong ones.
	if rows[0].ID != 30 || rows[1].ID != 10 || rows[2].ID != 20 {
		t.Errorf("Build reordered the caller's slice: %d %d %d", rows[0].ID, rows[1].ID, rows[2].ID)
	}
}

// TestBuild_IsDeterministicUnderPermutation states the property directly rather
// than at one convenient ordering: any permutation of the same rows produces
// byte-identical JSON. Serialising differently on a retry defeats body_sha.
func TestBuild_IsDeterministicUnderPermutation(t *testing.T) {
	t.Parallel()
	base := []*cms.Transaction{
		txn(11, "A", "SM01", "B1", "R1", -5),
		txn(22, "B", "MAN", "B2", "R2", 7),
		txn(33, "C", "DOCK", "B3", "", 9),
	}
	permutations := [][]*cms.Transaction{
		{base[0], base[1], base[2]},
		{base[2], base[1], base[0]},
		{base[1], base[2], base[0]},
		{base[1], base[0], base[2]},
	}

	var first []byte
	for i, perm := range permutations {
		body, err := json.Marshal(Build(perm, testConfig()))
		if err != nil {
			t.Fatalf("marshal permutation %d: %v", i, err)
		}
		if i == 0 {
			first = body
			continue
		}
		if string(body) != string(first) {
			t.Errorf("permutation %d serialised differently:\n got %s\nwant %s", i, body, first)
		}
	}

	// And twice over the same input, which catches map iteration and anything
	// reading a clock.
	again, err := json.Marshal(Build(permutations[0], testConfig()))
	testutil.MustNoErr(t, err, "marshal the second call")
	if string(again) != string(first) {
		t.Errorf("the same input serialised differently on a second call:\n got %s\nwant %s", again, first)
	}
}

// TestBuild_DropsZeroDeltaRows: a transfer of nothing is noise in a ledger, and
// it has no direction — sign(0) would have to pick one and be wrong half the
// time by construction.
func TestBuild_DropsZeroDeltaRows(t *testing.T) {
	t.Parallel()
	got := Build([]*cms.Transaction{
		txn(1, "A", "SM01", "B", "R", 0),
		txn(2, "B", "SM01", "B", "R", 4),
		nil,
	}, testConfig())

	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1 (the zero-delta row and the nil are dropped): %+v", len(got), got)
	}
	if got[0].PartNumber != "B" {
		t.Errorf("surviving row = %q, want B", got[0].PartNumber)
	}
	if got[0].EntryNumber != 1 {
		t.Errorf("EntryNumber = %d, want 1 — numbering counts what is SENT, not what was offered", got[0].EntryNumber)
	}
}

func TestBuild_EmptyInputIsAnEmptyArrayNotNull(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(Build(nil, testConfig()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// A nil slice marshals to `null`, which is not a JSON array and which a
	// strict endpoint is entitled to refuse. The poster should never send an
	// empty batch, but the shape has to be right if it ever does.
	if string(body) != "[]" {
		t.Errorf("empty build marshalled to %s, want []", body)
	}
}

// TestBuild_ConfigSuppliesTheVocabulary: the codes are config, not constants.
// SCO has not confirmed them, and the whole point of taking them as a parameter
// is that changing one is a config edit rather than a release.
func TestBuild_ConfigSuppliesTheVocabulary(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ReasonCode: " 03", IncreaseType: "RECEIPT", DecreaseType: "ISSUE",
		UnitOfMeasure: "PC", UserID: "OPERATOR",
	}
	got := Build([]*cms.Transaction{txn(1, "A", "SM01", "B", "R", -2)}, cfg)
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	// The leading space in ReasonCode is deliberate — the vendor's sample has
	// one, and whether it is fixed-width padding or an artefact is an open
	// question for IT. Either way this package must not trim it.
	if got[0].ReasonCode != " 03" {
		t.Errorf("ReasonCode = %q, want %q untrimmed", got[0].ReasonCode, " 03")
	}
	if got[0].TransactionType != "ISSUE" || got[0].UnitOfMeasure != "PC" || got[0].UserID != "OPERATOR" {
		t.Errorf("row = %+v, want the supplied vocabulary throughout", got[0])
	}
}

// TestBuild_OperatorDragHasNoResource: blank is the accurate answer for a bin a
// person dragged, not a gap to fill.
func TestBuild_OperatorDragHasNoResource(t *testing.T) {
	t.Parallel()
	got := Build([]*cms.Transaction{txn(1, "A", "SM01", "B", "", 3)}, testConfig())
	if len(got) != 1 || got[0].Resource != "" {
		t.Errorf("Resource = %q, want empty for a move with no robot", got[0].Resource)
	}
}

// TestBuild_JSONKeysAreTheVendorsNotOurs pins the wire names. They are the only
// part of this file a person can check against the vendor document, and a
// well-meant rename to shingo's vocabulary would be invisible until a real POST
// came back rejected.
func TestBuild_JSONKeysAreTheVendorsNotOurs(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(Build([]*cms.Transaction{
		txn(1, "P", "SM01", "SHG:1", "AMR-1", -1),
	}, testConfig())[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{
		"TicketNumber", "EntryNumber", "PartNumber", "StockLocation", "Bin",
		"Quantity", "TransactionType", "Resource", "ReasonCode", "UnitOfMeasure",
		"UserId", "Department", "Operation",
	}
	for _, k := range want {
		if _, ok := decoded[k]; !ok {
			t.Errorf("wire body is missing key %q: %s", k, body)
		}
	}
	if len(decoded) != len(want) {
		t.Errorf("wire body has %d keys, want %d — an extra key is a field the vendor did not ask for: %s",
			len(decoded), len(want), body)
	}
}

// TestBuild_MultiLinePayloadIsNotPostedPerLine is the pin that separates the
// two candidate bindings, and it is the kit case because that is the only place
// they differ.
//
// A transaction is ONE MANIFEST LINE's movement, so a two-line payload produces
// two of them per boundary. Bind PartNumber to the payload code and both rows
// name the payload: a payload-15 bin of capacity 1000 posts 1000 twice and CMS
// is told 2,000 moved. Bind it to the line, and each row names the part it is
// actually about.
//
// The numbers here are Springfield's payload 15 — capacity 1000, two lines —
// which is the specimen the census found and the reason this guard exists. The
// other half of the guard is upstream: material.BuildMovementTransactions will
// not build these rows at all until both lines resolve to parts, so a value
// reaching this function has been through the identity correction.
func TestBuild_MultiLinePayloadIsNotPostedPerLine(t *testing.T) {
	t.Parallel()

	const payload = "74343-6SA0A.06"
	got := Build([]*cms.Transaction{
		{ID: 1, PayloadCode: payload, CatID: "51015-LH",
			Storeroom: "SM01", BinLabel: "SHG:0015", Delta: -1000, SourceType: "movement"},
		{ID: 2, PayloadCode: payload, CatID: "51015-RH",
			Storeroom: "SM01", BinLabel: "SHG:0015", Delta: -1000, SourceType: "movement"},
	}, testConfig())

	if len(got) != 2 {
		t.Fatalf("built %d rows, want 2 (one per manifest line)", len(got))
	}
	if got[0].PartNumber == got[1].PartNumber {
		t.Fatalf("both rows name %q. A kit's two lines are two different parts; naming the "+
			"payload on each books the same 1000 units twice — 2,000 where 1,000 moved.",
			got[0].PartNumber)
	}
	for i, want := range []string{"51015-LH", "51015-RH"} {
		if got[i].PartNumber != want {
			t.Errorf("row %d PartNumber = %q, want %q — the line's own part, not the kit's name",
				i, got[i].PartNumber, want)
		}
	}
	for i, r := range got {
		if r.PartNumber == payload {
			t.Errorf("row %d names the PAYLOAD CODE %q, which for a kit is the name of the "+
				"container and matches no line", i, payload)
		}
	}
}
