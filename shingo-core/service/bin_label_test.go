package service

import (
	"reflect"
	"testing"
)

// TestBatchLabels covers bin-label generation. batchLabels is pure, so
// this test carries no docker build tag (unlike the DB-backed CreateBatch
// behaviors in bin_service_test.go) and runs in the no-Postgres fast path.
func TestBatchLabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		label string
		count int
		want  []string
	}{
		{"verbatim single", "BIN-CART-7", 1, []string{"BIN-CART-7"}},
		{"verbatim single trailing dash", "BIN-", 1, []string{"BIN-"}},
		{"width preserving increment", "CART-08", 3, []string{"CART-08", "CART-09", "CART-10"}},
		{"single digit increments", "BIN9", 3, []string{"BIN9", "BIN10", "BIN11"}},
		{"overflow widening", "CART-98", 4, []string{"CART-98", "CART-99", "CART-100", "CART-101"}},
		{"no trailing digits fallback", "BATCH-", 3, []string{"BATCH-0001", "BATCH-0002", "BATCH-0003"}},
		{"empty label fallback", "", 2, []string{"0001", "0002"}},
		{"only digits increments whole", "100", 3, []string{"100", "101", "102"}},
		{"interior digits ignored", "A1B2", 2, []string{"A1B2", "A1B3"}},
		{"count zero is verbatim single", "KEEP-5", 0, []string{"KEEP-5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := batchLabels(tc.label, tc.count)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("batchLabels(%q, %d) = %v, want %v", tc.label, tc.count, got, tc.want)
			}
		})
	}
}
