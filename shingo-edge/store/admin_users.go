package store

// Phase 5b delegate file: admin-user CRUD now lives in store/admin/.
// This file preserves the *store.DB method surface so external callers
// do not need to change.

import "shingoedge/store/admin"

// AdminUser is a user who can access the setup page.
type AdminUser = admin.User

// GetAdminUser returns one admin user by username.
func (db *DB) GetAdminUser(username string) (*AdminUser, error) {
	return admin.Get(db.DB, username)
}

// CreateAdminUser inserts an admin user and returns the new row id.
func (db *DB) CreateAdminUser(username, passwordHash string) (int64, error) {
	return admin.Create(db.DB, username, passwordHash)
}

// UpdateAdminPassword sets a new password hash for the given username.
func (db *DB) UpdateAdminPassword(username, passwordHash string) error {
	return admin.UpdatePassword(db.DB, username, passwordHash)
}

// AdminUserExists reports whether at least one admin_users row exists.
func (db *DB) AdminUserExists() (bool, error) {
	return admin.AnyExists(db.DB)
}
