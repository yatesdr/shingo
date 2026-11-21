package store

import (
	"database/sql"
	"fmt"

	"shingocore/store/schema"
)

// v189 — cell_config dropped. A cell is always the catalog cell an Edge
// reported (edge_cells, service.deriveCellConfig).
//
// Its one writer was POST /api/cells, which nothing sent once /admin/cells
// was retired; the override branches that read it go with this commit, and
// the cells every surface shows read the same with no row as before
// (TestCellsAPI_CatalogCellWithNoConfigRow). Down recreates the table as v185
// left it, empty (the pre-deploy read counts its rows:
// SELECT count(*) FROM cell_config).
//
// v185 renames two of its columns and applies a comment to one: v185Verify
// takes a renamed column's table being gone as the rename holding, and v185Up
// applies no comment to an object whose table is gone (v185CommentsFor), so a
// re-run of v185 on a database v189 has passed through neither loops nor
// fails.
//
// One transaction (the runner's). Idempotent (DROP TABLE IF EXISTS).

// v189CellConfigRestore is cell_config as v185 left it: v185's names, its key,
// its index; the comment is v185's own (comments.sql).
var v189CellConfigRestore = []string{
	`CREATE TABLE public.cell_config (
		cell_label text NOT NULL,
		station_uid text NOT NULL,
		primary_process_id bigint NOT NULL,
		sub_process_ids jsonb DEFAULT '[]'::jsonb NOT NULL,
		display_name text DEFAULT ''::text NOT NULL,
		updated_at timestamp with time zone DEFAULT now() NOT NULL)`,
	`ALTER TABLE ONLY public.cell_config ADD CONSTRAINT cell_config_pkey PRIMARY KEY (cell_label)`,
	`CREATE INDEX cell_config_station_uid_idx ON public.cell_config USING btree (station_uid)`,
}

// v189Up is migration 189's apply function.
func v189Up(tx *sql.Tx) error {
	if schema.TableExists(tx, "cell_config") {
		if err := migrationExec(tx, "v189 lock", `LOCK TABLE cell_config IN ACCESS EXCLUSIVE MODE`); err != nil {
			return err
		}
	}
	return migrationExec(tx, "v189", `DROP TABLE IF EXISTS cell_config`)
}

// v189Verify is v189's post-condition: the table is gone. Catalog reads only.
func v189Verify(q schema.Querier) bool {
	return schema.TableAbsent(q, "cell_config")
}

// v189Down recreates cell_config, empty, in v185's shape.
func v189Down(tx *sql.Tx) error {
	comment, err := v185Comment("COMMENT ON COLUMN cell_config.cell_label IS ")
	if err != nil {
		return fmt.Errorf("v189 down: %w", err)
	}
	for _, q := range append(v189CellConfigRestore, comment) {
		if err := migrationExec(tx, "v189 down", q); err != nil {
			return err
		}
	}
	return nil
}
