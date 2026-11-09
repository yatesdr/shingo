package main

// datamodel.go — the generated blocks of docs/data-model.md: every Core and
// Edge table with its columns, read from the two committed schema snapshots,
// and the protocol vocabularies the tables store (order types, swap modes;
// statuses are wire.go's table).
//
// The snapshots are each module's own rendering of its migrated schema
// (Core: pg_dump of a migrated Postgres, `make schema-snapshot`; Edge: the
// SQLite DDL of a migrated database), so a column the generator shows is one
// a fresh database has. Core's dump carries no column comments (the schema
// declares none); the Edge's DDL keeps the `--` comments its migrations wrote,
// and those become the Comment column.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"shingo/protocol"
)

func init() {
	register(Generator{Name: coreSnapshot, Emit: func(root string, args []string) (string, error) {
		return emitSchema(root, coreSnapshot, "core", args)
	}})
	register(Generator{Name: edgeSnapshot, Emit: func(root string, args []string) (string, error) {
		return emitSchema(root, edgeSnapshot, "edge", args)
	}})
	register(Generator{Name: "protocol/types.go OrderType", Emit: emitOrderTypes})
	register(Generator{Name: "protocol/swap_mode.go ConfigurableSwapModes", Emit: emitSwapModes})
}

const (
	coreSnapshot = "shingo-core/store/schema/schema.snapshot.sql"
	edgeSnapshot = "shingo-edge/store/schema/schema.snapshot.sql"
)

// sqlColumn is one column of a table.
type sqlColumn struct {
	name, typ, def, key, comment string
	notNull                      bool
}

// sqlTable is one table: its columns and the constraints that span them.
type sqlTable struct {
	name  string
	cols  []sqlColumn
	notes []string // table-level keys, unique indexes and checks, rendered
	refs  map[string]bool
	pk    []string
}

func (t *sqlTable) col(name string) *sqlColumn {
	for i := range t.cols {
		if t.cols[i].name == name {
			return &t.cols[i]
		}
	}
	return nil
}

func (t *sqlTable) addRef(col, target string) {
	if t.refs == nil {
		t.refs = map[string]bool{}
	}
	table, _, _ := strings.Cut(target, "(")
	t.refs[table] = true
	if c := t.col(col); c != nil {
		c.key = joinNote(c.key, "→ `"+target+"`")
	} else {
		t.notes = append(t.notes, fmt.Sprintf("Foreign key (%s) → `%s`", col, target))
	}
}

func (t *sqlTable) setPK(cols []string) {
	t.pk = cols
	if len(cols) == 1 {
		if c := t.col(cols[0]); c != nil {
			c.key = joinNote("PK", c.key)
			return
		}
	}
	t.notes = append(t.notes, "Primary key ("+strings.Join(cols, ", ")+")")
}

func (t *sqlTable) addUnique(cols []string, where string) {
	if len(cols) == 1 && where == "" {
		if c := t.col(cols[0]); c != nil {
			c.key = joinNote(c.key, "unique")
			return
		}
	}
	note := "Unique (" + strings.Join(cols, ", ") + ")"
	if where != "" {
		note += " where `" + where + "`"
	}
	t.notes = append(t.notes, note)
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + ", " + b
}

var schemaCache sync.Map // root+path → []*sqlTable

func loadSchema(root, path, dialect string) ([]*sqlTable, error) {
	key := root + "\x00" + path
	if v, ok := schemaCache.Load(key); ok {
		return v.([]*sqlTable), nil
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
	var tables []*sqlTable
	if dialect == "core" {
		tables, err = parsePgDump(src)
	} else {
		tables, err = parseSQLiteDDL(src)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("%s: no CREATE TABLE found", path)
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].name < tables[j].name })
	schemaCache.Store(key, tables)
	return tables, nil
}

// ---------------------------------------------------------------------------
// The Core snapshot: pg_dump output.

var (
	pgCreateRe  = regexp.MustCompile(`(?ms)^CREATE TABLE public\.(\w+) \(\n(.*?)\n\)(?:\n(PARTITION BY [^;]*))?;`)
	pgAlterRe   = regexp.MustCompile(`(?m)^ALTER TABLE (?:ONLY )?public\.(\w+)\n\s+ADD CONSTRAINT \w+ (.*);$`)
	pgSerialRe  = regexp.MustCompile(`(?m)^ALTER TABLE ONLY public\.(\w+) ALTER COLUMN (\w+) SET DEFAULT (.*);$`)
	pgUniqIdxRe = regexp.MustCompile(`(?m)^CREATE UNIQUE INDEX \w+ ON public\.(\w+) USING \w+ \((.*?)\)(?: WHERE (.*))?;$`)
	pgPKRe      = regexp.MustCompile(`^PRIMARY KEY \((.*)\)$`)
	pgUniqueRe  = regexp.MustCompile(`^UNIQUE \((.*)\)$`)
	pgFKRe      = regexp.MustCompile(`^FOREIGN KEY \((.*?)\) REFERENCES public\.(\w+\(.*?\))(.*)$`)
	pgCheckRe   = regexp.MustCompile(`^CHECK \((.*)\)( NOT VALID)?$`)
)

func parsePgDump(src string) ([]*sqlTable, error) {
	byName := map[string]*sqlTable{}
	var out []*sqlTable
	for _, m := range pgCreateRe.FindAllStringSubmatch(src, -1) {
		t := &sqlTable{name: m[1]}
		for _, item := range splitTop(m[2]) {
			item = strings.TrimSpace(item)
			if rest, ok := strings.CutPrefix(item, "CONSTRAINT "); ok {
				_, body, _ := strings.Cut(rest, " ")
				if c := pgCheckRe.FindStringSubmatch(body); c != nil {
					t.notes = append(t.notes, "Check `"+sqlText(c[1])+"`")
					continue
				}
				return nil, fmt.Errorf("%s: unread table constraint %q", t.name, item)
			}
			t.cols = append(t.cols, parsePgColumn(item))
		}
		if m[3] != "" {
			t.notes = append(t.notes, "Partitioned: `"+strings.TrimPrefix(m[3], "PARTITION BY ")+"`")
		}
		byName[t.name] = t
		out = append(out, t)
	}
	for _, m := range pgSerialRe.FindAllStringSubmatch(src, -1) {
		if t := byName[m[1]]; t != nil {
			if c := t.col(m[2]); c != nil {
				c.def = sqlText(m[3])
			}
		}
	}
	for _, m := range pgAlterRe.FindAllStringSubmatch(src, -1) {
		t := byName[m[1]]
		if t == nil {
			return nil, fmt.Errorf("a constraint on unknown table %s", m[1])
		}
		body := m[2]
		switch {
		case pgPKRe.MatchString(body):
			t.setPK(splitList(pgPKRe.FindStringSubmatch(body)[1]))
		case pgUniqueRe.MatchString(body):
			t.addUnique(splitList(pgUniqueRe.FindStringSubmatch(body)[1]), "")
		case pgFKRe.MatchString(body):
			f := pgFKRe.FindStringSubmatch(body)
			target := f[2]
			if tail := strings.TrimSpace(f[3]); tail != "" {
				target += " " + tail
			}
			t.addRef(f[1], target)
		case pgCheckRe.MatchString(body):
			t.notes = append(t.notes, "Check `"+sqlText(pgCheckRe.FindStringSubmatch(body)[1])+"`")
		default:
			return nil, fmt.Errorf("%s: unread constraint %q", t.name, body)
		}
	}
	for _, m := range pgUniqIdxRe.FindAllStringSubmatch(src, -1) {
		if t := byName[m[1]]; t != nil {
			t.addUnique(splitList(m[2]), sqlText(strings.TrimSuffix(strings.TrimPrefix(m[3], "("), ")")))
		}
	}
	return out, nil
}

func parsePgColumn(item string) sqlColumn {
	name, rest, _ := strings.Cut(item, " ")
	c := sqlColumn{name: strings.Trim(name, `"`)}
	if r, ok := strings.CutSuffix(rest, " NOT NULL"); ok {
		c.notNull, rest = true, r
	}
	if typ, gen, ok := strings.Cut(rest, " GENERATED ALWAYS AS "); ok {
		c.typ, c.def = typ, "generated: "+sqlText(strings.TrimSuffix(gen, " STORED"))
		return c
	}
	if typ, def, ok := strings.Cut(rest, " DEFAULT "); ok {
		c.typ, c.def = typ, sqlText(def)
		return c
	}
	c.typ = rest
	return c
}

// sqlText drops the dump's noise from an expression: the schema prefix and
// the casts a reader does not need (a text literal on a text column, the
// regclass of a sequence name).
func sqlText(s string) string {
	s = strings.ReplaceAll(s, "public.", "")
	s = strings.ReplaceAll(s, "::text", "")
	s = strings.ReplaceAll(s, "::regclass", "")
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// The Edge snapshot: SQLite DDL, comments kept.

var (
	sqliteCreateRe  = regexp.MustCompile(`(?m)^CREATE TABLE (?:IF NOT EXISTS )?"?(\w+)"?\s*\(`)
	sqliteUniqIdxRe = regexp.MustCompile(`(?s)CREATE UNIQUE INDEX (?:IF NOT EXISTS )?\w+\s+ON (\w+)\s*\(([^)]*)\)\s*(?:WHERE (.*?))?;`)
	sqliteFKRe      = regexp.MustCompile(`(?is)^FOREIGN KEY\s*\((.*?)\)\s*REFERENCES\s+(\w+\s*\(.*?\))(.*)$`)
	sqlitePKRe      = regexp.MustCompile(`(?is)^PRIMARY KEY\s*\((.*)\)$`)
	sqliteUniqueRe  = regexp.MustCompile(`(?is)^UNIQUE\s*\((.*)\)$`)
	sqliteCheckRe   = regexp.MustCompile(`(?is)^CHECK\s*\((.*)\)$`)
)

// ddlItem is one comma-separated element of a CREATE TABLE body, with the
// comments that belong to it.
type ddlItem struct {
	text    string
	comment []string
	indent  int
}

func parseSQLiteDDL(src string) ([]*sqlTable, error) {
	var out []*sqlTable
	for _, loc := range sqliteCreateRe.FindAllStringSubmatchIndex(src, -1) {
		name := src[loc[2]:loc[3]]
		body, ok := parenBody(src, loc[1]-1)
		if !ok {
			return nil, fmt.Errorf("%s: unbalanced CREATE TABLE", name)
		}
		t := &sqlTable{name: name}
		for _, it := range ddlItems(body) {
			if err := t.addSQLiteItem(it); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		out = append(out, t)
	}
	byName := map[string]*sqlTable{}
	for _, t := range out {
		byName[t.name] = t
	}
	for _, m := range sqliteUniqIdxRe.FindAllStringSubmatch(src, -1) {
		if t := byName[m[1]]; t != nil {
			t.addUnique(splitList(m[2]), strings.Join(strings.Fields(m[3]), " "))
		}
	}
	return out, nil
}

// parenBody is the text between the '(' at open and its matching ')',
// skipping quotes and comments.
func parenBody(src string, open int) (string, bool) {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '\'':
			j := strings.IndexByte(src[i+1:], '\'')
			if j < 0 {
				return "", false
			}
			i += j + 1
		case '-':
			if i+1 < len(src) && src[i+1] == '-' {
				j := strings.IndexByte(src[i:], '\n')
				if j < 0 {
					return "", false
				}
				i += j
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return src[open+1 : i], true
			}
		}
	}
	return "", false
}

// ddlItems splits a CREATE TABLE body into its elements and hands each its
// comments: a trailing `--` on an element's line is that element's, a
// comment line is the next element's — unless it is indented deeper than the
// element before it, which makes it that element's continuation.
func ddlItems(body string) []ddlItem {
	var items []ddlItem
	cur := ddlItem{indent: -1}
	var pending []string
	lastClosed := -1 // index into items of the element the last comma closed
	depth := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if strings.HasPrefix(trimmed, "--") {
			text := strings.TrimSpace(strings.TrimPrefix(trimmed, "--"))
			if strings.TrimSpace(cur.text) == "" && lastClosed >= 0 && indent > items[lastClosed].indent && len(pending) == 0 {
				items[lastClosed].comment = append(items[lastClosed].comment, text)
			} else {
				pending = append(pending, text)
			}
			continue
		}
		code, comment := splitComment(line)
		endedOnComma := false
		for i := 0; i < len(code); i++ {
			ch := code[i]
			if ch == '\'' {
				j := strings.IndexByte(code[i+1:], '\'')
				if j >= 0 {
					cur.text += code[i : i+j+2]
					i += j + 1
					continue
				}
			}
			switch ch {
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 0 {
					items = append(items, cur)
					lastClosed = len(items) - 1
					cur = ddlItem{indent: -1}
					endedOnComma = strings.TrimSpace(code[i+1:]) == ""
					continue
				}
			}
			if cur.indent < 0 && ch != ' ' && ch != '\t' {
				cur.indent = indent
				cur.comment = append(cur.comment, pending...)
				pending = nil
			}
			cur.text += string(ch)
		}
		cur.text += " "
		if comment != "" {
			if endedOnComma || strings.TrimSpace(cur.text) == "" {
				items[lastClosed].comment = append(items[lastClosed].comment, comment)
			} else {
				cur.comment = append(cur.comment, comment)
			}
		}
	}
	if strings.TrimSpace(cur.text) != "" {
		items = append(items, cur)
	}
	for i := range items {
		items[i].text = strings.Join(strings.Fields(items[i].text), " ")
	}
	return items
}

// splitComment splits a DDL line at a `--` outside quotes.
func splitComment(line string) (code, comment string) {
	in := false
	for i := 0; i+1 < len(line); i++ {
		switch {
		case line[i] == '\'':
			in = !in
		case !in && line[i] == '-' && line[i+1] == '-':
			return line[:i], strings.TrimSpace(line[i+2:])
		}
	}
	return line, ""
}

// keywordParenRe finds a constraint keyword written against its parenthesis,
// CHECK(...), so the word split sees the keyword alone.
var keywordParenRe = regexp.MustCompile(`(?i)\b(CHECK|DEFAULT)\(`)

var sqliteKeywords = map[string]bool{
	"PRIMARY": true, "NOT": true, "NULL": true, "UNIQUE": true, "DEFAULT": true,
	"REFERENCES": true, "CHECK": true, "COLLATE": true, "GENERATED": true, "CONSTRAINT": true, "AS": true,
}

func (t *sqlTable) addSQLiteItem(it ddlItem) error {
	text := it.text
	if rest, ok := strings.CutPrefix(text, "CONSTRAINT "); ok {
		_, text, _ = strings.Cut(rest, " ")
	}
	if handled, err := t.addSQLiteConstraint(text); handled || err != nil {
		return err
	}
	return t.addSQLiteColumn(text, strings.Join(it.comment, " "))
}

// addSQLiteConstraint reads a table-level key or check; false when text is a
// column definition.
func (t *sqlTable) addSQLiteConstraint(text string) (bool, error) {
	upper := strings.ToUpper(text)
	var re *regexp.Regexp
	switch {
	case strings.HasPrefix(upper, "PRIMARY KEY"):
		re = sqlitePKRe
	case strings.HasPrefix(upper, "UNIQUE"):
		re = sqliteUniqueRe
	case strings.HasPrefix(upper, "FOREIGN KEY"):
		re = sqliteFKRe
	case strings.HasPrefix(upper, "CHECK"):
		re = sqliteCheckRe
	default:
		return false, nil
	}
	m := re.FindStringSubmatch(text)
	if m == nil {
		return true, fmt.Errorf("unread table constraint %q", text)
	}
	switch re {
	case sqlitePKRe:
		t.setPK(splitList(m[1]))
	case sqliteUniqueRe:
		t.addUnique(splitList(m[1]), "")
	case sqliteFKRe:
		t.addRef(m[1], strings.TrimSpace(strings.ReplaceAll(m[2], " (", "(")+m[3]))
	default:
		t.notes = append(t.notes, "Check `"+m[1]+"`")
	}
	return true, nil
}

// addSQLiteColumn reads one column definition: its name, declared type, and
// the column constraints after the type.
func (t *sqlTable) addSQLiteColumn(text, comment string) error {
	words := splitWords(keywordParenRe.ReplaceAllString(text, "$1 ("))
	c := sqlColumn{name: strings.Trim(words[0], `"`), comment: comment}
	i := 1
	var typ []string
	for ; i < len(words) && !sqliteKeywords[strings.ToUpper(words[i])]; i++ {
		typ = append(typ, words[i])
	}
	c.typ = strings.Join(typ, " ")
	var keys []string
	for i < len(words) {
		w := strings.ToUpper(words[i])
		switch {
		case w == "NOT" && i+1 < len(words) && strings.ToUpper(words[i+1]) == "NULL":
			c.notNull = true
			i += 2
		case w == "NULL":
			i++
		case w == "PRIMARY":
			var k string
			k, i = sqlitePrimaryKey(words, i+2)
			c.notNull = true
			keys = append(keys, k)
			t.pk = []string{c.name}
		case w == "UNIQUE":
			keys = append(keys, "unique")
			i++
		case w == "DEFAULT" && i+1 < len(words):
			c.def = words[i+1]
			i += 2
		case w == "REFERENCES" && i+1 < len(words):
			var target string
			target, i = sqliteReference(words, i+1)
			table, _, _ := strings.Cut(target, "(")
			if t.refs == nil {
				t.refs = map[string]bool{}
			}
			t.refs[table] = true
			keys = append(keys, "→ `"+target+"`")
		case w == "CHECK" && i+1 < len(words):
			t.notes = append(t.notes, "Check `"+strings.TrimSuffix(strings.TrimPrefix(words[i+1], "("), ")")+"`")
			i += 2
		case w == "COLLATE" && i+1 < len(words):
			keys = append(keys, "collate "+words[i+1])
			i += 2
		default:
			return fmt.Errorf("column %s: unread %q in %q", c.name, words[i], text)
		}
	}
	c.key = strings.Join(keys, ", ")
	t.cols = append(t.cols, c)
	return nil
}

// sqlitePrimaryKey reads the words after PRIMARY KEY, from i: an order and
// AUTOINCREMENT. It returns the key cell and the index after them.
func sqlitePrimaryKey(words []string, i int) (string, int) {
	k := "PK"
	for i < len(words) && (strings.EqualFold(words[i], "AUTOINCREMENT") || strings.EqualFold(words[i], "ASC") || strings.EqualFold(words[i], "DESC")) {
		if strings.EqualFold(words[i], "AUTOINCREMENT") {
			k += " autoincrement"
		}
		i++
	}
	return k, i
}

// sqliteReference reads a REFERENCES target from i (the table(column) word)
// and its ON DELETE / ON UPDATE actions, and returns it with the index after.
func sqliteReference(words []string, i int) (string, int) {
	target := words[i]
	i++
	for i < len(words) && strings.EqualFold(words[i], "ON") && i+2 < len(words) {
		target += " " + strings.Join(words[i:i+3], " ")
		i += 3
		if i < len(words) && strings.EqualFold(words[i-1], "SET") {
			target += " " + words[i]
			i++
		}
	}
	return target, i
}

// splitWords splits on spaces outside parentheses and quotes, so a
// parenthesised default or check is one word.
func splitWords(s string) []string {
	var out []string
	var cur strings.Builder
	depth, in := 0, false
	for _, r := range s {
		switch {
		case r == '\'':
			in = !in
		case !in && r == '(':
			depth++
		case !in && r == ')':
			depth--
		case !in && depth == 0 && r == ' ':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// splitTop splits on commas outside parentheses and quotes.
func splitTop(s string) []string {
	var out []string
	depth, in, start := 0, false, 0
	for i, r := range s {
		switch {
		case r == '\'':
			in = !in
		case !in && r == '(':
			depth++
		case !in && r == ')':
			depth--
		case !in && depth == 0 && r == ',':
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func splitList(s string) []string {
	var out []string
	for _, p := range splitTop(s) {
		out = append(out, strings.Trim(strings.TrimSpace(p), `"`))
	}
	return out
}

// ---------------------------------------------------------------------------
// Rendering.

// tableAnchor is the heading slug of a table's section in the full listing.
func tableAnchor(side, table string) string {
	return side + "-" + strings.ToLower(table)
}

func code(s string) string {
	if s == "" {
		return ""
	}
	return "`" + strings.ReplaceAll(s, "|", `\|`) + "`"
}

func renderColumns(b *strings.Builder, t *sqlTable) {
	withComment := false
	for _, c := range t.cols {
		if c.comment != "" {
			withComment = true
		}
	}
	if withComment {
		b.WriteString("| Column | Type | Not null | Default | Key | Comment |\n|---|---|---|---|---|---|\n")
	} else {
		b.WriteString("| Column | Type | Not null | Default | Key |\n|---|---|---|---|---|\n")
	}
	for _, c := range t.cols {
		nn := ""
		if c.notNull {
			nn = "yes"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |", c.name, code(c.typ), nn, code(c.def), cell(c.key))
		if withComment {
			fmt.Fprintf(b, " %s |", cell(c.comment))
		}
		b.WriteString("\n")
	}
	if len(t.notes) > 0 {
		b.WriteString("\n")
		for _, n := range t.notes {
			fmt.Fprintf(b, "- %s\n", strings.ReplaceAll(n, "|", `\|`))
		}
	}
}

// emitSchema renders a snapshot. With no arguments: an index of every table,
// then a section per table (heading anchor side-table). With table names:
// those tables' column tables alone, for a doc that shows one table in place.
func emitSchema(root, path, side string, args []string) (string, error) {
	tables, err := loadSchema(root, path, side)
	if err != nil {
		return "", err
	}
	byName := map[string]*sqlTable{}
	for _, t := range tables {
		byName[t.name] = t
	}
	var b strings.Builder
	if len(args) > 0 {
		for i, a := range args {
			t := byName[a]
			if t == nil {
				return "", fmt.Errorf("%s has no table %s", path, a)
			}
			if i > 0 {
				fmt.Fprintf(&b, "\n`%s`:\n\n", a)
			}
			renderColumns(&b, t)
		}
		return b.String(), nil
	}
	label := map[string]string{"core": "Core", "edge": "Edge"}[side]
	fmt.Fprintf(&b, "%d tables.\n\n| Table | Columns | Primary key | References |\n|---|---|---|---|\n", len(tables))
	for _, t := range tables {
		var refs []string
		for r := range t.refs {
			if byName[r] != nil && r != t.name {
				refs = append(refs, fmt.Sprintf("[`%s`](#%s)", r, tableAnchor(side, r)))
			} else {
				refs = append(refs, "`"+r+"`")
			}
		}
		sort.Strings(refs)
		pk := ""
		if len(t.pk) > 0 {
			pk = "`" + strings.Join(t.pk, "`, `") + "`"
		}
		fmt.Fprintf(&b, "| [`%s`](#%s) | %d | %s | %s |\n", t.name, tableAnchor(side, t.name), len(t.cols), pk, strings.Join(refs, ", "))
	}
	for _, t := range tables {
		fmt.Fprintf(&b, "\n#### %s: `%s`\n\n", label, t.name)
		renderColumns(&b, t)
	}
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// Protocol vocabularies stored in the tables.

func emitOrderTypes(root string, args []string) (string, error) {
	if len(args) > 0 {
		return "", fmt.Errorf("takes no arguments")
	}
	p, err := loadProto(root)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| Order type | Constant | Meaning |\n|---|---|---|\n")
	n := 0
	for _, c := range p.consts {
		if c.typ != "OrderType" {
			continue
		}
		n++
		fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", c.value, c.name, cell(c.doc))
	}
	if n == 0 {
		return "", fmt.Errorf("protocol declares no OrderType constants")
	}
	return b.String(), nil
}

func emitSwapModes(root string, args []string) (string, error) {
	if len(args) > 0 {
		return "", fmt.Errorf("takes no arguments")
	}
	p, err := loadProto(root)
	if err != nil {
		return "", err
	}
	name := func(m protocol.SwapMode) (string, error) {
		c, ok := p.constFor("SwapMode", string(m))
		if !ok {
			return "", fmt.Errorf("no SwapMode constant holds %q", m)
		}
		return c.name, nil
	}
	configurable := map[protocol.SwapMode]bool{}
	var b strings.Builder
	b.WriteString("| Swap mode | Constant | Two robots |\n|---|---|---|\n")
	for _, m := range protocol.ConfigurableSwapModes() {
		configurable[m] = true
		n, err := name(m)
		if err != nil {
			return "", err
		}
		two := "no"
		if m.IsTwoRobot() {
			two = "yes"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", m, n, two)
	}
	var rest []string
	for _, m := range protocol.AllSwapModes() {
		if !configurable[m] {
			n, err := name(m)
			if err != nil {
				return "", err
			}
			rest = append(rest, fmt.Sprintf("`%s` (`%s`)", m, n))
		}
	}
	if len(rest) > 0 {
		fmt.Fprintf(&b, "\nIn `AllSwapModes` but not configurable (never stored on a claim): %s.\n", strings.Join(rest, ", "))
	}
	return b.String(), nil
}
