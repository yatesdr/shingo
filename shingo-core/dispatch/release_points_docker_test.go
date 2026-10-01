//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// release_points_docker_test.go — releasePoints (S3): per order, the nodes its
// pending segment enters, the lifts it waits on (SHAPE §3.4, with co_release),
// and the lineside bin it departs with.

// rpNode creates a plain node and, when full, a bin on it.
func rpNode(t *testing.T, r releaseRig, payload, name string, full bool) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(n), "create "+name)
	if full {
		testdb.CreateBinAtNode(t, r.db, payload, n.ID, "BIN-"+name)
	}
	return n
}

// rpPoint renders one order's release point.
func rpPoint(p protocol.ReleasePoint) string {
	var deps []string
	for _, d := range p.AwaitsLift {
		deps = append(deps, fmt.Sprintf("%s@%s co=%v", d.LifterUUID, d.Node, d.CoRelease))
	}
	bin := "-"
	if p.LinesideBin != nil {
		bin = fmt.Sprintf("%s/%v", p.LinesideBin.NodeName, p.LinesideBin.Occupied)
	}
	return fmt.Sprintf("found=%v enters=[%s] awaits=[%s] lineside=%s",
		p.Found, strings.Join(p.Enters, ","), strings.Join(deps, "; "), bin)
}

func TestReleasePoints(t *testing.T) {
	t.Parallel()
	// press-index unflipped: R1 lifts the press and drops on the index node B;
	// R2 lifts B and drops on the press. Both open on a station wait.
	r1Steps := func() []resolvedStep {
		return []resolvedStep{rlWait("RP-PRESS", WaitKindStation), vsPick("RP-PRESS"), vsDrop("RP-B"), vsDrop("RP-OUT")}
	}
	r2Steps := func() []resolvedStep {
		return []resolvedStep{rlWait("RP-B", WaitKindStation), vsPick("RP-B"), vsDrop("RP-PRESS")}
	}
	for _, c := range []struct {
		name               string
		r1Status, r2Status protocol.Status
		r1, r2             []resolvedStep
		pressFull, bFull   bool
		want               string // R1's point; R2's point
	}{
		{"press-index unflipped, both staged: both co-release", StatusStaged, StatusStaged, r1Steps(), r2Steps(), true, true,
			"found=true enters=[RP-PRESS,RP-B,RP-OUT] awaits=[rp-r2@RP-B co=true] lineside=RP-PRESS/true; " +
				"found=true enters=[RP-B,RP-PRESS] awaits=[rp-r1@RP-PRESS co=true] lineside=-"},
		// N-b's shape: R2 staged, R1 still driving to its wait. R2's lifter is
		// not parked, so no co-release.
		{"press-index unflipped, R1 still driving: R2 waits on R1's lift", StatusInTransit, StatusStaged, r1Steps(), r2Steps(), true, true,
			"found=true enters=[RP-PRESS,RP-B,RP-OUT] awaits=[rp-r2@RP-B co=true] lineside=RP-PRESS/true; " +
				"found=true enters=[RP-B,RP-PRESS] awaits=[rp-r1@RP-PRESS co=false] lineside=-"},
		// The press already empty: (a), no dependency.
		{"the drop node holds no bin: no dependency", StatusStaged, StatusStaged, r1Steps(), r2Steps(), false, true,
			"found=true enters=[RP-PRESS,RP-B,RP-OUT] awaits=[rp-r2@RP-B co=true] lineside=RP-PRESS/false; " +
				"found=true enters=[RP-B,RP-PRESS] awaits=[] lineside=-"},
		// A placer holding its bin at staging (holdInbound): its segment places
		// with nothing picked up first, so it never co-releases.
		{"a placer holding its bin never co-releases", StatusStaged, StatusStaged, r1Steps(),
			[]resolvedStep{vsPick("RP-STAGE"), rlWait("RP-STAGE", WaitKindStation), vsDrop("RP-PRESS")}, true, true,
			"found=true enters=[RP-PRESS,RP-B,RP-OUT] awaits=[] lineside=RP-PRESS/true; " +
				"found=true enters=[RP-PRESS] awaits=[rp-r1@RP-PRESS co=false] lineside=-"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newReleaseRig(t)
			sd := testdb.SetupStandardData(t, r.db)
			rpNode(t, r, sd.Payload.Code, "RP-PRESS", c.pressFull)
			rpNode(t, r, sd.Payload.Code, "RP-B", c.bFull)
			rpNode(t, r, sd.Payload.Code, "RP-OUT", false)
			rpNode(t, r, sd.Payload.Code, "RP-STAGE", true)
			o1 := rlLeg(t, r.db, "rp-r1", c.r1Status, 0, string(mustJSON(t, c.r1)))
			o2 := rlLeg(t, r.db, "rp-r2", c.r2Status, 0, string(mustJSON(t, c.r2)))
			// Each order's delivery node is its last drop, so Core's redirect
			// patch leaves the segment as built.
			for o, last := range map[int64]string{o1.ID: "RP-OUT", o2.ID: "RP-PRESS"} {
				_, err := r.db.Exec(`UPDATE orders SET delivery_node=$1 WHERE id=$2`, last, o)
				testutil.MustNoErr(t, err, "delivery node")
			}
			_, err := r.db.LinkOrderSiblingsByEdgeUUID("rp-r1", "rp-r2")
			testutil.MustNoErr(t, err, "link siblings")
			_, err = r.db.Exec(`UPDATE orders SET process_node='RP-PRESS' WHERE id=$1`, o1.ID)
			testutil.MustNoErr(t, err, "line node")
			pts := r.d.ReleasePoints("line-1", []string{"rp-r1", "rp-r2"})
			got := rpPoint(pts[0]) + "; " + rpPoint(pts[1])
			if got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}

	t.Run("another station's order is not found", func(t *testing.T) {
		t.Parallel()
		r := newReleaseRig(t)
		rlLeg(t, r.db, "rp-other", StatusStaged, 0, string(mustJSON(t, r1Steps())))
		if got := rpPoint(r.d.ReleasePoints("line-2", []string{"rp-other"})[0]); got != "found=false enters=[] awaits=[] lineside=-" {
			t.Errorf("got %s", got)
		}
	})
}
