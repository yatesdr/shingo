//go:build sim

package main

import (
	"strings"
)

// statementName names a statement by its verb and PRINCIPAL table: the FROM of
// the outermost SELECT, or the target of an INSERT / REPLACE / UPDATE / DELETE.
//
// It reads the SQL as tokens with parenthesis depth rather than as a regular
// expression, because the Edge store's statements nest: every claim read
// carries `COALESCE((SELECT pc.uop_capacity FROM payload_catalog pc ...))` in
// its select list, and the plant-claims publisher's styles read carries a
// sub-select whose own FROM is a sub-query. A first-FROM rule names the former
// payload_catalog and the latter process_nodes, so the work lands on a table
// that was only consulted. Here only a FROM at the outermost depth counts; a
// FROM sub-query is followed to its own principal table, and a CTE name is
// resolved to its body's principal table. Comments and string literals are
// dropped before tokenising so a word inside them is never read as a keyword.
//
// Table names are lower-cased (SQLite identifiers are case-insensitive) and a
// schema qualifier ("main.") is dropped. A SELECT with no FROM is named "-". A
// statement that is none of the five verbs is "OTHER" and its first word.
func statementName(q string) (verb, table string) {
	return nameTokens(sqlTokens(q), nil, 0)
}

// keyOf is statementName as one "VERB table" string.
func keyOf(q string) string {
	v, t := statementName(q)
	return v + " " + t
}

// sqlTokens splits SQL into identifiers / keywords (quoted identifiers
// unquoted), single punctuation characters, and a placeholder for each string
// literal, dropping whitespace and comments.
func sqlTokens(q string) []string {
	var out []string
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			for i < len(q) && q[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			end := strings.Index(q[i+2:], "*/")
			if end < 0 {
				i = len(q)
			} else {
				i += end + 4
			}
		case c == '\'':
			i++
			for i < len(q) {
				if q[i] == '\'' {
					if i+1 < len(q) && q[i+1] == '\'' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			i++
			out = append(out, "'")
		case c == '"' || c == '`' || c == '[':
			closer := c
			if c == '[' {
				closer = ']'
			}
			end := strings.IndexByte(q[i+1:], closer)
			if end < 0 {
				out = append(out, q[i+1:])
				i = len(q)
			} else {
				out = append(out, q[i+1:i+1+end])
				i += end + 2
			}
		case isIdentByte(c):
			j := i
			for j < len(q) && isIdentByte(q[j]) {
				j++
			}
			out = append(out, q[i:j])
			i = j
		default:
			out = append(out, q[i:i+1])
			i++
		}
	}
	return out
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isKw(toks []string, i int, kw string) bool {
	return i < len(toks) && strings.EqualFold(toks[i], kw)
}

// closeParen returns the index of the ")" matching the "(" at toks[open], or
// len(toks) when it is unbalanced.
func closeParen(toks []string, open int) int {
	depth := 0
	for i := open; i < len(toks); i++ {
		switch toks[i] {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(toks)
}

// tableAt reads a possibly schema-qualified table name at toks[i].
func tableAt(toks []string, i int) string {
	if i >= len(toks) {
		return "-"
	}
	if i+2 < len(toks) && toks[i+1] == "." && isIdentByte(toks[i+2][0]) {
		i += 2
	}
	return strings.ToLower(toks[i])
}

// maxCTEDepth bounds CTE resolution: a recursive CTE names itself in its body.
const maxCTEDepth = 4

func nameTokens(toks []string, ctes map[string][]string, depth int) (verb, table string) {
	if len(toks) == 0 {
		return "OTHER", ""
	}
	i := 0
	if isKw(toks, 0, "WITH") {
		ctes = withCTEs(ctes)
		i = 1
		if isKw(toks, i, "RECURSIVE") {
			i++
		}
		for i < len(toks) {
			name := strings.ToLower(toks[i])
			i++
			if i < len(toks) && toks[i] == "(" { // column list
				i = closeParen(toks, i) + 1
			}
			for i < len(toks) && toks[i] != "(" { // AS [NOT] [MATERIALIZED]
				i++
			}
			end := closeParen(toks, i)
			if i < len(toks) {
				ctes[name] = toks[i+1 : min(end, len(toks))]
			}
			i = end + 1
			if i < len(toks) && toks[i] == "," {
				i++
				continue
			}
			break
		}
		if i >= len(toks) {
			return "OTHER", "WITH"
		}
	}
	switch strings.ToUpper(toks[i]) {
	case "SELECT", "VALUES":
		t := selectFrom(toks[i:], ctes, depth)
		return "SELECT", t
	case "INSERT", "REPLACE":
		for i < len(toks) && !isKw(toks, i, "INTO") {
			i++
		}
		return "INSERT", tableAt(toks, i+1)
	case "UPDATE":
		i++
		if isKw(toks, i, "OR") {
			i += 2
		}
		return "UPDATE", tableAt(toks, i)
	case "DELETE":
		i++
		if isKw(toks, i, "FROM") {
			i++
		}
		return "DELETE", tableAt(toks, i)
	}
	return "OTHER", strings.ToUpper(toks[i])
}

func withCTEs(outer map[string][]string) map[string][]string {
	m := make(map[string][]string, len(outer)+2)
	for k, v := range outer {
		m[k] = v
	}
	return m
}

// selectFrom is the table of the first FROM at the outermost depth of a
// SELECT: a sub-query there is followed to its own principal table, a CTE name
// to its body's.
func selectFrom(toks []string, ctes map[string][]string, depth int) string {
	d := 0
	for i := 0; i < len(toks); i++ {
		switch {
		case toks[i] == "(":
			d++
		case toks[i] == ")":
			d--
		case d == 0 && isKw(toks, i, "FROM"):
			if i+1 < len(toks) && toks[i+1] == "(" {
				end := closeParen(toks, i+1)
				_, t := nameTokens(toks[i+2:min(end, len(toks))], ctes, depth)
				return t
			}
			t := tableAt(toks, i+1)
			if body, ok := ctes[t]; ok && depth < maxCTEDepth {
				inner := withCTEs(ctes)
				delete(inner, t) // a recursive CTE's self-reference names the CTE
				if _, bt := nameTokens(body, inner, depth+1); bt != "-" && bt != "" {
					return bt
				}
			}
			return t
		}
	}
	return "-"
}
