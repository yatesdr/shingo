//go:build docker

package binresolver

import (
	"encoding/json"
	"os"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
)

// dig_reader_golden_docker_test.go — the two buried-bin readers, photographed
// apart from the sourcing matrix.
//
// THEY ANSWER A DIFFERENT QUESTION. "Which buried bin do I dig" is not "may
// this bin be sourced", and the clearest evidence is the reservation arm: every
// sourcing reader refuses a bin somebody has reserved, and these two take it on
// purpose. A reservation on a buried bin is the REASON it needs digging, not a
// reason to leave it there. They are reservation-blind by design and do not
// share the sourcing predicate.
//
// They are photographed anyway, because the 2026-09-14 rulings change them: a
// disabled node is dead to automation, digs included, so both gained the
// enabled check. Nothing changes unwatched.
//
// Fixtures, topology and verdict convention are the sourcing harness's — see
// eligibility_golden_docker_test.go. Only lane B is read here.
//
// Run with -update to regenerate:
//
//	go test -tags docker ./dispatch/binresolver/ -run TestGolden_DigReaders -update

// digRow is one fixture's verdict from each dig reader.
type digRow struct {
	Fixture string `json:"fixture"`

	// store.FindBuriedBin — shallowest buried, the cheapest reshuffle.
	LaneBuried string `json:"lane_buried"`

	// store.FindOldestBuriedBin — oldest buried, strict FIFO.
	LaneBuriedOldest string `json:"lane_buried_oldest"`
}

func TestGolden_DigReaders(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)

	var orderID int64
	if err := db.DB.QueryRow(
		`INSERT INTO orders (edge_uuid) VALUES ('dig-golden') RETURNING id`,
	).Scan(&orderID); err != nil {
		t.Fatalf("seed order for reservations: %v", err)
	}
	binType, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		t.Fatalf("get DEFAULT bin type: %v", err)
	}
	otherType := &bins.BinType{Code: "DIG-OTHER", Description: "bin-type-rule arm"}
	if err := db.CreateBinType(otherType); err != nil {
		t.Fatalf("create DIG-OTHER bin type: %v", err)
	}

	cases := eligSQLCases()
	rows := make([]digRow, 0, len(cases))
	for i, c := range cases {
		f := eligSQLBuild(t, db, binType.ID, otherType.ID, orderID, i, c)

		buried, _, err := db.FindBuriedBin(f.BuriedLane, f.BuriedAsk)
		oldest, _, oerr := db.FindOldestBuriedBin(f.BuriedLane, f.BuriedAsk)

		rows = append(rows, digRow{
			Fixture:          c.Name,
			LaneBuried:       eligVerdict(err == nil && buried != nil),
			LaneBuriedOldest: eligVerdict(oerr == nil && oldest != nil),
		})
	}

	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	got = append(got, '\n')

	const goldenPath = "testdata/golden/dig_readers.json"
	if *updateFlag {
		if err := os.MkdirAll("testdata/golden", 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s (%d fixtures)", goldenPath, len(rows))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skipf("golden file %s not found (run with -update to create): %v", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("dig-reader matrix changed.\n--- want (golden) ---\n%s\n--- got ---\n%s\n"+
			"If this change is intended, re-run with -update and justify each differing row.",
			want, got)
	}
}
