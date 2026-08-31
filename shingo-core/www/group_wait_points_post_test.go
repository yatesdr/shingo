package www

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// group_wait_points_post_test.go — the group waiting spots are saved against the
// GROUP's node id, and nothing else in this suite can say so.
//
// ── WHY THIS NEEDS ITS OWN TEST ───────────────────────────────────────────
//
// The section renders two kinds of row against ONE endpoint. The per-lane rows
// post `{node_id: <laneId>, key: lane_gate_point}`; the block's row posts
// `{node_id: <groupId>, key: group_wait_points}`. Both are well-formed JSON to
// the same handler, both return 200, and both write a real property.
//
// Post the block's list against a lane id and the UI reports success, the field
// keeps its value on reload — GetNodeProperty finds it, on the wrong node — and
// ONE aisle is gated by a legacy override while the rest of the block stays
// ungated with nothing anywhere saying so. That is a config bug that survives a
// page refresh, which is the kind that gets found on the floor.
//
// The drift scans cannot see it: TestNoMalformedDataActionInJS checks the SHAPE
// of data-action values, and the template scan reads templates/*.html, where
// none of this lives. Nothing else asserts which id a POST body carries.
//
// MUTATION (verified): change the group row's post to use
// `parseInt(groupRow.dataset.laneId, 10)` or key 'lane_gate_point'. Either fires.

var (
	// The block's spots, against the group.
	groupPostRe = regexp.MustCompile(
		`node_id:\s*parseInt\(\s*groupRow\.dataset\.groupId\s*,\s*10\s*\)\s*,\s*key:\s*'group_wait_points'`)
	// The legacy per-lane override, against the lane. Pinned in the same test so
	// a refactor that unified the two rows onto one id has to fail here rather
	// than quietly write both properties onto the same node.
	lanePostRe = regexp.MustCompile(
		`node_id:\s*parseInt\(\s*row\.dataset\.laneId\s*,\s*10\s*\)\s*,\s*key:\s*'lane_gate_point'`)
)

func TestGroupWaitPointsPostAgainstTheGroupID(t *testing.T) {
	t.Parallel()
	body, err := fs.ReadFile(staticFS, "static/pages/nodes-detail.js")
	if err != nil {
		t.Fatalf("read nodes-detail.js: %v", err)
	}
	js := string(body)

	if !groupPostRe.MatchString(js) {
		t.Error("the block's waiting spots are not posted as {node_id: <groupId>, key: 'group_wait_points'} — " +
			"posting them against a lane id writes the whole block's list onto ONE aisle as a legacy " +
			"override: the save reports success, the value survives a reload, and every other lane in " +
			"the block is silently ungated")
	}
	if !lanePostRe.MatchString(js) {
		t.Error("the per-lane legacy override is not posted as {node_id: <laneId>, key: 'lane_gate_point'} — " +
			"if the two rows have been unified onto one id, one of the two properties is now being " +
			"written to the wrong node")
	}

	// NOT VACUOUS: the section has to still exist. A rename that deleted the
	// group row would otherwise make both patterns above unreachable and this
	// test would pass by finding nothing to check.
	for _, anchor := range []string{"groupWaitRow", "clearGroupWaitPoints", "group-wait-input"} {
		if !strings.Contains(js, anchor) {
			t.Fatalf("nodes-detail.js no longer contains %q — the waiting-spots section has been "+
				"renamed or removed and this test is now checking nothing", anchor)
		}
	}
}
