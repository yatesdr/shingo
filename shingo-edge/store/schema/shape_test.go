package schema

import (
	"slices"
	"testing"
)

const shipped = `-- a note; with a semicolon
CREATE TABLE t (
    id INTEGER PRIMARY KEY, -- the key, commented
    name TEXT NOT NULL DEFAULT ';',
    parent INTEGER REFERENCES p(id) ON DELETE SET NULL,
    UNIQUE(name, parent)
);
CREATE TABLE p (id INTEGER PRIMARY KEY);
CREATE UNIQUE INDEX t_name ON t(name) WHERE name <> '';
CREATE TRIGGER t_after AFTER UPDATE ON t BEGIN SELECT CASE WHEN 1 THEN 2 END; DELETE FROM p; END;
INSERT INTO p VALUES (1);`

func TestShape_Compare(t *testing.T) {
	t.Parallel()
	const trigger = `CREATE TRIGGER t_after AFTER UPDATE ON t BEGIN SELECT CASE WHEN 1 THEN 2 END; DELETE FROM p; END;`
	const index = `CREATE UNIQUE INDEX t_name ON t(name) WHERE name <> '';`
	for _, c := range []struct {
		name             string
		live             string
		missing, differs []string
	}{
		{"another spelling of the same shape", `create table "p" ( id integer primary key ) ;
			CREATE TABLE "t" (parent INTEGER REFERENCES "p" (id) ON DELETE SET NULL, id INTEGER PRIMARY KEY,
			name TEXT NOT NULL DEFAULT ';', UNIQUE (name, parent));` + index + trigger, nil, nil},
		{"a table and a column gone", `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT ';',
			UNIQUE(name, parent));` + index + trigger,
			[]string{"column t.parent", "table p"}, nil},
		{"everything else is a difference", `CREATE TABLE p (id INT PRIMARY KEY); CREATE TABLE extra (a, b);
			CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT ';',
			parent INTEGER REFERENCES gone(id) ON DELETE SET NULL, more TEXT);`, nil,
			[]string{
				"column p.id differs: the database has int primary key, this build ships integer primary key",
				"column t.more is in the database and not in this build's schema",
				"column t.parent differs: the database has integer references gone ( id ) on delete set null, this build ships integer references p ( id ) on delete set null",
				"constraint t.unique ( name , parent ) is missing (this build ships it)",
				"index t_name is missing (this build ships it)",
				"table extra is in the database and not in this build's schema",
				"trigger t_after is missing (this build ships it)",
			}},
	} {
		missing, differs := Compare(ParseShape(shipped), ParseShape(c.live))
		if !slices.Equal(missing, c.missing) || !slices.Equal(differs, c.differs) {
			t.Errorf("%s:\n missing %q, want %q\n differs %q, want %q", c.name, missing, c.missing, differs, c.differs)
		}
	}
}
