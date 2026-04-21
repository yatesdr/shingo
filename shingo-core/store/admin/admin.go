// Package admin holds admin-user persistence for shingo-core.
//
// Phase 5 of the architecture plan moved admin_users CRUD out of the
// flat store/ package and into this sub-package. The outer store/ keeps
// a type alias (`store.AdminUser = admin.AdminUser`) and one-line
// delegate methods on *store.DB so external callers see no API change.
package admin

import (
	"database/sql"
	"time"
)

// User is the admin-user entity. The type is re-aliased at the outer
// store/ level as store.AdminUser so service/admin_service.go compiles
// unchanged.
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// Create inserts a new admin user.
func Create(db *sql.DB, username, passwordHash string) error {
	_, err := db.Exec(`INSERT INTO admin_users (username, password_hash) VALUES ($1, $2)`, username, passwordHash)
	return err
}

// Get fetches an admin user by username.
func Get(db *sql.DB, username string) (*User, error) {
	var u User
	err := db.QueryRow(`SELECT id, username, password_hash, created_at FROM admin_users WHERE username=$1`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// AnyExists reports whether at least one admin user exists.
func AnyExists(db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&count)
	return count > 0, err
}
