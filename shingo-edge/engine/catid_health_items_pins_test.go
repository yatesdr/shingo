package engine

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
)

var updateCatidHealthItemsGolden = flag.Bool("update-catid-health-items-golden", false,
	"rewrite testdata/catid-health-items/*.txt from CatidHealth as it computes today")

// TestCatidHealthItemsPins characterises the health items that read the
// CATID monitor's live state, which www's TestCatidHealthBodiesPins cannot
// reach (its engine is not started): the wrong-part item (no strokes yet;
// counted off-plan with the changeover offered; the part taken as the
// unpinned style's, decision 56), the changeover waiting on the PLC's new
// part, the assumed item, identity unreadable, and the MES slots' idle and
// lockstep items. Each case is one rig's process: its summary row
// (CatidHealthSummary) and its items encoded as apiCatidHealth encodes them
// ({"items": [...]}), so a body diff here is a body diff on the wire. Brief
// S4 item 6 (no change to what any check computes) is checked against these
// and www/testdata/catid-health/.
//
// The rigs are the tests' own (healthRig, switchRig, primedRig) and their
// times are now-relative, so every timestamp reads <time> and a hold's age
// <age>. Regenerate with
//
//	go test ./engine -run TestCatidHealthItemsPins -update-catid-health-items-golden
//
// and read the diff.
func TestCatidHealthItemsPins(t *testing.T) {
	t.Parallel()
	got := map[string]string{}
	pin := func(name, what string, eng *Engine, pid int64) {
		t.Helper()
		got[name] = itemsGolden(t, what, eng, pid)
	}

	{
		eng, _, pid, _, _, cm := healthRig(t, "1001")
		setLive(cm, pid, "1002")
		freshRead(cm, pid)
		pin("wrong-part-no-strokes.txt", "the PLC on STYLE-B's 1002 while PROD-STYLE (1001) runs, nothing counted on it", eng, pid)
	}
	{
		eng, _, pid, a, b, cm := switchRig(t, "prompt")
		mirrorNode(t, eng, pid, a, b, "N-MIRROR", "PART-A1", "PART-B1", 9741)
		changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
		countedStroke(eng, pid, a)
		countedStroke(eng, pid, a)
		freshRead(cm, pid)
		pin("wrong-part-offered.txt", "two strokes counted off-plan on 1002 (STYLE-B's pin), no changeover open: the one-tap offered", eng, pid)
	}
	{
		eng, db, pid, a, b, cm := switchRig(t, "prompt")
		testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, ""), "unpin B")
		mirrorNode(t, eng, pid, a, b, "N-MIRROR", "PART-A1", "PART-B1", 9741)
		changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
		countedStroke(eng, pid, a)
		countedStroke(eng, pid, a)
		freshRead(cm, pid)
		pin("wrong-part-assumed.txt", "the same with STYLE-B unpinned: 1002 taken as its part (decision 56), amber, the pin asked for", eng, pid)
	}
	{
		eng, db, pid, _, b, cm := healthRig(t, "1002")
		testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, ""), "unpin B")
		testutil.MustNoErr(t, db.SetActiveStyle(pid, &b), "B runs")
		freshRead(cm, pid)
		pin("assumed-catid.txt", "STYLE-B (unpinned) runs and the PLC is on 1002: taken as its part, the pin asked for", eng, pid)
	}
	{
		eng, _, pid, a, b, cm := healthRig(t, "1001")
		_, err := eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil, nil, nil)
		testutil.MustNoErr(t, err, "open a changeover to B")
		setLive(cm, pid, "1002")
		freshRead(cm, pid)
		pin("changeover-waiting.txt", "a changeover to STYLE-B open and the PLC already on its 1002", eng, pid)
	}
	{
		eng, _, pid, _, _, cm := healthRig(t, "1001")
		cm.snapshots.Store(pid, &catidSnapshot{value: "1001", lastOK: time.Now().Add(-time.Minute)})
		pin("identity-unreadable.txt", "the identity's last good read a minute old", eng, pid)
	}
	{
		eng, _, pid, cm, _ := primedRig(t)
		t0 := time.Now()
		setSlots(eng, [mesSlots]int64{1001, 1002, -1, -1, -1}, [mesSlots]int64{50, 10, 0, 0, 0})
		cm.watchSlots(t0)
		setSlots(eng, [mesSlots]int64{1001, 1002, -1, -1, -1}, [mesSlots]int64{50, 12, 0, 0, 0})
		cm.watchSlots(t0.Add(time.Minute))
		pin("slot-idle.txt", "the counted MES slot flat while slot 2 (CATID 1002) rises", eng, pid)
	}
	{
		eng, _, pid, cm, _ := primedRig(t)
		t0 := time.Now()
		setSlots(eng, [mesSlots]int64{1001, 1002, -1, -1, -1}, [mesSlots]int64{400, 400, 0, 0, 0})
		cm.watchSlots(t0)
		setSlots(eng, [mesSlots]int64{1001, 1002, -1, -1, -1}, [mesSlots]int64{403, 403, 0, 0, 0})
		cm.watchSlots(t0.Add(time.Minute))
		pin("slot-lockstep.txt", "slot 2 (CATID 1002) rising with the counted slot", eng, pid)
	}

	checkItemsGoldenDir(t, filepath.Join("testdata", "catid-health-items"), got)
}

var (
	itemsPinTime = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})?`)
	itemsPinAge  = regexp.MustCompile(`strokes over (?:\d+h)?(?:\d+m)?\d+s \(`)
)

// itemsGolden is one rig's process as the two routes would serve it.
func itemsGolden(t *testing.T, what string, eng *Engine, pid int64) string {
	t.Helper()
	items, err := eng.CatidHealth(pid)
	testutil.MustNoErr(t, err, "health")
	if items == nil {
		items = []CatidHealthItem{}
	}
	rows, err := eng.CatidHealthSummary()
	testutil.MustNoErr(t, err, "summary")
	row := "(none)"
	for _, r := range rows {
		if r.ProcessID == pid {
			b, err := json.Marshal(r)
			testutil.MustNoErr(t, err, "summary row")
			row = string(b)
		}
	}
	var body bytes.Buffer
	testutil.MustNoErr(t, json.NewEncoder(&body).Encode(map[string]any{"items": items}), "encode")
	var ind bytes.Buffer
	testutil.MustNoErr(t, json.Indent(&ind, body.Bytes(), "", "  "), "indent")
	text := "# " + what + "\nsummary row: " + row + "\nitems body:\n" + strings.TrimRight(ind.String(), "\n") + "\n"
	text = itemsPinTime.ReplaceAllString(text, "<time>")
	return itemsPinAge.ReplaceAllString(text, "strokes over <age> (")
}

// checkItemsGoldenDir compares got with dir, or rewrites it under the flag. It
// owns dir: a file got does not name is reported, or removed under the flag.
func checkItemsGoldenDir(t *testing.T, dir string, got map[string]string) {
	t.Helper()
	update := *updateCatidHealthItemsGolden
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if update {
		testutil.MustNoErr(t, os.MkdirAll(dir, 0o755), "mkdir "+dir)
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		if update {
			testutil.MustNoErr(t, os.WriteFile(path, []byte(got[n]), 0o644), "write "+path)
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden (%v); regenerate with -update-catid-health-items-golden and read it", n, err)
			continue
		}
		if string(want) != got[n] {
			t.Errorf("%s moved; regenerate only for a named change\n--- golden\n%s\n--- now\n%s", path, want, got[n])
		}
	}
	entries, err := os.ReadDir(dir)
	testutil.MustNoErr(t, err, "read "+dir)
	for _, e := range entries {
		if _, ok := got[e.Name()]; ok || e.IsDir() {
			continue
		}
		if update {
			testutil.MustNoErr(t, os.Remove(filepath.Join(dir, e.Name())), "remove stale "+e.Name())
			continue
		}
		t.Errorf("%s: a golden no pin writes; delete it, or restore the pin that wrote it", filepath.Join(dir, e.Name()))
	}
}

// freshRead dates the process's last good identity read now, as the poll
// would: the rigs read it at their start, and on a loaded box more than the
// attribution's trust (tickIdentityFresh) could pass before the strip is
// read, adding an "unreadable" item to a golden that is not about it.
func freshRead(cm *catidMonitor, pid int64) {
	if s, ok := cm.snapshot(pid); ok {
		c := *s
		c.lastOK = time.Now()
		cm.snapshots.Store(pid, &c)
	}
}
