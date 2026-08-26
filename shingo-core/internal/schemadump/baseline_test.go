package schemadump

import "testing"

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
