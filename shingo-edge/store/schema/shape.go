package schema

import (
	"slices"
	"strings"
)

// A schema's shape, read from the CREATE statements SQLite stores: what a
// start holds the live file to (against the baseline) and what the vintage
// test compares. It reads text and runs no statement, so a start's check costs
// one catalogue read.
//
// Two spellings of one shape are one shape: comments, whitespace, letter case
// and quotes around a name do not count, nor does the order of a table's
// columns. What does: a column's type and constraints as written, the table's
// own constraints and options, and each index, trigger and view.

// Shape maps an object's kind and name ("column orders.status") to its
// definition.
type Shape map[string]string

// ParseShape reads every CREATE statement in script; any other statement, and
// SQLite's own tables, are left out.
func ParseShape(script string) Shape {
	s := Shape{}
	var stmt []string
	trigger, nested := false, 0
	for _, tok := range tokens(script) {
		if tok == ";" && nested <= 0 {
			s.add(stmt)
			stmt, trigger, nested = nil, false, 0
			continue
		}
		stmt = append(stmt, tok)
		if len(stmt) <= 3 && tok == "trigger" && stmt[0] == "create" {
			trigger = true
		}
		// A trigger's body has its own semicolons, between BEGIN and its END.
		if trigger {
			switch tok {
			case "begin", "case":
				nested++
			case "end":
				nested--
			}
		}
	}
	s.add(stmt)
	return s
}

func (s Shape) add(t []string) {
	if len(t) < 3 || t[0] != "create" {
		return
	}
	i := 1
	for i < len(t)-1 && slices.Contains([]string{"unique", "temp", "temporary", "virtual"}, t[i]) {
		i++
	}
	kind, qualifiers := t[i], t[1:i]
	i++
	if i+3 < len(t) && t[i] == "if" && t[i+1] == "not" && t[i+2] == "exists" {
		i += 3
	}
	if i >= len(t) || strings.HasPrefix(t[i], "sqlite_") {
		return
	}
	name := t[i]
	rest := t[i+1:]
	if kind != "table" || len(rest) == 0 || rest[0] != "(" {
		s[kind+" "+name] = strings.Join(slices.Concat(qualifiers, rest), " ")
		return
	}
	var items [][]string
	var item []string
	depth, end := 0, len(rest)
	for j, tok := range rest {
		switch {
		case tok == "(":
			depth++
			if depth == 1 {
				continue
			}
		case tok == ")":
			depth--
			if depth == 0 {
				end = j + 1
			}
		case tok == "," && depth == 1:
			items = append(items, item)
			item = nil
			continue
		}
		if depth == 0 {
			break
		}
		item = append(item, tok)
	}
	items = append(items, item)
	s["table "+name] = strings.Join(rest[end:], " ")
	for _, it := range items {
		if len(it) == 0 {
			continue
		}
		switch it[0] {
		case "primary", "unique", "foreign", "check", "constraint":
			s["constraint "+name+"."+strings.Join(it, " ")] = ""
		default:
			s["column "+name+"."+it[0]] = strings.Join(it[1:], " ")
		}
	}
}

// tokens splits SQL into words, quoted strings and single punctuation marks: a
// word in lower case, a quoted name without its quotes, comments gone.
func tokens(sql string) []string {
	var out []string
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case strings.HasPrefix(sql[i:], "--"):
			n := strings.IndexByte(sql[i:], '\n')
			if n < 0 {
				return out
			}
			i += n + 1
		case strings.HasPrefix(sql[i:], "/*"):
			n := strings.Index(sql[i+2:], "*/")
			if n < 0 {
				return out
			}
			i += 2 + n + 2
		case c == '\'' || c == '"' || c == '`' || c == '[':
			closing := c
			if c == '[' {
				closing = ']'
			}
			j := i + 1
			for ; j < len(sql); j++ {
				if sql[j] == closing {
					if c != '[' && j+1 < len(sql) && sql[j+1] == closing {
						j++
						continue
					}
					break
				}
			}
			end := min(j+1, len(sql))
			if c == '\'' {
				out = append(out, sql[i:end])
			} else {
				out = append(out, strings.ToLower(sql[i+1:min(j, len(sql))]))
			}
			i = end
		case wordByte(c):
			j := i
			for j < len(sql) && wordByte(sql[j]) {
				j++
			}
			out = append(out, strings.ToLower(sql[i:j]))
			i = j
		default:
			out = append(out, sql[i:i+1])
			i++
		}
	}
	return out
}

func wordByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 ||
		('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// Compare holds got to want. missing is the tables and columns got lacks,
// which the code that shipped want depends on. differs is every other
// difference, one sentence each naming the object; a table only one side has
// is one line, not one for each object it would carry. Both are sorted.
func Compare(want, got Shape) (missing, differs []string) {
	for key, def := range want {
		kind, _, _ := strings.Cut(key, " ")
		if t := owner(key, def); t != "" && kind != "table" {
			if _, ok := got["table "+t]; !ok {
				continue
			}
		}
		have, ok := got[key]
		switch {
		case !ok && (kind == "table" || kind == "column"):
			missing = append(missing, key)
		case !ok:
			differs = append(differs, key+" is missing (this build ships it)")
		case have != def:
			differs = append(differs, key+" differs: the database has "+have+", this build ships "+def)
		}
	}
	for key, def := range got {
		if _, ok := want[key]; ok {
			continue
		}
		kind, _, _ := strings.Cut(key, " ")
		if t := owner(key, def); t != "" && kind != "table" {
			if _, ok := want["table "+t]; !ok {
				continue
			}
		}
		differs = append(differs, key+" is in the database and not in this build's schema")
	}
	slices.Sort(missing)
	slices.Sort(differs)
	return missing, differs
}

// owner is the table an object belongs to: a column's or a constraint's by its
// key, an index's or a trigger's by the word after ON.
func owner(key, def string) string {
	kind, name, _ := strings.Cut(key, " ")
	switch kind {
	case "table":
		return name
	case "column", "constraint":
		t, _, _ := strings.Cut(name, ".")
		return t
	case "index", "trigger":
		words := strings.Fields(def)
		if i := slices.Index(words, "on"); i >= 0 && i+1 < len(words) {
			return words[i+1]
		}
	}
	return ""
}
