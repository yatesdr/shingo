package store

import (
	"errors"
	"testing"
	"time"

	"shingo/protocol"
)

// ReconcileSupplyRefusals is one transaction: the merge sees the table as read
// inside it, and the rewrite and the enqueued messages commit together or not
// at all.
func TestReconcileSupplyRefusals_OneTransaction(t *testing.T) {
	t.Parallel()
	ack := time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)
	cases := []struct {
		name      string
		keep      []SupplyRefusal
		mergeErr  error
		wantErr   bool
		wantRows  int
		wantMsgs  int
		wantFirst string // refused_by of LN-2/P1 after, "" when absent
	}{
		{
			name: "commit: table rewritten, message enqueued",
			keep: []SupplyRefusal{{LoaderNode: "LN-2", PayloadCode: "P1", RefusedAt: ack, RefusedBy: "edge.b",
				AckAt: &ack, AckChoice: "wait", AckProcessID: "PR1"}},
			wantRows: 1, wantMsgs: 1, wantFirst: "edge.b",
		},
		{
			name: "merge fails: nothing written, nothing enqueued", mergeErr: errors.New("boom"),
			wantErr: true, wantRows: 1, wantMsgs: 0,
		},
		{
			name: "an insert fails part-way: the delete and the message roll back with it",
			keep: []SupplyRefusal{
				{LoaderNode: "LN-2", PayloadCode: "P1", RefusedAt: ack, RefusedBy: "edge.b"},
				{LoaderNode: "LN-2", PayloadCode: "P1", RefusedAt: ack, RefusedBy: "edge.b"},
			},
			wantErr: true, wantRows: 1, wantMsgs: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			if err := db.OpenSupplyRefusal("LN-1", "P1", "edge.test"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			var seen []SupplyRefusal
			err := db.ReconcileSupplyRefusals(func(open []SupplyRefusal) ([]SupplyRefusal, [][]byte, error) {
				seen = open
				return tc.keep, [][]byte{[]byte(`{}`)}, tc.mergeErr
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if len(seen) != 1 || seen[0].LoaderNode != "LN-1" {
				t.Errorf("merge saw %+v, want the one seeded row", seen)
			}
			open, err := db.ListOpenSupplyRefusals()
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(open) != tc.wantRows {
				t.Errorf("rows after = %d, want %d", len(open), tc.wantRows)
			}
			msgs, err := db.ListUnsentOutboxByType([]string{protocol.SubjectSupplyRefusal})
			if err != nil {
				t.Fatalf("outbox: %v", err)
			}
			if len(msgs) != tc.wantMsgs {
				t.Errorf("messages after = %d, want %d", len(msgs), tc.wantMsgs)
			}
			if tc.wantFirst == "" {
				return
			}
			got, err := db.GetSupplyRefusal("LN-2", "P1")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.RefusedBy != tc.wantFirst || !got.Answered() || got.AckProcessID != "PR1" || !got.RefusedAt.Equal(ack) {
				t.Errorf("row = %+v, want refused_by %s answered by PR1 at %v", got, tc.wantFirst, ack)
			}
		})
	}
}
