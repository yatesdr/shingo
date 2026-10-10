//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/schema"
)

// TestMigrate_LinesideReportCarriesNoCarrierColumns (row L item 2): Core
// compares the 60-s lineside report on the decoded message, and nothing reads
// back the carrier and count columns it used to store. After the chain they are
// gone; the seat (station, node, part) and reported_at stay. That v127, which
// added three of them, does not re-add them on the next boot is
// TestMigrate_NoMigrationOscillatesOnEveryBoot's to catch.
func TestMigrate_LinesideReportCarriesNoCarrierColumns(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	for _, c := range []string{"bin_count", "bin_uop", "bucket_qty", "bin_id", "bin_epoch", "flushed_seq"} {
		if !schema.ColumnAbsent(db.DB, "edge_lineside_reports", c) {
			t.Errorf("edge_lineside_reports.%s exists; it has no reader and is not written", c)
		}
	}
	for _, c := range []string{"station", "core_node_name", "payload_code", "reported_at"} {
		if !schema.ColumnExists(db.DB, "edge_lineside_reports", c) {
			t.Errorf("edge_lineside_reports.%s is gone; the seat and reported_at stay", c)
		}
	}
}
