package service

import (
	"shingoedge/store"
	"shingoedge/store/admin"
)

// AdminService owns admin-user CRUD and auth-time lookups. The only
// admin operations that exist on edge today are for the local setup
// page; production operator HMI does not require login.
//
// Phase 6.2′ extracted this from named methods on *engine.Engine.
type AdminService struct {
	db *store.DB
}

// NewAdminService constructs an AdminService wrapping the shared
// *store.DB.
func NewAdminService(db *store.DB) *AdminService {
	return &AdminService{db: db}
}

// Exists reports whether any admin_users row exists. Used by the
// setup page to decide whether to bootstrap a first admin or render
// the login form.
func (s *AdminService) Exists() (bool, error) {
	return s.db.AdminUserExists()
}

// Get fetches an admin user by username. Returns the row including
// password hash for auth comparison; callers are responsible for
// constant-time comparison.
func (s *AdminService) Get(username string) (*admin.User, error) {
	return s.db.GetAdminUser(username)
}

// Create inserts a new admin user and returns the new row id.
func (s *AdminService) Create(username, passwordHash string) (int64, error) {
	return s.db.CreateAdminUser(username, passwordHash)
}

// UpdatePassword changes the password_hash for an existing admin.
func (s *AdminService) UpdatePassword(username, passwordHash string) error {
	return s.db.UpdateAdminPassword(username, passwordHash)
}
