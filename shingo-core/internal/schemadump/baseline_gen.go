package schemadump

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"shingocore/store"
)

// BaselinePath is the committed baseline, relative to the shingo-core module
// root: Core's whole schema and seed rows as one SQL text, which store.migrate
// applies to a database with no tables. It is generated, never edited.
const BaselinePath = "store/schema/baseline.sql"

// CommentsPath holds the schema's rationale as COMMENT ON statements. The
// generator applies it before the dump, so the baseline carries every comment
// and a database born on it answers obj_description / col_description.
const CommentsPath = "store/schema/comments.sql"

// BaselineRegenCommand is what the staleness test tells you to run. The
// snapshot is regenerated after it, by a build that embeds the new baseline.
const BaselineRegenCommand = "make schema-baseline (go run ./cmd/schemabaseline), then make schema-snapshot"

// The sentinels the seed rows' times are stamped with before the dump, and
// what each becomes in the baseline. A seed row is written when the baseline
// runs, as the replay wrote it at boot, so its times are the boot's, not the
// generator's: a dumped literal would make every regeneration differ.
var timeSentinels = []struct {
	typ, literal, dumped, becomes string
}{
	{"timestamp with time zone", "'2000-01-01 00:00:00+00'::timestamptz", "'2000-01-01 00:00:00+00'", "now()"},
	{"timestamp without time zone", "'2000-01-01 00:00:00'::timestamp", "'2000-01-01 00:00:00'", "now()"},
	{"date", "'2000-01-01'::date", "'2000-01-01'", "CURRENT_DATE"},
}

// GenerateBaseline builds the baseline on this instance and returns its text.
//
//  1. An empty database is booted by RetiredChainRelease's own store.Open,
//     built from git (BuildRelease): the chain v1..RetiredChain, run by the
//     code the plants ran.
//  2. This tree's store.Open runs the migrations above RetiredChain.
//  3. CommentsPath is applied.
//  4. Every time column of every seed row is stamped with a sentinel.
//  5. pg_dump --inserts, schema and data, comments kept.
//  6. CleanBaseline.
//
// moduleRoot is the shingo-core directory; workDir holds the release build and
// is the caller's to remove.
func (i *Instance) GenerateBaseline(ctx context.Context, moduleRoot, workDir string) (string, error) {
	rel, err := BuildRelease(ctx, moduleRoot, store.RetiredChainRelease, workDir)
	if err != nil {
		return "", err
	}
	name, err := i.createDatabase("baseline")
	if err != nil {
		return "", err
	}
	if _, err := i.BootRelease(ctx, rel, name); err != nil {
		return "", err
	}
	db, err := store.Open(i.dbConfig(name))
	if err != nil {
		return "", fmt.Errorf("this tree's migrate on a %s database: %w", rel.Rev, err)
	}
	db.Close()

	comments, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(CommentsPath)))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", CommentsPath, err)
	}
	conn, err := sql.Open("pgx", fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=disable",
		i.host, i.port, name, pgUser, pgPass))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, string(comments)); err != nil {
		return "", fmt.Errorf("apply %s: %w", CommentsPath, err)
	}
	if err := stampSeedTimes(ctx, conn); err != nil {
		return "", err
	}
	dump, err := i.pgDump(ctx, name, "--inserts --no-owner --no-privileges", "PGTZ=UTC")
	if err != nil {
		return "", err
	}
	return CleanBaseline(dump)
}

// stampSeedTimes sets every non-null time column of every row in public to its
// type's sentinel, in one UPDATE per table. Triggers are off for it
// (session_replication_role = replica, on one connection): the stamp must
// change the time columns and nothing else.
func stampSeedTimes(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET session_replication_role = replica`); err != nil {
		return fmt.Errorf("disable triggers for the stamp: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT c.relname, a.attname, format_type(a.atttypid, NULL)
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r'
		   AND a.attnum > 0 AND NOT a.attisdropped
		   AND a.atttypid IN ('timestamptz'::regtype, 'timestamp'::regtype, 'date'::regtype)
		 ORDER BY c.relname, a.attnum`)
	if err != nil {
		return fmt.Errorf("list time columns: %w", err)
	}
	sets := map[string][]string{}
	var tables []string
	for rows.Next() {
		var table, col, typ string
		if err := rows.Scan(&table, &col, &typ); err != nil {
			rows.Close()
			return err
		}
		lit := ""
		for _, s := range timeSentinels {
			if s.typ == typ {
				lit = s.literal
			}
		}
		if lit == "" {
			rows.Close()
			return fmt.Errorf("%s.%s is %s, which has no sentinel", table, col, typ)
		}
		if _, ok := sets[table]; !ok {
			tables = append(tables, table)
		}
		sets[table] = append(sets[table], fmt.Sprintf("%q = CASE WHEN %q IS NULL THEN NULL ELSE %s END", col, col, lit))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range tables {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`UPDATE public.%q SET %s`, t, strings.Join(sets[t], ", "))); err != nil {
			return fmt.Errorf("stamp %s: %w", t, err)
		}
	}
	return nil
}

var (
	// pg_dump's own comment lines: the banner, and the header above each
	// object. Matched by shape, so a "--" line inside a function body or a
	// string is left alone.
	dumpHeaderLine = regexp.MustCompile(`^--( (Name|Data for Name|TOC entry|PostgreSQL database dump|Dumped from database version|Dumped by pg_dump version|Started on|Completed on)\b.*)?$`)
	// A session setting. Left in, one would outlive the baseline on the pool
	// connection that ran it: pg_dump's preamble turns statement_timeout and
	// lock_timeout off and empties search_path.
	dumpSessionLine = regexp.MustCompile(`^(SET |SELECT pg_catalog\.set_config\()`)
	// A date-shaped literal still in a seed row after the sentinels are
	// replaced: a time the stamp missed.
	leftoverDate = regexp.MustCompile(`'\d{4}-\d{2}-\d{2}`)
	// A schema_migrations row, as pg_dump --inserts writes it.
	migrationRow = regexp.MustCompile(`^INSERT INTO public\.schema_migrations VALUES \((\d+), now\(\)\);$`)
)

// CleanBaseline turns the generator's pg_dump into the baseline: pg_dump's
// header lines, every SET and set_config line and the \restrict/\unrestrict
// pair go; the time sentinels become now() (CURRENT_DATE for a date); blank
// runs collapse. It refuses a dump that carries a partition (partitions are
// made at runtime: store/heartbeat, store/robotconfidence), a seed row with a
// time it did not replace, or a schema_migrations missing a version of
// 1..RetiredChain.
func CleanBaseline(dump string) (string, error) {
	var kept []string
	recorded := map[int]bool{}
	maxRecorded := 0
	for line := range strings.SplitSeq(strings.ReplaceAll(dump, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, " \t")
		switch {
		case dumpHeaderLine.MatchString(line), dumpSessionLine.MatchString(line), dumpRestrictLine.MatchString(line):
			continue
		case strings.Contains(line, "PARTITION OF ") || strings.Contains(line, "ATTACH PARTITION "):
			return "", fmt.Errorf("the dump carries a partition, which the baseline must not: %s", line)
		}
		if strings.HasPrefix(line, "INSERT INTO ") {
			for _, s := range timeSentinels {
				line = strings.ReplaceAll(line, s.dumped, s.becomes)
			}
			if leftoverDate.MatchString(line) {
				return "", fmt.Errorf("a seed row keeps a dumped time; stamp its type: %s", line)
			}
			if m := migrationRow.FindStringSubmatch(line); m != nil {
				var v int
				fmt.Sscan(m[1], &v)
				recorded[v] = true
				maxRecorded = max(maxRecorded, v)
			}
		}
		kept = append(kept, line)
	}
	var missing []int
	for v := 1; v <= store.RetiredChain; v++ {
		if !recorded[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("the dump's schema_migrations is missing %d of v1..v%d (first v%d)", len(missing), store.RetiredChain, missing[0])
	}
	body := blankRun.ReplaceAllString(strings.Join(kept, "\n"), "\n\n")
	return baselineHeader(maxRecorded) + "\n" + strings.TrimSpace(body) + "\n", nil
}

func baselineHeader(head int) string {
	return strings.Join([]string{
		fmt.Sprintf("-- Core's schema at v%d with its seed rows: GENERATED, never edited by hand.", head),
		fmt.Sprintf("-- cmd/schemabaseline boots %s's chain (v1..v%d) on an empty database, runs this", store.RetiredChainRelease, store.RetiredChain),
		"-- tree's migrations above it, applies " + CommentsPath + ", and dumps the result with",
		"-- pg_dump --inserts. Regenerate: " + BaselineRegenCommand + ".",
		"-- store.migrate applies it, as one statement, to a database with no tables.",
	}, "\n") + "\n"
}
