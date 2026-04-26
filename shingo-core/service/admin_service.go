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
// (PR 3a.6). The service is intentionally small today — password
// verification and session management still live in www/auth.go — and
// will grow as the auth layer moves behind a first-class service in a
// future phase.
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
