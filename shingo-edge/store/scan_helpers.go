package store

import (
	"database/sql"
	"time"
)

const timeLayout = "2006-01-02 15:04:05"

func scanTime(s string) time.Time {
	t, _ := time.ParseInLocation(timeLayout, s, time.UTC)
	return t
}

func scanTimePtr(ns sql.NullString) *time.Time {
	if !ns.Valid {
		return nil
	}
	t, err := time.ParseInLocation(timeLayout, ns.String, time.UTC)
	if err != nil {
		return nil
	}
	return &t
}
