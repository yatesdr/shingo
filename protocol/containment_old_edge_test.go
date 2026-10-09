package protocol_test

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/router"
)

// An old Edge on a new Core: Core broadcasts containment.snapshot after every
// known containment write, and an Edge built before the subject has no handler
// for it. The Edge's dispatch is the one its composition root builds (the
// TypeData closure handing the Data to the subject router), here with every
// inbound subject registered except the new one. The cost is one "no handler"
// log line per broadcast and nothing else: no panic, no other handler runs.
// Counted, as the brief asks, rather than treated as a bug.
func TestContainmentSnapshot_OldEdgeLogsOncePerBroadcast(t *testing.T) {
	sub := router.NewSubject()
	ran := 0
	for _, s := range protocol.EdgeInboundSubjects() {
		if s == protocol.SubjectContainmentSnapshot {
			continue
		}
		router.RegisterSubjectBare(sub, s, func(*protocol.Envelope) { ran++ })
	}
	proto := router.New[string]()
	router.Register(proto, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		sub.Dispatch(env, p)
	})

	var buf bytes.Buffer
	orig, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(orig); log.SetFlags(flags) })

	core := protocol.Address{Role: protocol.RoleCore, Station: "core"}
	all := protocol.Address{Role: protocol.RoleEdge, Station: protocol.StationBroadcast}
	const broadcasts = 2
	for i := 0; i < broadcasts; i++ {
		env, err := protocol.NewDataEnvelope(protocol.SubjectContainmentSnapshot, core, all,
			&protocol.ContainmentSnapshot{Digest: "d1", Flags: []protocol.PayloadContainmentRow{},
				HeldBins: []protocol.HeldBinRow{}, Destinations: []protocol.ContainmentDestination{}})
		if err != nil {
			t.Fatalf("build envelope: %v", err)
		}
		proto.Dispatch(env, env.Type)
	}

	got := strings.Count(buf.String(), "no handler registered for subject "+protocol.SubjectContainmentSnapshot)
	if got != broadcasts {
		t.Errorf("%d \"no handler\" lines for %d broadcasts, want one each; log:\n%s", got, broadcasts, buf.String())
	}
	if ran != 0 {
		t.Errorf("%d other handlers ran for a containment snapshot", ran)
	}
}
