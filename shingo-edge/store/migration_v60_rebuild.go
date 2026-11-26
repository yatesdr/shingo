package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// A table v60 rebuilds where its shape differs from what v60 leaves: the
// plants' foreign keys naming renamed-away tables and a quarantine column's
// type. SQLite has no ALTER for either.
type v60Rebuild struct {
	table   string
	indexes []string // the indexes this build knows on it
	ddl     string   // what v60 leaves, the table's name as %s
}

// v60ShapeDiffers is 1 when ?1's columns or foreign keys are not ?2's.
const v60ShapeDiffers = `SELECT EXISTS (
		SELECT name, upper(type), "notnull", dflt_value, pk FROM pragma_table_info(?1)
		EXCEPT SELECT name, upper(type), "notnull", dflt_value, pk FROM pragma_table_info(?2))
	OR EXISTS (
		SELECT name, upper(type), "notnull", dflt_value, pk FROM pragma_table_info(?2)
		EXCEPT SELECT name, upper(type), "notnull", dflt_value, pk FROM pragma_table_info(?1))
	OR EXISTS (
		SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?1)
		EXCEPT SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?2))
	OR EXISTS (
		SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?2)
		EXCEPT SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?1))`

// converge builds the table v60 leaves beside the live one and, only where the
// two differ, copies every row by column name, puts it in the live one's place
// and carries the AUTOINCREMENT sequence. A column, index or trigger this
// build does not know fails v60 whole: nothing is lost without a word.
func (r v60Rebuild) converge(tx *sql.Tx) error {
	next := r.table + "_v60"
	if _, err := tx.Exec(fmt.Sprintf(r.ddl, next)); err != nil {
		return fmt.Errorf("create %s: %w", next, err)
	}
	var differs bool
	if err := tx.QueryRow(v60ShapeDiffers, r.table, next).Scan(&differs); err != nil {
		return fmt.Errorf("compare: %w", err)
	}
	if !differs {
		_, err := tx.Exec(`DROP TABLE ` + next)
		return err
	}

	live, err := txStrings(tx, `SELECT name FROM pragma_table_info(?)`, r.table)
	if err != nil {
		return err
	}
	known, err := txStrings(tx, `SELECT name FROM pragma_table_info(?)`, next)
	if err != nil {
		return err
	}
	var unknown []string
	for _, column := range live {
		if !slices.Contains(known, column) {
			unknown = append(unknown, "column "+column)
		}
	}
	attached, err := txStrings(tx, `SELECT type || ' ' || name FROM sqlite_master
		WHERE tbl_name = ? AND type IN ('index', 'trigger') AND sql IS NOT NULL ORDER BY name`, r.table)
	if err != nil {
		return err
	}
	for _, object := range attached {
		if name, ok := strings.CutPrefix(object, "index "); ok && slices.Contains(r.indexes, name) {
			continue
		}
		unknown = append(unknown, object)
	}
	if len(unknown) > 0 {
		return fmt.Errorf("this build does not know its %s and will not rebuild over it: drop it by hand, or restore a backup; nothing was changed",
			strings.Join(unknown, ", "))
	}
	recreate, err := txStrings(tx, `SELECT sql FROM sqlite_master
		WHERE tbl_name = ? AND type = 'index' AND sql IS NOT NULL ORDER BY name`, r.table)
	if err != nil {
		return err
	}
	var seq sql.NullInt64
	err = tx.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = ?`, r.table).Scan(&seq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read its sequence: %w", err)
	}

	columns := `"` + strings.Join(live, `", "`) + `"`
	for _, stmt := range []string{
		`INSERT INTO ` + next + ` (` + columns + `) SELECT ` + columns + ` FROM ` + r.table + ` ORDER BY rowid`,
		`DROP TABLE ` + r.table,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
	}
	if err := renameKeepingReferences(tx, next, r.table); err != nil {
		return err
	}
	for _, stmt := range recreate {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("recreate an index: %w", err)
		}
	}
	// The copy left the sequence at max(id), which hands a deleted row's id out
	// again to the rows that still name it.
	if seq.Valid {
		if _, err := tx.Exec(`DELETE FROM sqlite_sequence WHERE name = ?`, r.table); err != nil {
			return fmt.Errorf("carry its sequence: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)`, r.table, seq.Int64); err != nil {
			return fmt.Errorf("carry its sequence: %w", err)
		}
	}
	return nil
}

// renameKeepingReferences renames with legacy_alter_table held on, so no other
// table's stored REFERENCES clause follows the rename, refuses to run if the
// pragma did not take, and puts it back on every way out.
func renameKeepingReferences(tx *sql.Tx, from, to string) (err error) {
	var was int
	if err := tx.QueryRow(`PRAGMA legacy_alter_table`).Scan(&was); err != nil {
		return fmt.Errorf("read legacy_alter_table: %w", err)
	}
	if _, err := tx.Exec(`PRAGMA legacy_alter_table = ON`); err != nil {
		return fmt.Errorf("set legacy_alter_table: %w", err)
	}
	defer func() {
		if _, restoreErr := tx.Exec(fmt.Sprintf(`PRAGMA legacy_alter_table = %d`, was)); err == nil && restoreErr != nil {
			err = fmt.Errorf("restore legacy_alter_table: %w", restoreErr)
		}
	}()
	var on int
	if err := tx.QueryRow(`PRAGMA legacy_alter_table`).Scan(&on); err != nil {
		return fmt.Errorf("read legacy_alter_table back: %w", err)
	}
	if on != 1 {
		return fmt.Errorf("legacy_alter_table did not take: a rename would rewrite other tables' REFERENCES clauses")
	}
	if _, err := tx.Exec(`ALTER TABLE ` + from + ` RENAME TO ` + to); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
