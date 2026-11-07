//go:build docker

package store_test

import (
	"database/sql"
	"errors"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/material"
)

// TestMaterialStore_AMissingRowIsAnError pins what the real store answers, on
// the reads material.Store declares, for a row that is not there: an error and
// no value, as material's fake answers (material/fake_store_test.go). The
// material package's walks are tested against that fake only, so a store read
// that answered a missing row with a zero value and no error would leave every
// material test green while the walk read "no such node" as "a node with no
// boundary".
func TestMaterialStore_AMissingRowIsAnError(t *testing.T) {
	t.Parallel()
	var s material.Store = testdb.Open(t)
	const missing = 99999
	for _, c := range []struct {
		name string
		read func() (found bool, err error)
		// noRows: material branches on sql.ErrNoRows itself (partsPerCycle:
		// "no such payload" against "the database is down"), so the error must
		// be that one, not any error.
		noRows bool
	}{
		// TestFindCMSBoundary_StoreErrorPropagates is the inverse of the test
		// this replaces. A store failure must reach the caller: "the lookup
		// failed" and "there is no boundary here" are different answers, and
		// collapsing them emitted zero CMS rows for real physical moves.
		{name: "FindCMSBoundary_StoreErrorPropagates", read: func() (bool, error) {
			n, err := s.GetNode(missing)
			return n != nil, err
		}},
		{name: "GetBin", read: func() (bool, error) {
			b, err := s.GetBin(missing)
			return b != nil, err
		}},
		{name: "GetPayloadByCode", noRows: true, read: func() (bool, error) {
			p, err := s.GetPayloadByCode("NO-SUCH-PAYLOAD")
			return p != nil, err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			found, err := c.read()
			if err == nil || found {
				t.Fatalf("a missing row: found=%v err=%v, want no value and an error", found, err)
			}
			if c.noRows && !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("a missing row: err = %v, want sql.ErrNoRows", err)
			}
		})
	}
}
