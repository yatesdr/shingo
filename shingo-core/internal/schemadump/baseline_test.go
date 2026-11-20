package schemadump

import (
	"strings"
	"testing"
)

// TestExtractDDLConstSkipsNonDDLConsts covers the regression this function was
// hardened against: it used to take the FIRST const in a historical file
// revision and fail if that const was not the schema. That works only by luck
// of which vintages happen to be pinned — any revision declaring another const
// ahead of the DDL would report "wrong const" for a file that does contain the
// schema.
func TestExtractDDLConstSkipsNonDDLConsts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "DDL is the only const",
			src:  "package store\n\nconst schemaPostgres = `CREATE TABLE bins (id INT);`\n",
			want: "CREATE TABLE bins (id INT);",
		},
		{
			name: "a non-DDL raw-string const comes first",
			src: "package store\n\nconst schemaVersion = `v42`\n\n" +
				"const schemaPostgres = `CREATE TABLE bins (id INT);`\n",
			want: "CREATE TABLE bins (id INT);",
		},
		{
			name: "a non-raw-string const comes first",
			src: "package store\n\nconst migrationCount = 317\n\n" +
				"const postgresDDL = `CREATE TABLE bins (id INT);`\n",
			want: "CREATE TABLE bins (id INT);",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := extractDDLConst(tt.src)
			if err != nil {
				t.Fatalf("extractDDLConst: %v", err)
			}
			if got != tt.want {
				t.Errorf("extractDDLConst = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractDDLConstErrorsWhenNoDDLPresent(t *testing.T) {
	t.Parallel()

	src := "package store\n\nconst schemaVersion = `v42`\n"
	if _, err := extractDDLConst(src); err == nil {
		t.Fatal("extractDDLConst returned nil error for a file with no CREATE TABLE const")
	}
}

// TestExcludePartitions_DropsEveryStatementOfAChild: a child's table, its
// attach, its index and its index's attach go; the parent and its own index
// stay; a dump with no child is returned as it came.
func TestExcludePartitions_DropsEveryStatementOfAChild(t *testing.T) {
	t.Parallel()
	dump := `CREATE TABLE public.cell_part_events (
    id bigint NOT NULL
)
PARTITION BY RANGE (recorded_at);

CREATE TABLE public.cell_part_events_2026_06 (
    id bigint NOT NULL
);

ALTER TABLE ONLY public.cell_part_events ATTACH PARTITION public.cell_part_events_2026_06 FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('2026-07-01 00:00:00+00');

CREATE INDEX idx_cell_part_events_cell_time ON ONLY public.cell_part_events USING btree (cell_id, recorded_at);

CREATE INDEX cell_part_events_2026_06_cell_id_recorded_at_idx ON public.cell_part_events_2026_06 USING btree (cell_id, recorded_at);

ALTER INDEX public.idx_cell_part_events_cell_time ATTACH PARTITION public.cell_part_events_2026_06_cell_id_recorded_at_idx;
`
	got := ExcludePartitions(dump)
	for _, gone := range []string{"cell_part_events_2026_06"} {
		if strings.Contains(got, gone) {
			t.Errorf("ExcludePartitions kept a statement naming %s:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"CREATE TABLE public.cell_part_events (", "CREATE INDEX idx_cell_part_events_cell_time ON ONLY"} {
		if !strings.Contains(got, kept) {
			t.Errorf("ExcludePartitions dropped %q:\n%s", kept, got)
		}
	}
	plain := "CREATE TABLE public.orders (\n    id bigint\n);\n"
	if ExcludePartitions(plain) != plain {
		t.Errorf("ExcludePartitions changed a dump with no partition")
	}
}
