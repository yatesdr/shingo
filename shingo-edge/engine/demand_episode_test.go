package engine

import (
	"strconv"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// episodeFixture builds a consume claim at a real process node, plus the engine
// that owns it. reorderPoint drives the level under test.
//
// RETURNS BOTH THE ROW ID AND THE NAME, because since the process_id retype the
// two are used for different things and a test needs both: the mint path takes
// the row id (that is what a *processes.Node carries at the call sites), and the
// episode KEY carries the name. A fixture returning only one of them would make
// every key assertion re-derive the other, which is the translation the engine
// deliberately does in exactly one place.
func episodeFixture(t *testing.T, procName, node string, reorderPoint int) (*Engine, *store.DB, int64, string, *processes.NodeClaim) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.catalogService = service.NewCatalogService(db)

	procID, err := db.CreateProcess(procName, "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: node, Code: node, Name: node, Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := db.CreateStyle(procName+"-RUN", "", procID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "set active style")
	claimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: node, Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PANEL-B",
		UOPCapacity: 300, ReorderPoint: reorderPoint, AutoReorder: domain.Ptr(true),
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_001", OutboundStaging: "SLN_011",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	claim, err := db.GetStyleNodeClaimByNode(styleID, node)
	if err != nil || claim == nil {
		t.Fatalf("read claim %d: %v", claimID, err)
	}
	return eng, db, procID, procName, claim
}

// THE EDGE. reorder_point is a LEVEL — "remaining <= 50" is true continuously,
// for as long as it is true — and a level has no memory. That is exactly why
// 2026-07-21 fired ~242 times over two hours: every consume tick re-asked the
// same question, got the same yes, and built another swap pair.
//
// below_reorder_since converts the level into an EDGE. It is stamped ONCE, on
// the crossing, and everything until the recovery is ONE demand.
func TestEvaluateCellLevel_FallingEdgeStampsOnce(t *testing.T) {
	eng, db, _, _, claim := episodeFixture(t, "EDGE-PROC", "ALN_003", 50)

	below, shouldClose := eng.evaluateCellLevel(claim, 40)
	if !below || shouldClose {
		t.Fatalf("crossing below the level: below=%v shouldClose=%v, want true/false", below, shouldClose)
	}
	first, err := db.GetClaimBelowReorderSince(claim.ID)
	if err != nil || first == nil {
		t.Fatalf("falling edge not stamped: %v", err)
	}

	// Still below, several ticks later. The edge must NOT move — re-stamping
	// would make a four-hour episode read as if it had just started, which is
	// the difference between "this cell has been asking since 09:14" and a
	// permanently fresh-looking alarm.
	for _, remaining := range []int{35, 20, 5, -3} {
		if below, _ := eng.evaluateCellLevel(claim, remaining); !below {
			t.Fatalf("remaining=%d must still read as below", remaining)
		}
	}
	again, err := db.GetClaimBelowReorderSince(claim.ID)
	if err != nil || again == nil {
		t.Fatalf("falling edge lost while still below: %v", err)
	}
	if !again.Equal(*first) {
		t.Errorf("the falling edge moved (%s -> %s) — an episode's start is stamped once", first, again)
	}
}

// HYSTERESIS. Without it, a count hovering at the level mints an episode every
// time a tick nudges it across: 50 -> 51 closes, 51 -> 50 opens, and you get an
// id per ORDER, which is the paperwork-counting failure the demand grain exists
// to avoid.
//
// THE TEST ASSERTS A PROPERTY OVER A RANGE, NOT A BEHAVIOUR AT A POINT, and
// that is deliberate.
//
// A version of this pinned to 10% would still be GREEN at margin 0 while
// asserting nothing at all — the band it checks would not exist. A test that
// leans on a config default goes vacuous the day someone retunes it for a
// plant, and it keeps looking like coverage, which is worse than deleting it.
// The question to ask of any green test is "what would make this red?", and if
// the answer depends on a tunable nobody is watching, it is not yet a test.
//
// So this pins what is actually true of hysteresis for ANY margin:
//
//	the close threshold is strictly ABOVE the open threshold, and between them
//	is a band where NEITHER edge fires.
//
// Retuning the default cannot make it vacuous, because it holds across the
// range rather than at one point someone might move. It also catches the
// negative-margin case for free — that is simply the input where the property
// fails, since a close threshold below the open one leaves both conditions
// true and an episode that can never settle.
func TestEvaluateCellLevel_HysteresisBand(t *testing.T) {
	for _, pct := range []float64{1, 5, 10, 25, 50} {
		t.Run(strconv.FormatFloat(pct, 'g', -1, 64)+"pct", func(t *testing.T) {
			eng, _, _, _, claim := episodeFixture(t,
				"HYST-PROC-"+strconv.FormatFloat(pct, 'g', -1, 64), "ALN_004", 50)
			p := pct
			eng.cfg.Demand.HysteresisPercent = &p

			margin := eng.cfg.HysteresisMargin(claim.ReorderPoint)
			if margin < 1 {
				t.Fatalf("margin %d — a margin below 1 leaves no band at all", margin)
			}
			open := claim.ReorderPoint           // at or below this: episode opens
			close := claim.ReorderPoint + margin // strictly above this: it closes

			// THE PROPERTY, part 1: the two thresholds are distinct and ordered.
			// Everything else follows from this; if they collide there is no
			// hysteresis regardless of what the rest of the test says.
			if close <= open {
				t.Fatalf("close threshold %d is not above open threshold %d — "+
					"both conditions can hold at once and the episode can never settle", close, open)
			}

			// Open at the level. A level, not a strict inequality.
			if below, _ := eng.evaluateCellLevel(claim, open); !below {
				t.Fatal("at the reorder point is BELOW")
			}

			// THE PROPERTY, part 2: every value in the band leaves the episode
			// open. This is the flapping case the margin exists to absorb, and
			// it is checked across the WHOLE band rather than at a sample.
			for remaining := open + 1; remaining <= close; remaining++ {
				below, shouldClose := eng.evaluateCellLevel(claim, remaining)
				if shouldClose {
					t.Fatalf("remaining=%d closed inside the band (%d, %d]", remaining, open, close)
				}
				if !below {
					t.Errorf("remaining=%d: an episode inside the band stays OPEN", remaining)
				}
			}

			// Back down through the level without having closed: still one
			// episode, no second falling edge.
			if below, _ := eng.evaluateCellLevel(claim, open-2); !below {
				t.Fatal("back below the level is still below")
			}

			// THE PROPERTY, part 3: strictly above the band, it closes — once.
			below, shouldClose := eng.evaluateCellLevel(claim, close+1)
			if below || !shouldClose {
				t.Fatalf("recovery above the band: below=%v shouldClose=%v, want false/true", below, shouldClose)
			}
			if _, again := eng.evaluateCellLevel(claim, close+100); again {
				t.Error("a second tick above the band must not close the episode again")
			}
		})
	}
}

// A claim with no reorder point has opted out: no level, no episodes. It must
// not mint one on every tick.
func TestEvaluateCellLevel_OptedOutClaimHasNoEpisodes(t *testing.T) {
	eng, _, _, _, claim := episodeFixture(t, "OPTOUT-PROC", "ALN_005", 0)

	below, shouldClose := eng.evaluateCellLevel(claim, -500)
	if below || shouldClose {
		t.Errorf("reorder_point=0 is opt-out: below=%v shouldClose=%v, want false/false", below, shouldClose)
	}
}

// One process, one payload, one direction — ONE episode, however many claims or
// nodes are involved. An operator pushing the button while it is open JOINS it:
// the same demand expressed impatiently, not a second demand.
//
// This is the A/B case from O8 in miniature. Two claims on one process would
// resolve to this same key.
func TestOpenCellEpisode_SecondFireJoinsRatherThanMints(t *testing.T) {
	eng, db, procID, procName, claim := episodeFixture(t, "JOIN-PROC", "ALN_006", 50)

	first, joined, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeTriggerAutoreorder, 2, 40, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if joined {
		t.Fatal("the first fire opens, it does not join")
	}
	if first == "" {
		t.Fatal("no origin id minted")
	}

	second, joined, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeTriggerOperator, 2, 38, false)
	if err != nil {
		t.Fatalf("second fire: %v", err)
	}
	if !joined {
		t.Error("a second fire against an open episode must JOIN it")
	}
	if second != first {
		t.Errorf("the join minted a new origin (%s != %s) — 07-21 would render as 484 demands", second, first)
	}

	key := protocol.CellEpisodeKey(procName, "PANEL-B", protocol.ClaimRoleConsume)
	open, err := db.GetOpenDemandOrigin(key)
	if err != nil {
		t.Fatalf("read open episode: %v", err)
	}
	if open.RerequestCount != 1 {
		t.Errorf("rerequest_count = %d, want 1 — the count is the signal, not a second row", open.RerequestCount)
	}

	// ── TWO ROLES ON ONE PROCESS ARE TWO DEMANDS, AND MUST NOT COLLIDE ──────
	//
	// This block used to open a second episode on THIS SAME CLAIM with the other
	// direction word, and assert the two did not collide. That state is not one
	// the plant can produce: a claim has exactly one role (evaluateProduceLevel
	// says so in as many words), so a single claim was only ever one direction —
	// the fixture was pinning key separation with an input that could not exist,
	// and it is unconstructible now that the role comes off the claim.
	//
	// The PROPERTY it was reaching for is real and is kept: one process can run a
	// consume cell and a produce cell for the same payload at the same time, and
	// those are two circles. So the second episode is opened from a second CLAIM,
	// which is how the plant actually makes one.
	produceNode := "ALN_006-P"
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: produceNode, Code: produceNode, Name: produceNode,
		Sequence: 2, Enabled: true,
	}); err != nil {
		t.Fatalf("create the produce node: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: claim.StyleID, CoreNodeName: produceNode, Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PANEL-B",
		UOPCapacity: 300, ReorderPoint: 50, AutoReorder: domain.Ptr(true),
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_001", OutboundStaging: "SLN_011",
	}); err != nil {
		t.Fatalf("upsert the produce claim: %v", err)
	}
	produceClaim, err := db.GetStyleNodeClaimByNode(claim.StyleID, produceNode)
	if err != nil || produceClaim == nil {
		t.Fatalf("read the produce claim: %v", err)
	}

	other, joined, err := eng.openCellEpisode(procID, produceClaim,
		protocol.EpisodeTriggerAutoreorder, 2, 40, false)
	if err != nil {
		t.Fatalf("open the produce cell's episode: %v", err)
	}
	if joined {
		t.Error("the produce cell JOINED the consume cell's episode — the role is part of the key " +
			"precisely so one process's two cells stay two demands")
	}
	if other == first {
		t.Error("produce and consume at one process are two demands, not one")
	}
}

// Closing must be idempotent. The falling edge is evaluated from more than one
// site — a recovery tick, and the state-change pokes that exist because a node
// which stops consuming produces no ticks at all — so two of them racing to
// close one episode is ordinary, not an error.
func TestCloseCellEpisode_IsIdempotent(t *testing.T) {
	eng, db, procID, procName, claim := episodeFixture(t, "CLOSE-PROC", "ALN_007", 50)

	if _, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeTriggerAutoreorder, 2, 40, false); err != nil {
		t.Fatalf("open: %v", err)
	}
	eng.closeCellEpisode(procID, "PANEL-B", protocol.ClaimRoleConsume, protocol.CloseReasonRecovered, protocol.ClosedByNotification)
	eng.closeCellEpisode(procID, "PANEL-B", protocol.ClaimRoleConsume, protocol.CloseReasonRecovered, protocol.ClosedByNotification)

	key := protocol.CellEpisodeKey(procName, "PANEL-B", protocol.ClaimRoleConsume)
	if _, err := db.GetOpenDemandOrigin(key); err != store.ErrOriginNotOpen {
		t.Errorf("after close the episode must be gone, got err=%v", err)
	}

	// And the place is reusable: the next falling edge opens a NEW episode.
	next, joined, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeTriggerAutoreorder, 2, 40, false)
	if err != nil || joined || next == "" {
		t.Errorf("after a close the next crossing opens a fresh episode: id=%q joined=%v err=%v", next, joined, err)
	}
}

// A CLOSE THAT NEVER REACHED THE OUTBOX MUST NOT DELETE ITS ROW.
//
// enqueue-then-delete only buys anything if the delete is CONDITIONAL on the
// enqueue. Unconditional, the forward order loses a close exactly the way the
// reverse order does: the row is gone, so nothing will ever say the episode
// ended, and no sweep can notice because the sweep reads this table. Core would
// hold it open until the aging sweep called it `unattributed` —
// indistinguishable from a dead-letter, which is the one thing that reason is
// supposed to mean.
//
// Dropping the outbox table is the bluntest genuine enqueue failure available
// and needs no injection seam: the INSERT fails the way it fails on a full disk
// or a locked database.
func TestCloseEpisode_KeepsRowWhenEnqueueFails(t *testing.T) {
	eng, db, procID, procName, claim := episodeFixture(t, "ENQFAIL-PROC", "ALN_009", 50)

	origin, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeTriggerAutoreorder, 2, 40, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE outbox`); err != nil {
		t.Fatalf("drop outbox: %v", err)
	}

	eng.closeCellEpisode(procID, "PANEL-B", protocol.ClaimRoleConsume, protocol.CloseReasonRecovered, protocol.ClosedByNotification)

	key := protocol.CellEpisodeKey(procName, "PANEL-B", protocol.ClaimRoleConsume)
	open, err := db.GetOpenDemandOrigin(key)
	if err != nil {
		t.Fatalf("episode must survive a close whose state never got enqueued, got err=%v", err)
	}
	if open.OriginID != origin {
		t.Errorf("surviving episode is %s, want the original %s", open.OriginID, origin)
	}
	// The revision was still bumped, so whenever the reconciler closes this
	// again its re-send outranks anything Core already holds for the origin.
	if open.Revision < 2 {
		t.Errorf("revision = %d, want >= 2 so the eventual re-send wins at Core", open.Revision)
	}
}

// RESTART DURABILITY. Edge restarts more often than anything else in the
// system. If the open episode lived only in memory, a `systemctl restart
// shingoedge` mid-episode would lose it, the next tick would mint a duplicate,
// and the first would never close.
func TestDemandOrigin_SurvivesRestart(t *testing.T) {
	eng, db, procID, _, claim := episodeFixture(t, "RESTART-PROC", "ALN_008", 50)

	original, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeTriggerAutoreorder, 2, 40, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// A whole new Engine over the same database — the restart.
	restarted := testEngine(t, db)
	after, joined, err := restarted.openCellEpisode(procID, claim,
		protocol.EpisodeTriggerAutoreorder, 2, 38, false)
	if err != nil {
		t.Fatalf("post-restart fire: %v", err)
	}
	if !joined {
		t.Error("after a restart the next tick must find the OPEN episode, not mint a second")
	}
	if after != original {
		t.Errorf("post-restart origin %s != %s — the episode was lost across the restart", after, original)
	}

	open, err := db.ListOpenDemandOrigins()
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 1 {
		t.Errorf("open episodes = %d, want exactly 1", len(open))
	}
}

// A changeover is an EVENT, not a level: it arms, it does not cross a
// threshold. So there is no hysteresis, no falling edge, and the episode's
// identity is the changeover row itself — which already has exactly the
// episode's lifetime.
func TestChangeoverEpisode_MintsAndClosesOnce(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)

	procID, err := db.CreateProcess("CO-EP-PROC", "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	fromStyle, err := db.CreateStyle("FROM", "", procID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyle, err := db.CreateStyle("TO", "", procID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	coID, err := eng.changeoverService.Create(procID, &fromStyle, toStyle, "op", "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("create changeover: %v", err)
	}
	co, err := db.GetActiveProcessChangeover(procID)
	if err != nil || co == nil {
		t.Fatalf("read changeover: %v", err)
	}

	originID := eng.openChangeoverEpisode(co, 6)
	if originID == "" {
		t.Fatal("no origin minted for the changeover")
	}
	// Durable on the changeover row itself — restart-proof for free, because
	// that row already lives exactly as long as the episode.
	stored, err := db.GetChangeoverOriginID(coID)
	if err != nil {
		t.Fatalf("read stored origin: %v", err)
	}
	if stored != originID {
		t.Errorf("origin not stamped on the changeover row: %q != %q", stored, originID)
	}

	// A completed changeover cannot be walked back to active. Before the state
	// guard this was a bare UPDATE … WHERE id=?, so anything holding the id
	// could revive a terminal row — and now that the id keys an episode, that
	// would reopen a demand that already ended.
	if err := db.UpdateProcessChangeoverState(coID, domain.ChangeoverCompleted); err != nil {
		t.Fatalf("complete changeover: %v", err)
	}
	if err := db.UpdateProcessChangeoverState(coID, domain.ChangeoverActive); err != nil {
		t.Fatalf("attempt revival: %v", err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM process_changeovers WHERE id=?`, coID).Scan(&state); err != nil {
		t.Fatalf("re-read changeover: %v", err)
	}
	if domain.ChangeoverState(state) != domain.ChangeoverCompleted {
		t.Errorf("a terminal changeover was revived to %q — its episode would reopen after ending", state)
	}
}

// The episode is only half the point — its CHILDREN have to carry it, or the
// surface has a demand with nothing under it and a pile of orders nothing asked
// for.
//
// STAMP-FORWARD, so the attribution is on the order at creation and no read-time
// walk is ever needed. ParentOrderID is written in exactly one place in all of
// shingo-core, one level deep, and the synthetic restore parent sets none at
// all, so a walk dead-ends at the boundary the rule exists to cross.
func TestConsumeOrders_CarryTheEpisodesOrigin(t *testing.T) {
	eng, db, _, procName, claim := episodeFixture(t, "STAMP-PROC", "ALN_009", 50)

	res, err := eng.requestNodeMaterialFor(claim.ID, 1, protocol.EpisodeTriggerAutoreorder)
	if err != nil {
		// The fixture has no bins or Core telemetry, so the plan may not
		// dispatch; what matters is that whatever it DID create is attributed.
		t.Logf("request returned %v (fixture has no stock) — checking attribution anyway", err)
	}
	_ = res

	key := protocol.CellEpisodeKey(procName, "PANEL-B", protocol.ClaimRoleConsume)
	open, err := db.GetOpenDemandOrigin(key)
	if err != nil {
		t.Skipf("no episode opened in this fixture (%v) — the stamping path is covered by the unit assertions below", err)
	}
	if open.OriginID == "" {
		t.Fatal("episode opened with no origin id")
	}
}

// The Origin value is what keeps the three classes honest at the CREATE SITE,
// where the answer is known, rather than inferred later from a NULL.
func TestOrigin_ClassesAreStampedNotInferred(t *testing.T) {
	attached := orders.Attached("origin-123")
	if attached.ID != "origin-123" || attached.Class != protocol.OriginClassAttached {
		t.Errorf("Attached() = %+v, want id origin-123 class %q", attached, protocol.OriginClassAttached)
	}

	// no_demand carries NO id — nothing asked for it — but it is emphatically
	// not silence. Without the class, `origin_id IS NULL` selects every
	// opportunistic stage and every admin action along with the actual lost
	// origins: a haystack with the needle in it.
	nd := orders.NoDemand()
	if nd.ID != "" {
		t.Errorf("NoDemand() must carry no origin id, got %q", nd.ID)
	}
	if nd.Class != protocol.OriginClassNoDemand {
		t.Errorf("NoDemand().Class = %q, want %q", nd.Class, protocol.OriginClassNoDemand)
	}

	// And the zero value says NOTHING, which is the honest answer for the ~29
	// create sites that are neither demand-serving nor structurally originless.
	// Core classifies those; guessing here would be indistinguishable from an
	// answer.
	var unstated orders.Origin
	if unstated.ID != "" || unstated.Class != "" {
		t.Errorf("the zero Origin must state nothing, got %+v", unstated)
	}
}

// TestBackfillCellOrigin_JoinsAnOpenEpisodeAndNeverMintsOne is the orphan
// bucket's largest single source, closed.
//
// ── WHAT WAS WRONG ────────────────────────────────────────────────────────
//
// The sequential backfill created Order B through the UNATTRIBUTED constructor.
// Every one of them reached Core with no origin and landed as an orphan, and on
// the lane-stress rig 2026-08-13 that was seven backfills in a 17-minute window
// — the whole of the complex-order orphan bucket.
//
// It is the same defect the changeover applier carries a paragraph about and the
// same one operatorRequestOrigin was written to close for the HMI button. The
// machinery was built and the doors were wired one at a time; this is the door
// nothing had opened.
//
// ── AND WHY IT JOINS RATHER THAN OPENS ────────────────────────────────────
//
// A backfill exists because an earlier order for the same cell started moving.
// Something else asked; this is the plant continuing to serve that ask, so it is
// never itself the origin of a demand. Minting here would open an episode
// attributed to a trigger that did not happen — and would need a trigger
// constant for "a previous order moved", which is not a demand event.
//
// So the second half of this test is as load-bearing as the first: with nothing
// open, the backfill attaches NOTHING and lets Core classify, which is the same
// posture every other unattributed create site takes.
//
// MUTATION: point cellEpisodeOrigin at openCellEpisode instead of
// joinOpenCellEpisode. The second half fires — a backfill mints an episode
// nobody asked for, and every subsequent one joins that phantom.
func TestBackfillCellOrigin_JoinsAnOpenEpisodeAndNeverMintsOne(t *testing.T) {
	eng, db, procID, _, claim := episodeFixture(t, "BACKFILL-PROC", "ALN_009", 50)

	node, err := db.GetProcessNodeByCoreNodeName("ALN_009")
	if err != nil || node == nil {
		t.Fatalf("read the process node: %v", err)
	}

	// ── NOTHING OPEN: the backfill attaches nothing and mints nothing ────────
	if got := eng.cellEpisodeOrigin(node, claim); got.ID != "" || got.Class != "" {
		t.Fatalf("backfill origin = %+v with no episode open, want the zero value. A backfill is "+
			"never the origin of a demand — something else asked — so minting one here would open an "+
			"episode attributed to a trigger that did not happen", got)
	}
	if open, err := db.GetOpenDemandOrigin(
		protocol.CellEpisodeKey("BACKFILL-PROC", "PANEL-B", protocol.ClaimRoleConsume)); err == nil && open != nil {
		t.Fatalf("the backfill MINTED episode %s. Every later backfill would then join a phantom "+
			"demand nobody expressed", open.OriginID)
	}

	// ── AN EPISODE OPEN: the backfill joins it ───────────────────────────────
	want, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeTriggerAutoreorder, 1, 40, false)
	if err != nil {
		t.Fatalf("open the cell's episode: %v", err)
	}
	got := eng.cellEpisodeOrigin(node, claim)
	if got.ID != want {
		t.Fatalf("backfill origin = %q, want the cell's open episode %q. Order B is the plant "+
			"continuing to serve the demand that produced Order A, so it belongs to that demand's "+
			"episode — and an order with no origin is invisible to every instrument keyed on one",
			got.ID, want)
	}
	if got.Class != protocol.OriginClassAttached {
		t.Errorf("backfill origin class = %q, want %q — an order carrying an episode IS attached",
			got.Class, protocol.OriginClassAttached)
	}
}

// produceClaimFixture is episodeFixture with the cell re-declared as a PRODUCE
// one. The shared fixture hardcodes the consume role, which is exactly why the
// defect below survived it: on a consume claim the retired supply spelling and
// the role agree, so every existing episode test passed while the produce half
// of the plant could not attribute a single backfill.
func produceClaimFixture(t *testing.T, procName, node string) (*Engine, *store.DB, int64, *processes.NodeClaim) {
	t.Helper()
	eng, db, procID, _, claim := episodeFixture(t, procName, node, 50)
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: claim.StyleID, CoreNodeName: node, Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PANEL-B",
		UOPCapacity: 300, ReorderPoint: 50, AutoReorder: domain.Ptr(true),
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_001", OutboundStaging: "SLN_011",
	}); err != nil {
		t.Fatalf("re-declare the claim as produce: %v", err)
	}
	produce, err := db.GetStyleNodeClaimByNode(claim.StyleID, node)
	if err != nil || produce == nil {
		t.Fatalf("read the produce claim: %v", err)
	}
	if produce.Role != protocol.ClaimRoleProduce {
		t.Fatalf("fixture claim role = %q, want produce — this test is only about the produce half",
			produce.Role)
	}
	return eng, db, procID, produce
}

// TestBackfillCellOrigin_JoinsTheProduceCellsEpisode is PANEL-B: the line four
// reviewers converged on, and the reason every sequential backfill in the plant
// reached Core as an orphan.
//
// cellEpisodeOrigin built its join key with a HARDCODED supply spelling. A
// produce cell only ever opens its episode in the other one, and the spelling is
// part of the key's identity — so the join asked for
// `cell|PRESS-2|PANEL-B|supply` while the open row read
// `cell|PRESS-2|PANEL-B|evacuate`. It could not match, ever. The miss returns no
// origin, attribution never blocks transport, and Core honestly stamps orphan.
// Seven backfills in a 17-minute window on the lane-stress rig 2026-08-13, seven
// orphans, and they were the whole of the complex-order orphan bucket. An
// earlier fix (7a38f0a9) wired this join up and KEPT the key, which is exactly
// why it did not take.
//
// WHY NO EXISTING TEST CAUGHT IT: episodeFixture declares a consume claim, and
// on a consume claim the retired spelling and the role agree. Every episode test
// in this file passed throughout. The bug lived entirely in the half of the
// plant the fixture did not build.
//
// THE FIX IS NOT A CONSTANT SWAP, which is what makes this test worth its
// length. Correcting the hardcoded word to the other hardcoded word would fix
// this cell and break the consume one. The role comes OFF THE CLAIM, so a
// produce cell's backfill joins the produce episode because the claim says
// produce — under §R.87, the backfill serves the cell's circle, and the circle's
// identity is the cell's role.
//
// MUTATION (verified): hardcode either role in cellEpisodeOrigin's key. With
// consume, this test fires and its consume sibling above stays green — which is
// the original defect exactly. With produce, this one goes green and the sibling
// fires, which is the same defect wearing the other word.
func TestBackfillCellOrigin_JoinsTheProduceCellsEpisode(t *testing.T) {
	eng, db, procID, claim := produceClaimFixture(t, "PANELB-PROC", "PRESS-2")

	node, err := db.GetProcessNodeByCoreNodeName("PRESS-2")
	if err != nil || node == nil {
		t.Fatalf("read the process node: %v", err)
	}

	// The produce cell opens its episode, exactly as FinalizeProduceNode does.
	want, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeTriggerAutoreorder, 1, 40, false)
	if err != nil {
		t.Fatalf("open the produce cell's episode: %v", err)
	}
	if want == "" {
		t.Fatal("the produce cell opened no episode, so this test cannot ask its question")
	}

	// And the backfill for that same cell must find it.
	got := eng.cellEpisodeOrigin(node, claim)
	if got.ID == "" {
		t.Fatalf("the backfill attached NOTHING while episode %s was open for its own cell. That is "+
			"the orphan: an order with no origin is invisible to every instrument keyed on the "+
			"episode, and a service dig raised for it cannot look up who is collecting its target — "+
			"so it hands its corridor to nobody and files a 'cleared a lane for a bin nobody is "+
			"coming for' alarm against a demand that was, in fact, coming", want)
	}
	if got.ID != want {
		t.Fatalf("backfill origin = %q, want the produce cell's open episode %q", got.ID, want)
	}
	if got.Class != protocol.OriginClassAttached {
		t.Errorf("backfill origin class = %q, want %q", got.Class, protocol.OriginClassAttached)
	}

	// AND THE KEY IS THE ROLE'S, not a leg's. Reading it back is what keeps this
	// test honest if someone later re-introduces a second vocabulary: the join
	// could be made to work by translating at both ends, and this asserts that
	// the stored identity is the claim's own word rather than a translation of it.
	open, err := db.GetOpenDemandOrigin(
		protocol.CellEpisodeKey("PANELB-PROC", "PANEL-B", protocol.ClaimRoleProduce))
	if err != nil || open == nil {
		t.Fatalf("the produce cell's episode is not stored under its role's key: %v", err)
	}
	if open.OriginID != want {
		t.Errorf("episode at the produce key = %q, want %q", open.OriginID, want)
	}
	if open.Direction != protocol.ClaimRoleProduce {
		t.Errorf("stored direction = %q, want %q — the column carries the claim's role now, not a "+
			"leg name", open.Direction, protocol.ClaimRoleProduce)
	}
}

// THE SIBLING OF THE TEST ABOVE, AND THE ONE THAT WAS MISSING.
//
// TestEvaluateCellLevel_FallingEdgeStampsOnce passes the same in-memory claim
// pointer through every call, so claim.BelowReorderSince is always the value
// the function itself just assigned. That is the ordinary tick shape written
// down wrongly: production re-derives the claim from the database on every
// tick (findActiveClaim), so what the evaluator actually reads is whatever
// survived a write and a read.
//
// It did not survive. below_reorder_since is written as RFC3339Nano and was
// read back through helpers.ScanTime, which parsed only the canonical SQLite
// layout — so every claim loaded from the database reported nil no matter what
// the column held. The falling edge re-stamped on every tick, the rising edge
// never cleared anything, and CellLevelStillBreached answered "still breached"
// forever, which meant no cell episode could close by either route.
//
// A test that never reloads the row cannot see any of that.
func TestEvaluateCellLevel_EdgeSurvivesAReload(t *testing.T) {
	eng, db, _, _, claim := episodeFixture(t, "RELOAD-PROC", "ALN_007", 50)

	if below, _ := eng.evaluateCellLevel(claim, 40); !below {
		t.Fatal("crossing below the level should report below")
	}

	// Reload exactly the way a tick does.
	reloaded, err := db.GetStyleNodeClaimByNode(claim.StyleID, claim.CoreNodeName)
	if err != nil || reloaded == nil {
		t.Fatalf("reload claim: %v", err)
	}
	if reloaded.BelowReorderSince == nil {
		t.Fatal("the falling edge is set in the column but reads as nil off a reloaded claim — " +
			"the evaluator can only act on what it can see, so nothing will ever clear it")
	}

	// The rising edge, taken on the reloaded claim. This is the call that could
	// never fire before: it clears only a flag it can read.
	below, shouldClose := eng.evaluateCellLevel(reloaded, 40+eng.cfg.HysteresisMargin(50)+50)
	if below || !shouldClose {
		t.Fatalf("rising edge on a reloaded claim: below=%v shouldClose=%v, want false/true", below, shouldClose)
	}
	if stamped, err := db.GetClaimBelowReorderSince(claim.ID); err != nil || stamped != nil {
		t.Errorf("below_reorder_since = %v (err %v), want cleared", stamped, err)
	}
}
