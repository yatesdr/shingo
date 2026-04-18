package store

// Stage 2D delegate file: bin_types CRUD lives in store/bins/.

import "shingocore/store/bins"

// BinType aliases the bin-types sub-package's row type.
type BinType = bins.BinType

func (db *DB) CreateBinType(bt *BinType) error              { return bins.CreateType(db.DB, bt) }
func (db *DB) UpdateBinType(bt *BinType) error              { return bins.UpdateType(db.DB, bt) }
func (db *DB) DeleteBinType(id int64) error                 { return bins.DeleteType(db.DB, id) }
func (db *DB) GetBinType(id int64) (*BinType, error)        { return bins.GetType(db.DB, id) }
func (db *DB) GetBinTypeByCode(code string) (*BinType, error) { return bins.GetTypeByCode(db.DB, code) }
func (db *DB) ListBinTypes() ([]*BinType, error)            { return bins.ListTypes(db.DB) }
