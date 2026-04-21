// Package admin holds admin-user persistence for shingo-edge.
//
// Phase 5b of the architecture plan moved the admin_users CRUD out of
// the flat store/ package and into this sub-package. The outer store/
// keeps a type alias (`store.AdminUser = admin.User`) and one-line
// delegate methods on *store.DB so external callers see no API change.
package admin

import (
	"database/sql"
	"time"

	"shingoedge/store/internal/helpers"
)

// User is one admin_users row.
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// Get returns one admin user by username.
func Get(db *sql.DB, username string) (*User, error) {
	u := &User{}
	var createdAt string
	err := db.QueryRow(`SELECT id, username, password_hash, created_at FROM admin_users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAt)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = helpers.ScanTime(createdAt)
	return u, nil
}

// Create inserts an admin user and returns the new row id.
func Create(db *sql.DB, username, passwordHash string) (int64, error) {
	res, err := db.Exec(`INSERT INTO admin_users (username, password_hash) VALUES (?, ?)`, username, passwordHash)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdatePassword sets a new password hash for the given username.
func UpdatePassword(db *sql.DB, username, passwordHash string) error {
	_, err := db.Exec(`UPDATE admin_users SET password_hash = ? WHERE username = ?`, passwordHash, username)
	return err
}

// AnyExists reports whether at least one admin_users row exists.
func AnyExists(db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&count)
	return count > 0, err
}
