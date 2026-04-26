package store

// Phase 5 delegate file: admin-user CRUD lives in store/admin/. This file
// preserves the *store.DB method surface so external callers don't need
// to change.

import "shingocore/store/admin"

func (db *DB) CreateAdminUser(username, passwordHash string) error {
	return admin.Create(db.DB, username, passwordHash)
}

func (db *DB) GetAdminUser(username string) (*admin.User, error) {
	return admin.Get(db.DB, username)
}

func (db *DB) AdminUserExists() (bool, error) {
	return admin.AnyExists(db.DB)
}
