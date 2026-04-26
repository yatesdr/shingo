package store

// Stage 2D delegate file: bin_types CRUD lives in store/bins/.

import "shingocore/store/bins"

func (db *DB) CreateBinType(bt *bins.BinType) error              { return bins.CreateType(db.DB, bt) }
func (db *DB) UpdateBinType(bt *bins.BinType) error              { return bins.UpdateType(db.DB, bt) }
func (db *DB) DeleteBinType(id int64) error                 { return bins.DeleteType(db.DB, id) }
func (db *DB) GetBinType(id int64) (*bins.BinType, error)        { return bins.GetType(db.DB, id) }
func (db *DB) GetBinTypeByCode(code string) (*bins.BinType, error) { return bins.GetTypeByCode(db.DB, code) }
func (db *DB) ListBinTypes() ([]*bins.BinType, error)            { return bins.ListTypes(db.DB) }
