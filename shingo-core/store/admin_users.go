package store

// Phase 5 delegate file: admin-user CRUD lives in store/admin/. This file
// preserves the *store.DB method surface so external callers don't need
// to change.

import (
	"shingocore/store/admin"
)

// AdminUser preserves the store.AdminUser public API.
type AdminUser = admin.User

func (db *DB) CreateAdminUser(username, passwordHash string) error {
	return admin.Create(db.DB, username, passwordHash)
}

func (db *DB) GetAdminUser(username string) (*AdminUser, error) {
	return admin.Get(db.DB, username)
}

func (db *DB) AdminUserExists() (bool, error) {
	return admin.AnyExists(db.DB)
}
