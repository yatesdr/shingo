package engine

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/messaging"
	"shingoedge/store"
)

// owned_test.go pins the Edge-owned map (owned.go): the rows, the quote, the
// answer from a Core that speaks the map and from an older one, the resend
// guard, the keeps (a quiet ack settles, the first ack after boot reconciles,
// an edit moves the quote, the floor), the cell catalog, and what a quiet
// heartbeat and a quiet ack cost.

// claimsEngine is an engine with the plant-claims publisher wired as
// cmd/shingoedge main.go wires it: the claims family reads and sends through
// it, and its edit and full publishes report to the engine.
func claimsEngine(t *testing.T, db *store.DB) (*Engine, *messaging.PlantClaimsPublisher) {
	t.Helper()
	e := New(Config{AppConfig: &config.Config{StationUID: "edge.owned"}, DB: db, LogFunc: func(string, ...any) {}})
	pub := messaging.NewPlantClaimsPublisher(db, "edge.owned", 0)
	pub.Reported = e.ClaimsReported
	pub.ReportedAll = e.ClaimsReportedAll
	e.SetClaimsSource(pub)
	return e, pub
}

// seedStyledProcess makes a process with styles styles of one produce claim
// each and returns its id.
func seedStyledProcess(t *testing.T, db *store.DB, name string, styles int) int64 {
	t.Helper()
	pid, err := db.CreateProcess(name, "", "", "", false)
	testutil.MustNoErr(t, err, "create process "+name)
	for s := 0; s < styles; s++ {
		sid, err := db.CreateStyle(fmt.Sprintf("%s-S%d", name, s), "", pid)
		testutil.MustNoErr(t, err, "create style")
		seedProduceClaim(t, db, sid, "QA-N0", fmt.Sprintf("SYN-%s-%d", name, s))
	}
	return pid
}

// claimsSent is "process:<styles>" for every unsent plant-claims report, sorted,
// and acks them.
func claimsSent(t *testing.T, db *store.DB) []string {
	t.Helper()
	msgs, err := db.ListUnsentOutboxByType([]string{protocol.SubjectPlantClaims})
	testutil.MustNoErr(t, err, "list plant-claims outbox")
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		r := decodePlantClaimsReport(t, m.Payload)
		out = append(out, fmt.Sprintf("%s:%d", r.ProcessID, len(r.Styles)))
		testutil.MustNoErr(t, db.AckOutbox(m.ID), "ack outbox row")
	}
	sort.Strings(out)
	return out
}

// drainClaimsReports acks every unsent plant-claims report in the outbox and
// returns process → digest of the reports it found.
func drainClaimsReports(t *testing.T, db *store.DB) map[string]string {
	t.Helper()
	msgs, err := db.ListUnsentOutboxByType([]string{protocol.SubjectPlantClaims})
	if err != nil {
		t.Fatalf("list plant-claims outbox: %v", err)
	}
	out := map[string]string{}
	for _, m := range msgs {
		r := decodePlantClaimsReport(t, m.Payload)
		out[r.ProcessID] = r.Digest
		if err := db.AckOutbox(m.ID); err != nil {
			t.Fatalf("ack outbox row: %v", err)
		}
	}
	return out
}

// ownedDigest is protocol.OwnedDigest, failing the test on an error.
func ownedDigest(t *testing.T, members map[string]string) string {
	t.Helper()
	d, err := protocol.OwnedDigest(members)
	testutil.MustNoErr(t, err, "owned digest")
	return d
}

// ownedBuilds is how many times each family has been built.
func ownedBuilds(e *Engine) map[string]int {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	out := map[string]int{}
	for k, h := range e.feeds.owned {
		out[k] = h.builds
	}
	return out
}

// differs is an owned answer for a family Core holds otherwise: the digest no
// quote can be, and the members Core holds.
func differs(members map[string]string) protocol.OwnedAnswer {
	return protocol.OwnedAnswer{Digest: "not-the-quote", Members: members}
}

// TestOwnedRows_CoverAllOwned: every family protocol.AllOwned names has one
// row, with a build, a send and a guard key that reads back to its member.
func TestOwnedRows_CoverAllOwned(t *testing.T) {
	t.Parallel()
	rows := map[string]int{}
	for _, r := range ownedRows {
		rows[r.key]++
		if r.build == nil || r.send == nil || r.guard == nil || r.member == nil {
			t.Errorf("family %q: a row needs a build, a send and a guard key", r.key)
			continue
		}
		member := "P1"
		if r.key != protocol.OwnedClaims {
			member = protocol.SingletonMember
		}
		if got, ok := r.member(r.guard(member)); !ok || got != member {
			t.Errorf("family %q: guard key %q reads back as %q (%v), want %q", r.key, r.guard(member), got, ok, member)
		}
	}
	for _, k := range protocol.AllOwned() {
		if rows[k] != 1 {
			t.Errorf("family %q has %d rows in ownedRows, want 1", k, rows[k])
		}
	}
	if len(rows) != len(protocol.AllOwned()) {
		t.Errorf("ownedRows has %d families, protocol.AllOwned %d", len(rows), len(protocol.AllOwned()))
	}
}

// The claims family is answered only where Core answers it: from an older
// Core, its Claims map (nil is a Core that does not quote claims, or could
// not read them: nothing read, nothing sent; {} is a Core holding none, and
// the styled process is sent); from a Core that speaks the owned map, its
// Owned answer, and then Claims is not read at all.
func TestOnCoreAck_ClaimsReachThePublisherOnlyWhenSent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ack  *protocol.EdgeHeartbeatAck
		want []string
	}{
		{"older Core: no feeds, no claims", &protocol.EdgeHeartbeatAck{}, nil},
		{"older Core, claims unread", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}}, nil},
		{"older Core holding none", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Claims: map[string]string{}}, []string{"P1:1"}},
		{"older Core holding another digest", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Claims: map[string]string{"P1": "d1"}}, []string{"P1:1"}},
		{"owned map, claims left off", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Owned: map[string]protocol.OwnedAnswer{},
			Claims: map[string]string{}}, nil},
		{"owned map, Core holding none", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{},
			Owned: map[string]protocol.OwnedAnswer{protocol.OwnedClaims: differs(nil)}}, []string{"P1:1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			e, _ := claimsEngine(t, db)
			seedStyledProcess(t, db, "P1", 1)
			e.OwnedDigests()
			e.OnCoreAck(tc.ack)
			if got := claimsSent(t, db); !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Errorf("published %v, want %v", got, tc.want)
			}
		})
	}
}

// The claims keys share the feeds' resend guard: the same digest inside the
// guard is held back, a new digest goes, the third send in a row flags the
// process on /status, and settling the family with one process still pending
// clears every other process's flag and keeps its own.
func TestOwned_ClaimsGuardAndFlag(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	steps := []struct {
		process, digest string
		want            bool
	}{
		{"P1", "d1", true},
		{"P1", "d1", false}, // in flight
		{"P1", "d2", true},
		{"P1", "d3", true}, // third send in a row
		{"P2", "e1", true},
		{"P2", "e2", true},
		{"P2", "e3", true},
	}
	for i, s := range steps {
		if got := e.sendDue(claimsGuardKey(s.process), s.digest, ""); got != s.want {
			t.Fatalf("step %d: sendDue(%s, %s) = %v, want %v", i, s.process, s.digest, got, s.want)
		}
	}
	want := []string{"claims:P1 not converging: sent 3 times", "claims:P2 not converging: sent 3 times"}
	if got := e.FeedFlags(); !reflect.DeepEqual(got, want) {
		t.Fatalf("flags = %v, want %v", got, want)
	}
	row, _ := ownedRowOf(protocol.OwnedClaims)
	e.settleOwned(row, map[string]bool{"P2": true})
	if got := e.FeedFlags(); !reflect.DeepEqual(got, want[1:]) {
		t.Errorf("flags after P1 settled = %v, want %v", got, want[1:])
	}
}

// TestOnCoreAck_AQuietAckRunsNoStatementAndNoHash replaces pin 9 (a quiet ack
// read 1 + 2P statements and hashed P + 1 times). With every feed confirmed and
// every family quoted back at this Edge's digest, a heartbeat's quote and an
// ack, from a Core that speaks the owned map and from an older one quoting
// Claims and BinWatch, run no statement (the store's counting driver), build
// no family (ownedBuilds) and send nothing. Every owned digest is computed in
// a build or a send (TestOwned_EveryOwnedDigestIsInABuildOrASend), so none ran
// either. At three process counts and a style count that differs per
// process, so a count that grew with P would show.
//
// Not parallel: the stations' bin lists the watch family reads are held under
// processes.NodeGeneration, which is process-wide, so a parallel test writing
// process_nodes would force a rebuild inside the count.
func TestOnCoreAck_AQuietAckRunsNoStatementAndNoHash(t *testing.T) {
	for _, p := range []int{1, 3, 6} {
		t.Run(fmt.Sprintf("P=%d", p), func(t *testing.T) {
			db, counter := testEngineDBCounting(t)
			e, _ := claimsEngine(t, db)
			e.SetCellCatalogFunc(func() ([]protocol.CellCatalogEntry, error) {
				pts, err := db.ListCountedReportingPoints()
				if err != nil {
					return nil, err
				}
				return messaging.BuildCellCatalog(pts), nil
			})
			sent := 0
			e.SetSendFunc(func(*protocol.Envelope) error { sent++; return nil })
			// Process i has i%3+1 live styles.
			for i := 0; i < p; i++ {
				seedStyledProcess(t, db, fmt.Sprintf("QA-PROC-%02d", i), i%3+1)
			}

			// Core's quote is this Edge's own: learned off the reports a Core
			// holding none makes this Edge send.
			quote := e.OwnedDigests()
			if len(quote) != len(protocol.AllOwned()) {
				t.Fatalf("the heartbeat quotes %v, want every family", quote)
			}
			e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Owned: map[string]protocol.OwnedAnswer{
				protocol.OwnedClaims: differs(nil)}})
			reports := drainClaimsReports(t, db)
			if len(reports) != p {
				t.Fatalf("learned %d process digests, want %d: the fixture no longer publishes one report per process", len(reports), p)
			}
			feeds := map[string]string{}
			for _, key := range protocol.AllFeeds() {
				d := "held-" + key
				e.holdFeed(key, d, "")
				feeds[key] = d
			}
			e.feeds.mu.Lock()
			watch := e.feeds.ownedLocked(protocol.OwnedBinWatch).raw
			e.feeds.mu.Unlock()
			owned := map[string]protocol.OwnedAnswer{}
			for k, d := range quote {
				owned[k] = protocol.OwnedAnswer{Digest: d}
			}
			acks := []struct {
				name string
				ack  *protocol.EdgeHeartbeatAck
			}{
				{"owned map", &protocol.EdgeHeartbeatAck{Feeds: feeds, Owned: owned}},
				{"older Core", &protocol.EdgeHeartbeatAck{Feeds: feeds, Claims: reports, BinWatch: &watch}},
			}
			for _, a := range acks {
				e.OwnedDigests()
				e.OnCoreAck(a.ack) // the first quiet ack of each kind settles the guard
				sent = 0
				for i := 1; i <= 2; i++ {
					builds := ownedBuilds(e)
					counter.Reset()
					if got := e.OwnedDigests(); !reflect.DeepEqual(got, quote) {
						t.Fatalf("%s: the quiet heartbeat quotes %v, want %v", a.name, got, quote)
					}
					e.OnCoreAck(a.ack)
					if n := counter.Count(); n != 0 {
						t.Errorf("%s: quiet heartbeat and ack %d at P=%d ran %d statements, want 0; statements:\n%s",
							a.name, i, p, n, strings.Join(counter.Statements(), "\n---\n"))
					}
					if got := ownedBuilds(e); !reflect.DeepEqual(got, builds) {
						t.Errorf("%s: quiet heartbeat and ack %d at P=%d built families %v, want %v: a build reads and hashes",
							a.name, i, p, got, builds)
					}
				}
				if left := drainClaimsReports(t, db); len(left) != 0 || sent != 0 {
					t.Errorf("%s: a quiet ack sent %d plant-claims reports and %d direct messages, want none", a.name, len(left), sent)
				}
			}
		})
	}
}

// TestOwned_EveryOwnedDigestIsInABuildOrASend is the census the quiet-ack pin
// leans on: in the Edge's non-test source, every call of an Edge-owned
// family's digest functions sits in a function named here, each reached only
// from a family's build or send, a save hook, or the decision rule
// (actableCopy), never from the heartbeat's quote or an ack's compare. A new
// call site fails here until it is placed. withDigest (messaging) is the
// claims report's: every caller reads the spec first, so a hash there is
// never without a statement the counting driver sees.
func TestOwned_EveryOwnedDigestIsInABuildOrASend(t *testing.T) {
	t.Parallel()
	digests := map[string]bool{"ClaimsDigest": true, "BinWatchDigest": true, "CellCatalogDigest": true, "OwnedDigest": true}
	want := map[string]bool{
		"messaging.withDigest":     true, // the claims report's, after its reads
		"engine.binWatchList":      true, // the watch family's build, and actableCopy
		"engine.buildCellsFamily":  true,
		"engine.buildOwned":        true,
		"engine.updateOwnedMember": true, // a send or an edit replacing one member
		"engine.ClaimsReportedAll": true, // the publisher's full read
	}
	got := map[string]bool{}
	callersOfBinWatchList := map[string]bool{}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) && path != ".." {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := f.Name.Name + "." + fn.Name.Name // a method by its name alone
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "protocol" && digests[sel.Sel.Name] {
					got[name] = true
				}
				if sel.Sel.Name == "binWatchList" {
					callersOfBinWatchList[fn.Name.Name] = true
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk shingo-edge: %v", err)
	}
	for site := range got {
		if !want[site] {
			t.Errorf("%s computes an Edge-owned digest and is not in the census: place it (a build, a send or a hook) and name it here", site)
		}
	}
	for site := range want {
		if !got[site] {
			t.Errorf("the census names %s, which computes no Edge-owned digest any more: take it out", site)
		}
	}
	if want := map[string]bool{"buildBinWatchFamily": true, "actableCopy": true}; !reflect.DeepEqual(callersOfBinWatchList, want) {
		t.Errorf("binWatchList is called from %v, want %v: the list's digest is the watch family's build's, and the decision rule's",
			callersOfBinWatchList, want)
	}
}

// TestOwned_TheFirstAckAfterBootReconciles: the boot build (Start's) reads the
// spec, and the first ack from a Core holding none, in the owned map or in an
// older Core's Claims, sends every styled process.
func TestOwned_TheFirstAckAfterBootReconciles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ack  *protocol.EdgeHeartbeatAck
	}{
		{"owned map", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Owned: map[string]protocol.OwnedAnswer{
			protocol.OwnedClaims: {Digest: "", Members: nil}}}},
		{"older Core", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Claims: map[string]string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			e, _ := claimsEngine(t, db)
			seedStyledProcess(t, db, "BOOT-A", 1)
			seedStyledProcess(t, db, "BOOT-B", 2)
			e.buildAllOwned() // as Start does
			e.OwnedDigests()
			e.OnCoreAck(tc.ack)
			if got, want := claimsSent(t, db), []string{"BOOT-A:1", "BOOT-B:2"}; !reflect.DeepEqual(got, want) {
				t.Errorf("first ack after boot published %v, want %v", got, want)
			}
		})
	}
}

// TestOwned_ASpecEditMovesTheQuote: an edit published through the publisher
// (PublishChanged, the spec-change hook) moves the claims family's member and
// the heartbeat's quote at once, with no read beyond the publish's own; a
// write no hook reaches moves it at the floor and not before.
func TestOwned_ASpecEditMovesTheQuote(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e, pub := claimsEngine(t, db)
	now := time.Unix(1_800_000_000, 0)
	e.feeds.now = func() time.Time { return now }
	pid := seedStyledProcess(t, db, "EDIT", 1)
	before := e.OwnedDigests()[protocol.OwnedClaims]

	sid, err := db.CreateStyle("EDIT-S9", "", pid)
	testutil.MustNoErr(t, err, "create style")
	seedProduceClaim(t, db, sid, "QA-N9", "SYN-EDIT-9")
	if got := e.OwnedDigests()[protocol.OwnedClaims]; got != before {
		t.Fatalf("a write no hook reached moved the quote before the floor (%s → %s)", before, got)
	}
	testutil.MustNoErr(t, pub.PublishChanged(pid), "publish the edit")
	edited := e.OwnedDigests()[protocol.OwnedClaims]
	if edited == before {
		t.Fatalf("the edit's publish left the quote at %s", before)
	}
	want := ownedDigest(t, map[string]string{"EDIT": drainClaimsReports(t, db)["EDIT"]})
	if edited != want {
		t.Errorf("quote after the edit = %s, want the family of the report sent, %s", edited, want)
	}

	sid, err = db.CreateStyle("EDIT-S10", "", pid)
	testutil.MustNoErr(t, err, "create style")
	seedProduceClaim(t, db, sid, "QA-N10", "SYN-EDIT-10")
	if got := e.OwnedDigests()[protocol.OwnedClaims]; got != edited {
		t.Fatalf("a write no hook reached moved the quote inside the floor")
	}
	now = now.Add(ownedFloor)
	if got := e.OwnedDigests()[protocol.OwnedClaims]; got == edited {
		t.Errorf("the floor left the quote at %s after a write no hook reached", got)
	}
}

// TestOwned_AQuietAckSettlesTheClaimsFlags: a process flagged as not
// converging is settled, its flag cleared, by an ack that quotes the claims
// family back at this Edge's digest (ClaimsSettled's rule on a quiet ack).
func TestOwned_AQuietAckSettlesTheClaimsFlags(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e, _ := claimsEngine(t, db)
	seedStyledProcess(t, db, "FLAG", 1)
	for _, d := range []string{"d1", "d2", "d3"} {
		e.sendDue(claimsGuardKey("FLAG"), d, "")
	}
	if len(e.FeedFlags()) != 1 {
		t.Fatalf("fixture: flags %v, want one", e.FeedFlags())
	}
	quote := e.OwnedDigests()
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Owned: map[string]protocol.OwnedAnswer{
		protocol.OwnedClaims: {Digest: quote[protocol.OwnedClaims]}}})
	if got := e.FeedFlags(); len(got) != 0 {
		t.Errorf("flags after a quiet ack = %v, want none", got)
	}
}

// TestAnswerOwned_ClaimsRepublishesWhatDiffers: on an answer whose claims
// differ, a styled process Core holds at the same digest is left alone; one
// at a different digest, or missing, is re-published; a name Core holds that
// this Edge has with no styles, or does not have at all, gets an empty report.
// The same from an older Core's Claims map.
func TestAnswerOwned_ClaimsRepublishesWhatDiffers(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned map", true: "older Core"}[legacy], func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			e, _ := claimsEngine(t, db)
			seedStyledProcess(t, db, "SAME", 2)
			seedStyledProcess(t, db, "STALE", 1)
			seedStyledProcess(t, db, "MISSING", 1)
			_, err := db.CreateProcess("BARE", "", "", "", false)
			testutil.MustNoErr(t, err, "create bare process")
			quote := e.OwnedDigests()
			e.feeds.mu.Lock()
			same := e.feeds.ownedLocked(protocol.OwnedClaims).members["SAME"]
			e.feeds.mu.Unlock()
			held := map[string]string{"SAME": same, "STALE": "0000000000000000", "BARE": "1111111111111111", "GONE": "2222222222222222"}
			ack := &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Owned: map[string]protocol.OwnedAnswer{
				protocol.OwnedClaims: {Digest: ownedDigest(t, held), Members: held}}}
			if legacy {
				ack = &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Claims: held}
			}
			if ack.Owned != nil && ack.Owned[protocol.OwnedClaims].Digest == quote[protocol.OwnedClaims] {
				t.Fatal("fixture: Core's claims equal the quote")
			}
			e.OnCoreAck(ack)
			if got, want := claimsSent(t, db), []string{"BARE:0", "GONE:0", "MISSING:1", "STALE:1"}; !reflect.DeepEqual(got, want) {
				t.Errorf("published %v, want %v", got, want)
			}
			// The guard holds a repeat: the same answer again sends nothing.
			e.OnCoreAck(ack)
			if got := claimsSent(t, db); len(got) != 0 {
				t.Errorf("the same answer again published %v, want each held back in flight", got)
			}
		})
	}
}

// TestAnswerOwned_AFailedReadPublishesNothing: a process list that cannot be
// read publishes nothing — not even the empty reports, since an unread spec
// is not evidence a process is gone — whether the family was never read (it
// is left off the heartbeat) or was, and its sends read again.
func TestAnswerOwned_AFailedReadPublishesNothing(t *testing.T) {
	t.Parallel()
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "never read", true: "read before"}[warm], func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			e, _ := claimsEngine(t, db)
			seedStyledProcess(t, db, "STALE", 1)
			if warm {
				e.OwnedDigests()
			}
			_, err := db.DB.Exec(`ALTER TABLE processes RENAME TO processes_hidden`)
			testutil.MustNoErr(t, err, "hide processes")
			quote := e.OwnedDigests()
			if _, ok := quote[protocol.OwnedClaims]; ok == !warm {
				t.Errorf("quote %v: the claims family should be quoted only once it has been read", quote)
			}
			held := map[string]string{"STALE": "0", "GONE": "1"}
			e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Claims: held})
			e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Owned: map[string]protocol.OwnedAnswer{
				protocol.OwnedClaims: differs(held)}})
			if got := claimsSent(t, db); len(got) != 0 {
				t.Errorf("published %v, want nothing", got)
			}
			if got := e.FeedFlags(); len(got) != 0 {
				t.Errorf("flags %v, want none", got)
			}
		})
	}
}

// TestAnswerOwned_TheAbsenceRules (protocol EdgeHeartbeatAck.Owned): a family
// the answer leaves off sends nothing and settles nothing, even beside an
// older Core's Claims (the owned map, when present, is the only one read); a
// family answered with no members is Core holding none, and every member is
// sent; a member Core holds at "" is never equal, so it is sent.
func TestAnswerOwned_TheAbsenceRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		answer map[string]protocol.OwnedAnswer
		want   []string
	}{
		{"family left off", map[string]protocol.OwnedAnswer{}, nil},
		{"no members", map[string]protocol.OwnedAnswer{protocol.OwnedClaims: {Digest: "other"}}, []string{"ABS:1"}},
		{"a member at \"\"", map[string]protocol.OwnedAnswer{protocol.OwnedClaims: differs(map[string]string{"ABS": ""})}, []string{"ABS:1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			e, _ := claimsEngine(t, db)
			seedStyledProcess(t, db, "ABS", 1)
			e.sendDue(claimsGuardKey("ABS"), "older", "")
			e.OwnedDigests()
			e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Owned: tc.answer, Claims: map[string]string{}})
			if got := claimsSent(t, db); !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Errorf("published %v, want %v", got, tc.want)
			}
		})
	}
}

// cellsSent decodes every unsent edge.cells catalog and acks it.
func cellsSent(t *testing.T, db *store.DB) []protocol.CellCatalog {
	t.Helper()
	msgs, err := db.ListUnsentOutboxByType([]string{protocol.SubjectEdgeCells})
	testutil.MustNoErr(t, err, "list edge.cells outbox")
	var out []protocol.CellCatalog
	for _, m := range msgs {
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal envelope")
		var d protocol.Data
		testutil.MustNoErr(t, env.DecodePayload(&d), "decode data")
		var c protocol.CellCatalog
		testutil.MustNoErr(t, json.Unmarshal(d.Body, &c), "decode catalog")
		if string(d.Body) != "" && !strings.Contains(string(d.Body), `"cells":[`) {
			t.Errorf("edge.cells body %s: cells must be a list, never null", d.Body)
		}
		out = append(out, c)
		testutil.MustNoErr(t, db.AckOutbox(m.ID), "ack outbox row")
	}
	return out
}

// TestCells_TheOwnedFamily: the cell catalog goes on edge.cells, through the
// outbox, to a Core that answers the cells family: when the answer differs,
// and at once when a save moves it; never when the answer is this Edge's;
// empty when this Edge's catalog has emptied; never to an older Core, and
// never as a register.
func TestCells_TheOwnedFamily(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	cells := []protocol.CellCatalogEntry{{CellLabel: "PLC-1", Processes: []protocol.CellProcessBinding{{ProcessID: 1, StyleID: 1, PLCName: "PLC-1", TagName: "T1"}}}}
	e.SetCellCatalogFunc(func() ([]protocol.CellCatalogEntry, error) { return cells, nil })
	digest, err := protocol.CellCatalogDigest(cells)
	testutil.MustNoErr(t, err, "digest")
	ack := func(a protocol.OwnedAnswer) {
		e.OwnedDigests()
		e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Owned: map[string]protocol.OwnedAnswer{protocol.OwnedCells: a}})
	}

	// An older Core: nothing, on an ack or on a save.
	e.OwnedDigests()
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	e.CellsChanged()
	if got := cellsSent(t, db); len(got) != 0 {
		t.Fatalf("an older Core was sent %d catalogs", len(got))
	}

	ack(differs(nil)) // Core holds none
	if got := cellsSent(t, db); len(got) != 1 || got[0].Digest != digest || !reflect.DeepEqual(got[0].Cells, cells) {
		t.Fatalf("a Core holding none was sent %+v, want the catalog at %s", got, digest)
	}
	ack(protocol.OwnedAnswer{Digest: ownedDigest(t, map[string]string{protocol.SingletonMember: digest})})
	if got := cellsSent(t, db); len(got) != 0 {
		t.Errorf("a Core holding the catalog was sent %d more", len(got))
	}

	// The catalog empties (its last counter was taken off): the save sends it
	// empty at once, and Core holding none then agrees with no send.
	cells = nil
	e.CellsChanged()
	got := cellsSent(t, db)
	if len(got) != 1 || len(got[0].Cells) != 0 {
		t.Fatalf("an emptied catalog sent %+v, want one empty catalog", got)
	}
	ack(protocol.OwnedAnswer{Digest: ownedDigest(t, nil)})
	if got := cellsSent(t, db); len(got) != 0 {
		t.Errorf("an empty catalog against a Core holding none sent %d", len(got))
	}
	if msgs, err := db.ListUnsentOutboxByType([]string{protocol.SubjectEdgeRegister}); err != nil || len(msgs) != 0 {
		t.Errorf("the cells family registered (%d rows, %v): a register re-runs the claims publish and the loader sweeps", len(msgs), err)
	}
}

// TestCells_ACounterSaveSendsTheCatalog: EditProcessCounter, the process
// counter and reporting-point save, re-reads the catalog and sends it at once
// to a Core serving the cells family.
func TestCells_ACounterSaveSendsTheCatalog(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, _ := counterRig(t)
	eng.SetCellCatalogFunc(func() ([]protocol.CellCatalogEntry, error) {
		pts, err := db.ListCountedReportingPoints()
		if err != nil {
			return nil, err
		}
		return messaging.BuildCellCatalog(pts), nil
	})
	quote := eng.OwnedDigests()
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Owned: map[string]protocol.OwnedAnswer{
		protocol.OwnedCells: {Digest: quote[protocol.OwnedCells]}}})
	if got := cellsSent(t, db); len(got) != 0 {
		t.Fatalf("fixture: %d catalogs sent before the save", len(got))
	}
	testutil.MustNoErr(t, eng.EditProcessCounter(pid, domain.ProcessCounterEdit{
		PLCName: "plc", TagName: "MES_Other.Prod_Counter_01", Enabled: true}), "re-point")
	got := cellsSent(t, db)
	if len(got) != 1 || len(got[0].Cells) != 1 || len(got[0].Cells[0].Processes) != 1 ||
		got[0].Cells[0].Processes[0].TagName != "MES_Other.Prod_Counter_01" {
		t.Errorf("after the counter save edge.cells sent %+v, want the re-pointed catalog once", got)
	}
}

// TestOwned_ASavesPublishQuotedBackConverges: the report a save publishes
// (PublishChanged, ClaimsReported) is a copy this Edge sent, so a Core quoting
// it while the next save's is on its way is converging; a Core that took none
// still flags on the third send.
func TestOwned_ASavesPublishQuotedBackConverges(t *testing.T) {
	t.Parallel()
	for _, frozen := range []bool{false, true} {
		t.Run(map[bool]string{false: "Core takes each", true: "Core takes none"}[frozen], func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			e, pub := claimsEngine(t, db)
			pid := seedStyledProcess(t, db, "SAVES", 1)
			n := 0
			save := func() string {
				n++
				sid, err := db.CreateStyle(fmt.Sprintf("SAVES-X%d", n), "", pid)
				testutil.MustNoErr(t, err, "create style")
				seedProduceClaim(t, db, sid, "QA-N0", fmt.Sprintf("SYN-SAVES-%d", n))
				testutil.MustNoErr(t, pub.PublishChanged(pid), "publish the save")
				return drainClaimsReports(t, db)["SAVES"]
			}
			coreHolds := save()
			for i := 0; i < 4; i++ {
				took := save()
				save() // published, not yet at Core when it answers
				if !frozen {
					coreHolds = took
				}
				e.OwnedDigests()
				e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Owned: map[string]protocol.OwnedAnswer{
					protocol.OwnedClaims: differs(map[string]string{"SAVES": coreHolds})}})
				drainClaimsReports(t, db)
			}
			if got := e.FeedFlags(); (len(got) > 0) != frozen {
				t.Errorf("after four acks: flags %v, want flagged %v", got, frozen)
			}
		})
	}
}
