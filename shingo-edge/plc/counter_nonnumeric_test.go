package plc

import (
	"strings"
	"sync"
	"testing"
)

// readErrRecorder records the read errors the poll raises.
type readErrRecorder struct {
	deltaRecorder
	emu  sync.Mutex
	errs []string
}

func (r *readErrRecorder) EmitCounterReadError(_ int64, _, _, msg string) {
	r.emu.Lock()
	r.errs = append(r.errs, msg)
	r.emu.Unlock()
}

func (r *readErrRecorder) readErrors() []string {
	r.emu.Lock()
	defer r.emu.Unlock()
	return append([]string(nil), r.errs...)
}

// TestPoll_NonNumericReadIsRaisedNotCounted: a counter read that is not a
// number counts nothing and is raised as a read error, naming the value;
// a numeric read after it counts again. Before, it returned in silence.
func TestPoll_NonNumericReadIsRaisedNotCounted(t *testing.T) {
	t.Parallel()
	em := &readErrRecorder{}
	r := newUnreadTickRig(t, em, 0)
	r.pass(100)

	mp := r.mgr.plcs[rigPLC]
	mp.mu.Lock()
	mp.Values[rigTag] = TagValue{Name: rigTag, TypeStr: "STRING", Value: "n/a"}
	mp.mu.Unlock()
	r.mgr.pollAllReportingPoints()
	r.mgr.pollAllReportingPoints()

	errs := em.readErrors()
	if len(errs) != 2 || !strings.Contains(errs[0], "not a number") || !strings.Contains(errs[0], "n/a") {
		t.Fatalf("read errors = %q, want two naming the value", errs)
	}
	if got := em.emitted(); len(got) != 0 {
		t.Errorf("emitted deltas = %+v, want none", got)
	}
	r.pass(103)
	if got := em.emitted(); len(got) != 1 || got[0].delta != 3 {
		t.Errorf("after a numeric read: %+v, want one delta of 3", got)
	}
	if n := len(em.readErrors()); n != 2 {
		t.Errorf("read errors after a numeric read = %d, want still 2", n)
	}
}
