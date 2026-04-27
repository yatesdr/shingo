package service

import (
	"shingocore/store"
	"shingocore/store/admin"
)

// AdminService exposes admin-user queries used by the login/session
// flow. Handlers call AdminService instead of reaching through engine
// passthroughs to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Stage 2A.2 (2026-04) added UserExists + CreateUser so
// www/auth.go no longer needs to reach a *store.DB to seed the
// default admin on a fresh install.
//
// Password verification and session management still live in
// www/auth.go and will grow as the auth layer moves behind a
// first-class service in a future phase.
type AdminService struct {
	db *store.DB
}

func NewAdminService(db *store.DB) *AdminService {
	return &AdminService{db: db}
}

// GetUser looks up an admin user by username.
func (s *AdminService) GetUser(username string) (*admin.User, error) {
	return s.db.GetAdminUser(username)
}

// UserExists reports whether at least one admin user has been
// created. Used by the bootstrap path that seeds a default admin on a
// fresh install.
func (s *AdminService) UserExists() (bool, error) {
	return s.db.AdminUserExists()
}

// CreateUser inserts a new admin user with the given password hash.
// Bootstrap-only — the handler computes the hash via auth.HashPassword
// before calling. Returns nil on success or the underlying database
// error on failure.
func (s *AdminService) CreateUser(username, passwordHash string) error {
	return s.db.CreateAdminUser(username, passwordHash)
}
