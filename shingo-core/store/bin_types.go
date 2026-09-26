package store

// Stage 2D delegate file: bin_types CRUD lives in store/bins/.

import "shingocore/store/bins"

func (db *DB) CreateBinType(bt *bins.BinType) error       { return bins.CreateType(db.DB, bt) }
func (db *DB) UpdateBinType(bt *bins.BinType) error       { return bins.UpdateType(db.DB, bt) }
func (db *DB) DeleteBinType(id int64) error               { return bins.DeleteType(db.DB, id) }
func (db *DB) GetBinType(id int64) (*bins.BinType, error) { return bins.GetType(db.DB, id) }
func (db *DB) GetBinTypeByCode(code string) (*bins.BinType, error) {
	return bins.GetTypeByCode(db.DB, code)
}
func (db *DB) ListBinTypes() ([]*bins.BinType, error) { return bins.ListTypes(db.DB) }

// BareBinTypeCodes returns the codes, among ids, of the types flagged bare.
func (db *DB) BareBinTypeCodes(ids []int64) ([]string, error) { return bins.BareTypeCodes(db.DB, ids) }

// EnsureBareMarker returns a cart type's bare marker, creating it the first
// time (bins.EnsureBareMarkerTx).
func (db *DB) EnsureBareMarker(typeID int64) (int64, error) {
	return bins.EnsureBareMarker(db.DB, typeID)
}

// BinTypeBareOf returns the carrier a bare marker stands for, nil for a real
// type. The dispatch fences read it (bins.TypeAdmits).
func (db *DB) BinTypeBareOf(typeID int64) (*int64, error) { return bins.TypeBareOf(db.DB, typeID) }

// ListBareCartsInGroup returns the bare carts a stage-2 pull may move out of a
// wait group, oldest first (bins.ListBareInGroup).
func (db *DB) ListBareCartsInGroup(groupNodeID int64) ([]*bins.Bin, error) {
	return bins.ListBareInGroup(db.DB, groupNodeID)
}

// BinRealTypeCode is the bin's cart type as an operator knows it: the carrier
// when the bin is bare (bins.RealTypeCode).
func (db *DB) BinRealTypeCode(binID int64) (string, error) { return bins.RealTypeCode(db.DB, binID) }
