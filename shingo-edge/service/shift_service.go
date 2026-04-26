package service

import (
	"shingoedge/store"
	"shingoedge/store/shifts"
)

// ShiftService owns production-shift CRUD. Shifts (1st, 2nd, 3rd) are
// a first-class manufacturing concept: operators identify by shift,
// production targets are set per-shift, hourly counts bucket by
// shift, payroll references shift assignments. Despite today's small
// CRUD surface (~3 methods), shifts is its own domain and earns its
// own service.
//
// Phase 6.2′ extracted this from named methods on *engine.Engine.
type ShiftService struct {
	db *store.DB
}

// NewShiftService constructs a ShiftService wrapping the shared
// *store.DB.
func NewShiftService(db *store.DB) *ShiftService {
	return &ShiftService{db: db}
}

// List returns all shifts ordered by shift_number.
func (s *ShiftService) List() ([]shifts.Shift, error) {
	return s.db.ListShifts()
}

// Upsert inserts or updates a shift definition by shift_number.
func (s *ShiftService) Upsert(shiftNumber int, name, startTime, endTime string) error {
	return s.db.UpsertShift(shiftNumber, name, startTime, endTime)
}

// Delete removes a shift row by shift_number.
func (s *ShiftService) Delete(shiftNumber int) error {
	return s.db.DeleteShift(shiftNumber)
}
