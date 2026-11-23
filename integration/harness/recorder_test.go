package harness

import (
	"strconv"
	"strings"
	"testing"
)

// TestNormaliser_TwoRunsOfOneCellCompareEqual: what differs between two runs
// of one cell without the cell doing anything different (stamps, uuids, row
// ids, digests) is taken out, and what the cell did is kept.
func TestNormaliser_TwoRunsOfOneCellCompareEqual(t *testing.T) {
	run := func(id1, id2, order string, row, bin int, ts, digest string) []string {
		n := newNormaliser()
		return []string{
			n.envelope([]byte(`{"v":1,"type":"order.request","id":"` + id1 + `","ts":"` + ts + `",` +
				`"exp":"0001-01-01T00:00:00Z","p":{"order_uuid":"` + order + `","quantity":1,"delivery_node":"LINE"}}`)),
			n.envelope([]byte(`{"v":1,"type":"order.ack","id":"` + id2 + `","cor":"` + id1 + `","ts":"` + ts + `",` +
				`"p":{"order_uuid":"` + order + `","shingo_order_id":` + itoa(row) + `,"bin_id":` + itoa(bin) +
				`,"digest":"` + digest + `","created_at":"` + ts + `"}}`)),
		}
	}
	a := run("0b3c1f9e-6a55-4d1b-9a8e-0f1e2d3c4b5a", "1c4d2a0f-7b66-4e2c-8b9f-1a2b3c4d5e6f",
		"2d5e3b1a-8c77-4f3d-9cab-2b3c4d5e6f70", 17, 4, "2026-11-23T10:00:00.123456Z",
		"a3f1c2e4b5d6a7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f70")
	z := run("9e8d7c6b-5a49-4382-a716-0f9e8d7c6b5a", "8d7c6b5a-4938-4271-b605-f9e8d7c6b5a4",
		"7c6b5a49-3827-4160-a5f4-e8d7c6b5a493", 23, 9, "2026-11-24T23:59:59Z",
		"0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0")
	for i := range a {
		if a[i] != z[i] {
			t.Errorf("line %d differs:\n  %s\n  %s", i+1, a[i], z[i])
		}
	}
	// The envelope id goes, the reply still names what it answers, and the zero
	// time stays.
	if strings.Contains(a[0], `"id"`) || !strings.Contains(a[1], `"cor":"<re:order.request>"`) {
		t.Errorf("the ack's cor lost its message: %s / %s", a[0], a[1])
	}
	if !strings.Contains(a[0], `"exp":"0001-01-01T00:00:00Z"`) {
		t.Errorf("the zero expiry was stamped out: %s", a[0])
	}
	// What the cell did is kept: another quantity is another line.
	n := newNormaliser()
	other := n.envelope([]byte(`{"v":1,"type":"order.request","id":"0b3c1f9e-6a55-4d1b-9a8e-0f1e2d3c4b5a",` +
		`"ts":"2026-11-23T10:00:00Z","exp":"0001-01-01T00:00:00Z","p":{"order_uuid":"2d5e3b1a-8c77-4f3d-9cab-2b3c4d5e6f70",` +
		`"quantity":2,"delivery_node":"LINE"}}`))
	if other == a[0] {
		t.Errorf("a different quantity normalised to the same line: %s", other)
	}
}

// TestNormaliser_RowIDsKeepTheirIdentity: one id under one key keeps its name
// across messages, and a new id is a new name, so a cell that refers to the
// same row twice reads so in its transcript.
func TestNormaliser_RowIDsKeepTheirIdentity(t *testing.T) {
	n := newNormaliser()
	a := n.envelope([]byte(`{"p":{"order_id":40,"bin_ids":[7,8]}}`))
	b := n.envelope([]byte(`{"p":{"order_id":41,"bin_ids":[8]}}`))
	c := n.envelope([]byte(`{"p":{"order_id":40}}`))
	if a != `{"p":{"bin_ids":["<bin_ids#1>","<bin_ids#2>"],"order_id":"<order_id#1>"}}` {
		t.Errorf("a = %s", a)
	}
	if b != `{"p":{"bin_ids":["<bin_ids#2>"],"order_id":"<order_id#2>"}}` {
		t.Errorf("b = %s", b)
	}
	if c != `{"p":{"order_id":"<order_id#1>"}}` {
		t.Errorf("c = %s", c)
	}
}

// TestNormaliser_ProseQuotesKeepTheirNames: a vendor order id and a known
// uuid's first eight characters quoted in a reason take the names the fields
// gave them; eight hex characters that quote no uuid stay as written.
func TestNormaliser_ProseQuotesKeepTheirNames(t *testing.T) {
	n := newNormaliser()
	n.envelope([]byte(`{"p":{"order_uuid":"bb27025d-1111-4222-8333-944455556666"}}`))
	got := n.envelope([]byte(`{"p":{"waybill_id":"sg-41-50ad09ea","detail":"vendor order sg-41-50ad09ea created",` +
		`"queue_reason":"Waiting for partner order bb27025d to be received","note":"deadbeef"}}`))
	want := `{"p":{"detail":"vendor order <V1> created","note":"deadbeef",` +
		`"queue_reason":"Waiting for partner order <U1> to be received","waybill_id":"<V1>"}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestRecorder_OffIsNil: every method of an off recorder (nil) and of its
// waits returns at once, so the scenarios call them unguarded.
func TestRecorder_OffIsNil(t *testing.T) {
	var r *Recorder
	r.Inc("x")
	r.Add("x", 2)
	r.Note("k", "v")
	r.CoreStarted()
	r.RobotPoll()
	r.Sent("E>C", []byte(`{}`))
	w := r.Wait("eventually", "what")
	w.Missed()
	w.Done(1, true)
	if r.On() || w != nil {
		t.Fatal("an off recorder is on")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
