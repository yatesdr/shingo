package testutil

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// StatementBreakdown summarises what a counted path sent to its database: the
// total, then each statement kind (SELECT, INSERT, UPDATE, DELETE, BEGIN,
// COMMIT, ...) with its count, then each (kind, table) pair. It reads the SQL a
// store's QueryCounter recorded, on either side: Core's pgx tracer and the
// Edge's counting driver hand it the same thing, the statement text.
//
// It is for budget EVIDENCE, logged beside a count: a number alone says a path
// got dearer, and the breakdown says whether it was a read or a write and which
// table. The table is the first one the statement names after FROM, INTO or
// UPDATE, which is right for the single-table statements that dominate these
// paths and names the driving table of a join.
func StatementBreakdown(stmts []string) string {
	kinds := map[string]int{}
	tables := map[string]int{}
	for _, s := range stmts {
		k, tbl := classifyStatement(s)
		kinds[k]++
		if tbl != "" {
			tables[k+" "+tbl]++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "total %d", len(stmts))
	for _, k := range sortedKeys(kinds) {
		fmt.Fprintf(&b, " · %s %d", k, kinds[k])
	}
	for _, k := range sortedKeys(tables) {
		fmt.Fprintf(&b, "\n    %-48s %d", k, tables[k])
	}
	return b.String()
}

var (
	leadingComment = regexp.MustCompile(`^(\s*--[^\n]*\n|\s*/\*.*?\*/)*\s*`)
	cteConsumer    = regexp.MustCompile(`(?i)\)\s*(insert|update|delete|select)\b`)
	tableAfter     = regexp.MustCompile(`(?is)\b(?:from|into|update)\s+([a-z_][a-z0-9_.]*)`)
)

func classifyStatement(sql string) (kind, table string) {
	s := leadingComment.ReplaceAllString(sql, "")
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "EMPTY", ""
	}
	kind = strings.ToUpper(strings.TrimRight(fields[0], ";("))
	if kind == "WITH" {
		// A CTE: name it by the statement that consumes it, the last one that
		// follows a closing parenthesis, which is what the database does work for.
		if m := cteConsumer.FindAllStringSubmatch(s, -1); len(m) > 0 {
			kind = "WITH/" + strings.ToUpper(m[len(m)-1][1])
		}
	}
	if m := tableAfter.FindStringSubmatch(s); m != nil {
		table = strings.ToLower(m[1])
	}
	return kind, table
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
