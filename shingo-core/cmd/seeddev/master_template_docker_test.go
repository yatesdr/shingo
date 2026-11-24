//go:build docker

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/internal/testdb"
	"shingocore/plantspec"
	"shingocore/store"
)

// master_template_docker_test.go — the master plant as a seeded template.
//
// seedCore on plants/master.yaml is 3.3-5.2 s a test alone (docket T4), and
// most of this package's docker tests want exactly its rows: the planner
// goldens, the kanban rig and its pins, the lanes family and the order-carts
// fixture. seedOfPlant names that seeding as a testdb.Seed; it is written once
// a process into a template, before the leaves start
// (TestMasterTemplate_IsBuiltBeforeTheLeaves), and each of those tests takes a
// clone of it (openSeededPlant). The clone is pinned as the plant a fresh
// seeding writes, at two ages of the template.
//
// The tests whose subject is the seeder itself (TestSeedCore_*,
// TestSeedDemo_KanbanIsSeededAtItsPools, TestSeedKanbanPress_CoreHoldsTheKanbans,
// TestPinSeedCore_*) still call seedCore: the seeder is proven by a seeding.

// seedOfPlant is seedCore of plant as a seeded template. The key is the plant's
// own content: Plant has no unexported field and is decoded with KnownFields
// (plantspec.Load), so its JSON is everything the file said, and a test that
// changes its plant gets a template of its own.
func seedOfPlant(t *testing.T, plant *plantspec.Plant) testdb.Seed {
	t.Helper()
	content, err := json.Marshal(plant)
	testutil.MustNoErr(t, err, "key the plant")
	return testdb.Seed{
		Key:   fmt.Sprintf("seeddev seedCore %x", sha256.Sum256(content)),
		Write: func(db *store.DB) error { return seedCore(db, plant, map[string]int64{}) },
	}
}

// openSeededPlant is a database of t's own holding plant as seedCore writes it,
// cloned from the plant's template (seedOfPlant). The master's is built before
// the leaves start; another plant's is built by its first asker. A clone is a
// fresh seeding in all but its times, which are the template's
// (TestMasterTemplate_ACloneIsAFreshSeedAtTwoAges).
func openSeededPlant(t *testing.T, plant *plantspec.Plant) (*store.DB, *config.DatabaseConfig) {
	t.Helper()
	return testdb.OpenSeeded(t, seedOfPlant(t, plant))
}

// TestMasterTemplate_IsBuiltBeforeTheLeaves builds the master plant's template
// while no other test of this package runs. go test runs every top-level test
// that does not call t.Parallel before it lets any that does go on, wherever it
// is declared (testdb seeded.go, BUILT BEFORE THE LEAVES), so no leaf waits on
// the seeding or holds a slot through it. With a -run that leaves this test
// out, the first test to ask builds it instead.
//
// serial: it builds the template the parallel tests clone, and must end before any of them goes on
func TestMasterTemplate_IsBuiltBeforeTheLeaves(t *testing.T) {
	buildMasterTemplate(t)
}

// masterBuild is the master plant's template as this process built it, with the
// image of a clone taken as soon as it was built and the template's age then.
var masterBuild struct {
	once     sync.Once
	ok       bool
	seed     testdb.Seed
	young    map[string][]string
	youngAge time.Duration
}

// buildMasterTemplate builds the master plant's template, if this process has
// not, and takes a clone of it at once; every later caller gets that build.
func buildMasterTemplate(t *testing.T) (testdb.Seed, map[string][]string, time.Duration) {
	t.Helper()
	masterBuild.once.Do(func() {
		seed := seedOfPlant(t, masterPlant(t))
		testdb.BuildSeeded(t, seed)
		clone, _ := testdb.OpenSeeded(t, seed)
		age, built := testdb.SeededAge(seed)
		if !built {
			t.Fatal("the master plant's template was cloned but SeededAge says it is not built")
		}
		masterBuild.seed, masterBuild.young, masterBuild.youngAge = seed, plantImage(t, clone), age
		masterBuild.ok = true
	})
	if !masterBuild.ok {
		t.Fatal("the master plant's template did not build: the first test that built it says why")
	}
	return masterBuild.seed, masterBuild.young, masterBuild.youngAge
}

// TestMasterTemplate_ACloneIsAFreshSeedAtTwoAges: the master plant seeded twice
// is one plant, and a clone of its template is that plant, both when the clone
// is taken as the template is built and when it is taken after other tests have
// cloned it and written their clones.
func TestMasterTemplate_ACloneIsAFreshSeedAtTwoAges(t *testing.T) {
	t.Parallel()
	plant := masterPlant(t)
	seed, young, youngAge := buildMasterTemplate(t)

	// Two fresh seedings, side by side.
	fresh, again := testdb.Open(t), testdb.Open(t)
	errs := make(chan error, 2)
	for _, db := range []*store.DB{fresh, again} {
		go func() { errs <- seedCore(db, plant, map[string]int64{}) }()
	}
	for range 2 {
		testutil.MustNoErr(t, <-errs, "seed the master fresh")
	}
	want := plantImage(t, fresh)
	if len(want["bins"]) == 0 || len(want["nodes"]) == 0 {
		t.Fatalf("a fresh seeding has %d bins and %d nodes: nothing to compare", len(want["bins"]), len(want["nodes"]))
	}

	t.Run("seeded twice, one plant", func(t *testing.T) {
		sameWorld(t, "a second fresh seeding", want, plantImage(t, again))
	})
	t.Run("a clone taken as the template was built", func(t *testing.T) {
		t.Logf("the template was %.1f s old when this clone was taken", youngAge.Seconds())
		sameWorld(t, "the clone", want, young)
	})
	// Three tests on clones of the template, each writing its clone as the
	// package's tests write theirs (the claims mirror and an engine's boot), and
	// then wrecking it.
	t.Run("other tests write their clones", func(t *testing.T) {
		for i := range 3 {
			t.Run(fmt.Sprintf("clone %d", i), func(t *testing.T) {
				t.Parallel()
				db, dbcfg := testdb.OpenSeeded(t, seed)
				seedClaimsMirror(t, db, plant)
				cdb, _, err := store.OpenCounting(dbcfg)
				testutil.MustNoErr(t, err, "open the counting store")
				t.Cleanup(func() { cdb.Close() })
				cfg := config.Defaults()
				cfg.Messaging.StationID = "test-core"
				cfg.Messaging.DispatchTopic = "shingo.dispatch"
				eng := engine.New(engine.Config{AppConfig: cfg, DB: cdb, Fleet: testdb.NewTrackingBackend(), LogFunc: func(string, ...any) {}})
				eng.Start()
				eng.Stop()
				for _, write := range []string{`UPDATE bins SET locked = true`, `DELETE FROM audit_log`} {
					_, err := db.DB.Exec(write)
					testutil.MustNoErr(t, err, write)
				}
			})
		}
	})
	t.Run("a clone taken after other tests wrote theirs", func(t *testing.T) {
		clone, _ := testdb.OpenSeeded(t, seed)
		age, _ := testdb.SeededAge(seed)
		t.Logf("the template was %.1f s old when this clone was taken (%.1f s for the first)", age.Seconds(), youngAge.Seconds())
		if age <= youngAge {
			t.Fatalf("this clone's template is %v old, the first clone's was %v: the two are not two ages", age, youngAge)
		}
		sameWorld(t, "the clone", want, plantImage(t, clone))
	})
}

// sameWorld fails t with the first difference of each part of two plant images.
func sameWorld(t *testing.T, what string, want, got map[string][]string) {
	t.Helper()
	for part := range got {
		if _, ok := want[part]; !ok {
			t.Errorf("%s has %s, which a fresh seeding does not", what, part)
		}
	}
	parts := make([]string, 0, len(want))
	for part := range want {
		parts = append(parts, part)
	}
	sort.Strings(parts)
	for _, part := range parts {
		w, g := want[part], got[part]
		if slices.Equal(w, g) {
			continue
		}
		i := 0
		for i < len(w) && i < len(g) && w[i] == g[i] {
			i++
		}
		at := func(s []string) string {
			if i < len(s) {
				return s[i]
			}
			return "(none)"
		}
		t.Errorf("%s: %d rows in a fresh seeding, %d in %s; first difference\n fresh: %s\n other: %s",
			part, len(w), len(g), what, at(w), at(g))
	}
}

// plantImage is a database's contents by table: every row of every table in
// the public schema, sorted, with each date or time column replaced by whether
// it is set; each sequence's last value; and the bins' loaded ages against the
// newest, to the minute (the rounding seedGoldenWorld's FIFO normalise gives
// them). audit_log's ids are compared with the rest: a kanban save writes its
// pools' rows in the families' code order
// (TestSaveKanban_PoolRowsComeInFamilyCodeOrder), so two fresh seedings number
// them alike.
func plantImage(t *testing.T, db *store.DB) map[string][]string {
	t.Helper()
	query := func(q string, args ...any) [][2]string {
		rows, err := db.DB.Query(q, args...)
		testutil.MustNoErr(t, err, q)
		defer rows.Close()
		var out [][2]string
		for rows.Next() {
			var a, b string
			testutil.MustNoErr(t, rows.Scan(&a, &b), q)
			out = append(out, [2]string{a, b})
		}
		testutil.MustNoErr(t, rows.Err(), q)
		return out
	}

	times := map[string][]string{}
	for _, c := range query(`SELECT table_name::text, column_name::text FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND data_type IN ('date', 'time with time zone', 'time without time zone',
		                    'timestamp with time zone', 'timestamp without time zone')`) {
		times[c[0]] = append(times[c[0]], c[1])
	}
	image := map[string][]string{}
	for _, tb := range query(`SELECT table_name::text, '' FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY 1`) {
		table := tb[0]
		out := []string{}
		for _, r := range query(fmt.Sprintf(`SELECT to_jsonb(r)::text, '' FROM public."%s" r`, table)) {
			var row map[string]json.RawMessage
			testutil.MustNoErr(t, json.Unmarshal([]byte(r[0]), &row), "a row of "+table)
			for _, c := range times[table] {
				if string(row[c]) != "null" {
					row[c] = json.RawMessage(`"set"`)
				}
			}
			masked, err := json.Marshal(row)
			testutil.MustNoErr(t, err, "a row of "+table)
			out = append(out, string(masked))
		}
		sort.Strings(out)
		image[table] = out
	}
	for _, s := range query(`SELECT sequencename::text, COALESCE(last_value, 0)::text
		FROM pg_sequences WHERE schemaname = 'public' ORDER BY 1`) {
		image["(sequences)"] = append(image["(sequences)"], s[0]+"="+s[1])
	}
	for _, b := range query(`SELECT label, ROUND(EXTRACT(EPOCH FROM (loaded_at - MAX(loaded_at) OVER ())) / 60)::text
		FROM bins WHERE loaded_at IS NOT NULL ORDER BY 1`) {
		image["(bin ages, minutes)"] = append(image["(bin ages, minutes)"], b[0]+"="+b[1])
	}
	return image
}
