package store

import (
	"time"
)

type AdminUser struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

func (db *DB) CreateAdminUser(username, passwordHash string) error {
	_, err := db.Exec(`INSERT INTO admin_users (username, password_hash) VALUES ($1, $2)`, username, passwordHash)
	return err
}

func (db *DB) GetAdminUser(username string) (*AdminUser, error) {
	var u AdminUser
	err := db.QueryRow(`SELECT id, username, password_hash, created_at FROM admin_users WHERE username=$1`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (db *DB) AdminUserExists() (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&count)
	return count > 0, err
}
