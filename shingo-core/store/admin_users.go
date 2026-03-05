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
	_, err := db.Exec(db.Q(`INSERT INTO admin_users (username, password_hash) VALUES (?, ?)`), username, passwordHash)
	return err
}

func (db *DB) GetAdminUser(username string) (*AdminUser, error) {
	var u AdminUser
	var createdAt any
	err := db.QueryRow(db.Q(`SELECT id, username, password_hash, created_at FROM admin_users WHERE username=?`), username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAt)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = parseTime(createdAt)
	return &u, nil
}

func (db *DB) AdminUserExists() (bool, error) {
	var count int
	err := db.QueryRow(db.Q(`SELECT COUNT(*) FROM admin_users`)).Scan(&count)
	return count > 0, err
}
