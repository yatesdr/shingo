package store

import (
	"reflect"
	"strconv"
	"testing"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// flow_census_test.go — the composer's cell shape against the REAL plant rows,
// on the fixtures the routing backfill already loads verbatim
// (shared/scenefixtures, plants A and B).
//
// IT USED TO ASK TWO QUESTIONS. The first was a census: how many live claims
// collapsed LOCKED and on which columns, pinned so a change to the lock rule
// was a visible edit. There is no lock rule now (2026-09-09), and the question
// it was really asking — does a composer save leave the columns it does not
// show alone — is asked directly, column by column, in
// flow_carry_through_test.go.
//
//     THE LAW, ON THE STORE. domain.MaterializeClaim models what UpsertClaim
//     writes so a draft can be planned in memory. Here the model meets the
//     real INSERT and UPDATE on every live plant row: Expand(Collapse(c), &c)
//     written through the store reads back as c, and a cell written fresh
//     reads back as that cell.

// liveFixtureClaims is every claim the backfill counts as live: on a style
// row that exists, is not soft-deleted, and whose process row exists.
func liveFixtureClaims(t *testing.T, db *DB) []processes.NodeClaim {
	t.Helper()
	procs, err := db.ListProcesses()
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}
	var out []processes.NodeClaim
	for _, p := range procs {
		claims, err := processes.ListLiveClaimsByProcess(db.DB, p.ID)
		if err != nil {
			t.Fatalf("list live claims for %s: %v", p.Name, err)
		}
		out = append(out, claims...)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// storedColumnsEqual compares two claims on the columns the store persists,
// with the ones a write always moves normalised out.
func storedColumnsEqual(t *testing.T, label string, got, want processes.NodeClaim) {
	t.Helper()
	// Capacity is not a stored column and the model does not predict it — it
	// is resolved from the payload catalog on read. It is checked on its own
	// terms in claim_capacity_test.go.
	got.UOPCapacity, want.UOPCapacity = 0, 0
	got.CreatedAt = want.CreatedAt
	got.UpdatedAt, want.UpdatedAt = nil, nil
	got.RetiredAt, want.RetiredAt = nil, nil
	got.BelowReorderSince, want.BelowReorderSince = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n stored %+v\n want   %+v", label, got, want)
	}
}

// TestFlowRoundTrip_StoreAgreesWithTheModel is the round-trip law on the
// store, over every live claim of plants A and B.
//
// UPDATE: Expand(Collapse(c), &c) written through UpsertClaim lands on c's
// own row and reads back as c on every column the INSERT writes, with only
// the attribution new — and as exactly what MaterializeClaim predicted.
//
// INSERT: the same cell, written into a scratch style, reads back
// as that cell, and as what the model predicted (sequence and id aside,
// which the store assigns).
func TestFlowRoundTrip_StoreAgreesWithTheModel(t *testing.T) {
	t.Parallel()
	for _, plant := range []string{"a", "b"} {
		t.Run(plant, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			f := readPlantFixture(t, plant)
			loadPlantFixture(t, db, f)
			live := liveFixtureClaims(t, db)
			if len(live) == 0 {
				t.Fatal("no live claims loaded")
			}
			scratch := map[int64]int64{} // process id → scratch style id
			for _, c := range live {
				label := plant + " claim " + itoa64(c.ID) + " " + c.CoreNodeName

				// UPDATE path.
				in := domain.Expand(domain.Collapse(c), &c, domain.ClaimSourceHMI, "Press 400")
				model := domain.MaterializeClaim(in, &c)
				id, err := processes.UpsertClaim(db.DB, in)
				if err != nil {
					t.Errorf("%s: the store refused the claim's own cell written back: %v", label, err)
					continue
				}
				if id != c.ID {
					t.Errorf("%s: upsert landed on row %d, not the claim's own row", label, id)
					continue
				}
				stored, err := db.GetStyleNodeClaim(id)
				if err != nil {
					t.Fatalf("%s: read back: %v", label, err)
				}
				storedColumnsEqual(t, label+" (update: store vs model)", *stored, model)
				want := c
				want.Source, want.CalledBy = domain.ClaimSourceHMI, "Press 400"
				storedColumnsEqual(t, label+" (update: the law)", *stored, want)
				if stored.UpdatedAt == nil {
					t.Errorf("%s: updated_at not stamped by the update", label)
				}

				// INSERT path: the cell, as the composer would write it for a
				// new style.
				style, err := db.GetStyle(c.StyleID)
				if err != nil {
					t.Fatalf("%s: get style: %v", label, err)
				}
				sid, ok := scratch[style.ProcessID]
				if !ok {
					sid, err = db.CreateStyle("scratch-"+plant+"-"+itoa64(style.ProcessID), "", style.ProcessID)
					if err != nil {
						t.Fatalf("create scratch style: %v", err)
					}
					scratch[style.ProcessID] = sid
				}
				cell := domain.Collapse(c)
				fresh := domain.Expand(cell, nil, domain.ClaimSourceHMI, "Press 400")
				fresh.StyleID = sid
				freshModel := domain.MaterializeClaim(fresh, nil)
				newID, err := processes.UpsertClaim(db.DB, fresh)
				if err != nil {
					t.Errorf("%s: the store refused the cell as a new claim: %v", label, err)
					continue
				}
				inserted, err := db.GetStyleNodeClaim(newID)
				if err != nil {
					t.Fatalf("%s: read back new claim: %v", label, err)
				}
				if back := domain.Collapse(*inserted); !reflect.DeepEqual(back, cell) {
					t.Errorf("%s (insert: the law): the cell did not survive the store:\n read %+v\n cell %+v", label, back, cell)
				}
				freshModel.ID, freshModel.Sequence = inserted.ID, inserted.Sequence
				storedColumnsEqual(t, label+" (insert: store vs model)", *inserted, freshModel)
			}
		})
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
