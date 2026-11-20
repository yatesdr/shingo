package schemadump

import (
	"context"
	"strconv"

	"shingocore/config"
)

// DumpWithRowsForTest is pg_dump of dbName, schema and rows (--inserts),
// normalized: what a second boot must leave unchanged.
func (i *Instance) DumpWithRowsForTest(ctx context.Context, dbName string) (string, error) {
	body, err := i.pgDump(ctx, dbName, "--inserts --no-owner --no-privileges", "PGTZ=UTC")
	if err != nil {
		return "", err
	}
	return Normalize(body), nil
}

// DSNForTest reaches name on the instance, with none of Core's session
// settings: the raw SQL a test seeds and reads back with.
func (i *Instance) DSNForTest(name string) string {
	return "host=" + i.host + " port=" + itoaPort(i.port) + " dbname=" + name +
		" user=" + pgUser + " password=" + pgPass + " sslmode=disable"
}

func itoaPort(p int) string { return strconv.Itoa(p) }

// DBConfigForTest is the store config that reaches name on the instance.
func (i *Instance) DBConfigForTest(name string) *config.DatabaseConfig { return i.dbConfig(name) }
