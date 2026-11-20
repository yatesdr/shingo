package schemadump

import "context"

// DumpWithRowsForTest is pg_dump of dbName, schema and rows (--inserts),
// normalized: what a second boot must leave unchanged.
func (i *Instance) DumpWithRowsForTest(ctx context.Context, dbName string) (string, error) {
	body, err := i.pgDump(ctx, dbName, "--inserts --no-owner --no-privileges", "PGTZ=UTC")
	if err != nil {
		return "", err
	}
	return Normalize(body), nil
}
