//go:build docker

package messaging

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store/nodes"
)

// node_claimed_twice_docker_test.go — what Core says of a node two stations
// name (owner ruling 2026-11-22): to a person on the Edges page (the flags),
// and to the other Edge (the node list).

const (
	ctA, ctB         = "edge.ct-a", "edge.ct-b"
	ctAName, ctBName = "Line A", "Line B"
)

// claimedTwiceCore is Core with two named stations, two physical nodes, a
// group and the one member of the group.
func claimedTwiceCore(t *testing.T) (*CoreDataService, *feedsPinResponder) {
	t.Helper()
	db := testdb.Open(t)
	for uid, name := range map[string]string{ctA: ctAName, ctB: ctBName} {
		_, err := db.EnrollEdge(uid, name)
		testutil.MustNoErr(t, err, "enroll "+uid)
	}
	ngrp, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "the group type")
	node := func(n *nodes.Node) *nodes.Node {
		n.Enabled = true
		testutil.MustNoErr(t, db.CreateNode(n), "create "+n.Name)
		return n
	}
	node(&nodes.Node{Name: "CT-N1"})
	node(&nodes.Node{Name: "CT-N2"})
	grp := node(&nodes.Node{Name: "CT-GRP", IsSynthetic: true, NodeTypeID: &ngrp.ID})
	node(&nodes.Node{Name: "CT-M1", ParentID: &grp.ID})
	resp := &feedsPinResponder{}
	return NewCoreDataService(db, resp, service.EpochAnnounce{}), resp
}

// reportNames sends the report of one process from station: claims are the
// nodes its one style claims, paired the node its first claim is paired with,
// positions the seats no claim names.
func reportNames(svc *CoreDataService, station, process string, claims []string, paired string, positions ...string) {
	st := styleSpec{name: "A", active: true}
	for i, n := range claims {
		c := claimSpec{node: n, payload: "PC-A", allowed: []string{"PC-A"}}
		if i == 0 {
			c.paired = paired
		}
		st.claims = append(st.claims, c)
	}
	r := digestedReport(process, station+process+paired, []styleSpec{st})
	r.Positions = positions
	svc.HandlePlantClaims(feedsPinEnv(station), r)
}

// TestEdgeFeedFlags_ANodeTwoStationsName: what the Edges page flags for a node
// two stations name. The states the ruling flags have at least one side that
// is a claim (its own position or a paired one) and are a physical node; the
// others never are. Each flagged state is one line, the same on both rows.
func TestEdgeFeedFlags_ANodeTwoStationsName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		make func(svc *CoreDataService)
		want []string
	}{
		{name: "two claims", want: []string{ctFlag("CT-N1")}, make: func(svc *CoreDataService) {
			reportNames(svc, ctA, "PA", []string{"CT-N1"}, "")
			reportNames(svc, ctB, "PB", []string{"CT-N1"}, "")
		}},
		{name: "a claim and a position", want: []string{ctFlag("CT-N1")}, make: func(svc *CoreDataService) {
			reportNames(svc, ctA, "PA", []string{"CT-N1"}, "")
			reportNames(svc, ctB, "PB", nil, "", "CT-N1")
		}},
		{name: "a paired position and a claim", want: []string{ctFlag("CT-N1")}, make: func(svc *CoreDataService) {
			reportNames(svc, ctA, "PA", []string{"CT-N2"}, "CT-N1")
			reportNames(svc, ctB, "PB", []string{"CT-N1"}, "")
		}},
		{name: "a group member, which is physical", want: []string{ctFlag("CT-M1")}, make: func(svc *CoreDataService) {
			reportNames(svc, ctA, "PA", []string{"CT-M1"}, "")
			reportNames(svc, ctB, "PB", []string{"CT-M1"}, "")
		}},
		{name: "two positions", want: []string{}, make: func(svc *CoreDataService) {
			reportNames(svc, ctA, "PA", nil, "", "CT-N1")
			reportNames(svc, ctB, "PB", nil, "", "CT-N1")
		}},
		{name: "a group", want: []string{}, make: func(svc *CoreDataService) {
			reportNames(svc, ctA, "PA", []string{"CT-GRP"}, "")
			reportNames(svc, ctB, "PB", []string{"CT-GRP"}, "")
		}},
		{name: "two processes of one Edge", want: []string{}, make: func(svc *CoreDataService) {
			reportNames(svc, ctA, "PA", []string{"CT-N1"}, "")
			reportNames(svc, ctA, "PA2", []string{"CT-N1"}, "")
		}},
		{name: "one stopped naming it", want: []string{}, make: func(svc *CoreDataService) {
			reportNames(svc, ctA, "PA", []string{"CT-N1"}, "")
			reportNames(svc, ctB, "PB", []string{"CT-N1"}, "")
			reportNames(svc, ctB, "PB", []string{"CT-N2"}, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, _ := claimedTwiceCore(t)
			tc.make(svc)
			for _, st := range []string{ctA, ctB} {
				if got := svc.EdgeFeedFlags(st); !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s flags = %q, want %q", st, got, tc.want)
				}
			}
		})
	}
}

// ctFlag is the Edges-page line for a node both stations name.
func ctFlag(node string) string {
	return "node " + node + " claimed by " + ctAName + " and " + ctBName
}

// claimedBy is the claimed_by key of each node of the list Core builds for
// station, as the wire carries it: absent is "".
func claimedBy(t *testing.T, svc *CoreDataService, station string) map[string]string {
	t.Helper()
	list, err := svc.buildNodeList(station, true)
	testutil.MustNoErr(t, err, "build the node list of "+station)
	raw, err := json.Marshal(list.Nodes)
	testutil.MustNoErr(t, err, "encode")
	var rows []struct {
		Name      string `json:"name"`
		ClaimedBy string `json:"claimed_by"`
	}
	testutil.MustNoErr(t, json.Unmarshal(raw, &rows), "decode")
	out := map[string]string{}
	for _, r := range rows {
		if r.ClaimedBy != "" {
			out[r.Name] = r.ClaimedBy
		}
	}
	return out
}

// nodesDigestOf is the digest of the list Core builds for station.
func nodesDigestOf(t *testing.T, svc *CoreDataService, station string) string {
	t.Helper()
	list, err := svc.buildNodeList(station, true)
	testutil.MustNoErr(t, err, "build the node list of "+station)
	return list.Digest
}

// TestNodeList_AClaimOfAnotherStation: what the node list of one station says
// of a node another station claims, and whose list a claim change moves: the
// other station is told the claimant's name for each physical node claimed
// (own, paired, a group's member), never of the group, and the claimant's own
// list says nothing and does not move.
func TestNodeList_AClaimOfAnotherStation(t *testing.T) {
	t.Parallel()
	svc, _ := claimedTwiceCore(t)
	a0, b0 := nodesDigestOf(t, svc, ctA), nodesDigestOf(t, svc, ctB)

	reportNames(svc, ctA, "PA", []string{"CT-N1", "CT-GRP", "CT-M1"}, "CT-N2")
	if got := claimedBy(t, svc, ctA); len(got) != 0 {
		t.Errorf("the list of A marks %v for its own claims, want nothing", got)
	}
	if got := nodesDigestOf(t, svc, ctA); got != a0 {
		t.Errorf("its own claim moved the list of A (%s to %s), want no re-send", a0, got)
	}
	want := map[string]string{"CT-N1": ctAName, "CT-N2": ctAName, "CT-GRP.CT-M1": ctAName}
	if got := claimedBy(t, svc, ctB); !reflect.DeepEqual(got, want) {
		t.Errorf("the list of B marks %v for the claims of A, want %v", got, want)
	}
	if got := nodesDigestOf(t, svc, ctB); got == b0 {
		t.Errorf("a claim of A left the list of B at %s, want a re-send", b0)
	}

	// A position alone is not a claim.
	reportNames(svc, ctA, "PA", nil, "", "CT-N1")
	if got := claimedBy(t, svc, ctB); len(got) != 0 {
		t.Errorf("the list of B marks %v for a position of A, want nothing", got)
	}
}

// TestNodeFeed_AnotherStationsClaimChange: how often a claim change re-sends
// the node list. Each station converges, A reports a claim, and both heartbeat
// again inside the life of the memo: one list to B, none to A.
func TestNodeFeed_AnotherStationsClaimChange(t *testing.T) {
	t.Parallel()
	svc, resp := claimedTwiceCore(t)
	held := map[string]map[string]string{}
	for _, st := range []string{ctA, ctB} {
		held[st] = map[string]string{protocol.FeedNodes: nodesDigestOf(t, svc, st)}
		if _, sends := heartbeatFrom(svc, resp, st, held[st]); len(sends) != 0 {
			t.Fatalf("setup: a converged heartbeat of %s sent %d messages", st, len(sends))
		}
	}
	reportNames(svc, ctA, "PA", []string{"CT-N1"}, "")
	for _, st := range []string{ctA, ctB} {
		_, sends := heartbeatFrom(svc, resp, st, held[st])
		var subjects []string
		for _, e := range sends {
			subjects = append(subjects, e.subject)
		}
		if got, want := slices.Contains(subjects, protocol.SubjectNodeListResponse), st == ctB; got != want {
			t.Errorf("a claim of A re-sent the node list to %s: %v, want %v", st, got, want)
		}
	}
}
