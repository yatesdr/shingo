package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestReleasePointsAsker_TheReadAsk round-trips the release points read over
// the ask pair (protocol.AskReleasePoints): the ask names the station and the
// orders, the answer decodes into the protocol type, and a refusal or an
// answer still pending past the wait is an error, never points.
func TestReleasePointsAsker_TheReadAsk(t *testing.T) {
	t.Parallel()
	e, _ := binsEngine(t, testEngineDB(t))
	e.setAskKinds([]string{protocol.AskReleasePoints})
	r := releasePointsAsker{e}
	ask := func(answer func(env *protocol.Envelope, req protocol.ReleasePointsRequest)) ([]protocol.ReleasePoint, error) {
		type out struct {
			pts []protocol.ReleasePoint
			err error
		}
		done := make(chan out, 1)
		go func() {
			pts, err := r.ReleasePoints("line-1", []string{"u-r2"})
			done <- out{pts, err}
		}()
		if answer != nil {
			var env *protocol.Envelope
			var a *protocol.EdgeAsk
			for deadline := time.Now().Add(3 * time.Second); env == nil; {
				msgs, lerr := e.db.ListPendingOutbox(10)
				if lerr != nil {
					t.Fatalf("list outbox: %v", lerr)
				}
				if len(msgs) > 0 {
					env, a = askSent(t, e)
					_ = e.db.AckOutbox(msgs[len(msgs)-1].ID)
				} else if time.Now().After(deadline) {
					t.Fatal("the ask never reached the outbox")
				}
			}
			var req protocol.ReleasePointsRequest
			testutil.MustNoErr(t, json.Unmarshal(a.Body, &req), "decode the ask")
			if a.Kind != protocol.AskReleasePoints || len(req.OrderUUIDs) != 1 || req.OrderUUIDs[0] != "u-r2" {
				t.Errorf("ask = %s %+v", a.Kind, req)
			}
			answer(env, req)
		}
		o := <-done
		return o.pts, o.err
	}

	pts, err := ask(func(env *protocol.Envelope, req protocol.ReleasePointsRequest) {
		body, err := json.Marshal(protocol.ReleasePointsResponse{Points: []protocol.ReleasePoint{{
			OrderUUID: req.OrderUUIDs[0], Found: true, Enters: []string{"SYN-PRESS"},
			AwaitsLift: []protocol.LiftDependency{{LifterUUID: "u-r1", Node: "SYN-PRESS", CoRelease: true}},
		}}})
		if err != nil {
			t.Fatalf("marshal release points: %v", err)
		}
		e.HandleEdgeAnswer(answerTo(env), &protocol.EdgeAnswer{Kind: protocol.AskReleasePoints, Status: protocol.AnswerOK, Body: body})
	})
	testutil.MustNoErr(t, err, "release points")
	if len(pts) != 1 || !pts[0].Found || pts[0].AwaitsLift[0].LifterUUID != "u-r1" || !pts[0].AwaitsLift[0].CoRelease {
		t.Errorf("points = %+v", pts)
	}
	if _, err := ask(func(env *protocol.Envelope, _ protocol.ReleasePointsRequest) {
		e.HandleEdgeAnswer(answerTo(env), &protocol.EdgeAnswer{Kind: protocol.AskReleasePoints, Status: protocol.AnswerBad, Sentence: "no"})
	}); err == nil {
		t.Error("a refusal from Core decoded as an answer")
	}
	if _, err := ask(nil); !errors.Is(err, ErrAskPending) {
		t.Errorf("no answer within the wait: err = %v, want ErrAskPending", err)
	}
}
