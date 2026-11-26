package registry

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotStale is the refusal of RemoveStale: the uid names no stale station.
// Nothing was written.
var ErrNotStale = errors.New("only a stale station can be removed")

// RemoveStale deletes one stale station's row on tx and returns its id and
// display name, for the audit.
//
// DELETE ONLY, AND THE STATUS IS IN ITS WHERE. A heartbeat that made the
// station active first leaves zero rows and that is the answer (ErrNotStale);
// one that comes after finds no row, is asked to register, and introduces the
// station as any unknown one. The display name lives on this row alone, so it
// goes with it.
func RemoveStale(tx *sql.Tx, uid string) (id int64, displayName string, err error) {
	err = tx.QueryRow(`DELETE FROM edge_registry WHERE station_uid = $1 AND status = 'stale'
		RETURNING id, display_name`, uid).Scan(&id, &displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotStale
	}
	if err != nil {
		return 0, "", fmt.Errorf("registry remove %s: %w", uid, err)
	}
	return id, displayName, nil
}
