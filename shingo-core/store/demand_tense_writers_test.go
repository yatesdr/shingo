package store_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"shingocore/store"
)

// demand_tense_writers_test.go — no production path writes a forecast.
//
// A forecast is a demand_origins row with opened_at NULL and closed_at NULL.
// Migration v153 makes such a row legal; nothing in this phase may write one.
// The claim is about source text plus one type, so no database is needed:
//
//   - the only INSERT INTO demand_origins sites are UpsertDemandOrigin and
//     OpenCoreEpisode, and
//   - both bind DemandOrigin.OpenedAt, a non-pointer time.Time, which
//     database/sql never sends as NULL (a zero time binds as year 1), and
//   - no statement anywhere sets opened_at to NULL.
//
// A new writer, or OpenedAt turning into a pointer, fails here and has to say
// whether it writes a forecast on purpose.

var demandOriginsInsert = regexp.MustCompile(`INSERT\s+INTO\s+demand_origins\b[^_]`)
var openedAtSetNull = regexp.MustCompile(`(?i)opened_at\s*=\s*NULL`)

// enclosingFunc names the top-level func declaration a byte offset falls in.
func enclosingFunc(src string, off int) string {
	i := strings.LastIndex(src[:off], "\nfunc ")
	if i < 0 {
		return "<file scope>"
	}
	line := src[i+1:]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	// "func (db *DB) Name(" or "func Name(" -> Name
	line = strings.TrimPrefix(line, "func ")
	if strings.HasPrefix(line, "(") {
		if j := strings.Index(line, ") "); j >= 0 {
			line = line[j+2:]
		}
	}
	if j := strings.IndexByte(line, '('); j >= 0 {
		line = line[:j]
	}
	return line
}

func TestDemandTense_NoProductionWriterWritesAForecast(t *testing.T) {
	t.Parallel()

	f, ok := reflect.TypeOf(store.DemandOrigin{}).FieldByName("OpenedAt")
	if !ok || f.Type != reflect.TypeOf(time.Time{}) {
		t.Fatalf("DemandOrigin.OpenedAt is %v, want a non-pointer time.Time: a pointer can bind NULL, "+
			"so the two writers below could write a forecast", f.Type)
	}

	want := []string{"demand_origins.go:OpenCoreEpisode", "demand_origins.go:UpsertDemandOrigin"}
	var got []string
	root := ".." // shingo-core
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		rel := filepath.ToSlash(path)
		for _, loc := range demandOriginsInsert.FindAllStringIndex(src, -1) {
			got = append(got, filepath.Base(rel)+":"+enclosingFunc(src, loc[0]))
		}
		for _, loc := range openedAtSetNull.FindAllStringIndex(src, -1) {
			t.Errorf("%s (%s) sets opened_at to NULL: that writes a forecast", rel, enclosingFunc(src, loc[0]))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("INSERT INTO demand_origins sites = %v, want exactly %v.\n"+
			"A new writer must bind a non-pointer opened_at, or it can write a forecast; "+
			"add it here only once that is shown.", got, want)
	}
}
