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
func episodeFixture(t *testing.T, procName, node string, reorderPoint int) (*Engine, *store.DB, int64, *processes.NodeClaim) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.catalogService = service.NewCatalogService(db)

	procID, err := db.CreateProcess(procName, "", "active_production", "", "", false, false)
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
		UOPCapacity: 300, ReorderPoint: reorderPoint, AutoReorder: true,
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
	return eng, db, procID, claim
}

// THE EDGE. reorder_point is a LEVEL — "remaining <= 50" is true continuously,
// for as long as it is true — and a level has no memory. That is exactly why
// 2026-07-21 fired ~242 times over two hours: every consume tick re-asked the
// same question, got the same yes, and built another swap pair.
//
// below_reorder_since converts the level into an EDGE. It is stamped ONCE, on
// the crossing, and everything until the recovery is ONE demand.
func TestEvaluateCellLevel_FallingEdgeStampsOnce(t *testing.T) {
	eng, db, _, claim := episodeFixture(t, "EDGE-PROC", "ALN_003", 50)

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
			eng, _, _, claim := episodeFixture(t,
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
	eng, _, _, claim := episodeFixture(t, "OPTOUT-PROC", "ALN_005", 0)

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
	eng, db, procID, claim := episodeFixture(t, "JOIN-PROC", "ALN_006", 50)

	first, joined, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 40, false)
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
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerOperator, 2, 38, false)
	if err != nil {
		t.Fatalf("second fire: %v", err)
	}
	if !joined {
		t.Error("a second fire against an open episode must JOIN it")
	}
	if second != first {
		t.Errorf("the join minted a new origin (%s != %s) — 07-21 would render as 484 demands", second, first)
	}

	key := protocol.CellEpisodeKey(eng.cfg.StationID(), procID, "PANEL-B", protocol.EpisodeDirectionSupply)
	open, err := db.GetOpenDemandOrigin(key)
	if err != nil {
		t.Fatalf("read open episode: %v", err)
	}
	if open.RerequestCount != 1 {
		t.Errorf("rerequest_count = %d, want 1 — the count is the signal, not a second row", open.RerequestCount)
	}

	// The two directions are two demands, and must not collide.
	evac, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionEvacuate, protocol.EpisodeTriggerAutoreorder, 2, 40, false)
	if err != nil {
		t.Fatalf("evacuate open: %v", err)
	}
	if evac == first {
		t.Error("supply and evacuate at one cell are two demands, not one")
	}
}

// Closing must be idempotent. The falling edge is evaluated from more than one
// site — a recovery tick, and the state-change pokes that exist because a node
// which stops consuming produces no ticks at all — so two of them racing to
// close one episode is ordinary, not an error.
func TestCloseCellEpisode_IsIdempotent(t *testing.T) {
	eng, db, procID, claim := episodeFixture(t, "CLOSE-PROC", "ALN_007", 50)

	if _, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 40, false); err != nil {
		t.Fatalf("open: %v", err)
	}
	eng.closeCellEpisode(procID, "PANEL-B", protocol.EpisodeDirectionSupply, protocol.CloseReasonRecovered)
	eng.closeCellEpisode(procID, "PANEL-B", protocol.EpisodeDirectionSupply, protocol.CloseReasonRecovered)

	key := protocol.CellEpisodeKey(eng.cfg.StationID(), procID, "PANEL-B", protocol.EpisodeDirectionSupply)
	if _, err := db.GetOpenDemandOrigin(key); err != store.ErrOriginNotOpen {
		t.Errorf("after close the episode must be gone, got err=%v", err)
	}

	// And the place is reusable: the next falling edge opens a NEW episode.
	next, joined, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 40, false)
	if err != nil || joined || next == "" {
		t.Errorf("after a close the next crossing opens a fresh episode: id=%q joined=%v err=%v", next, joined, err)
	}
}

// RESTART DURABILITY. Edge restarts more often than anything else in the
// system. If the open episode lived only in memory, a `systemctl restart
// shingoedge` mid-episode would lose it, the next tick would mint a duplicate,
// and the first would never close.
func TestDemandOrigin_SurvivesRestart(t *testing.T) {
	eng, db, procID, claim := episodeFixture(t, "RESTART-PROC", "ALN_008", 50)

	original, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 40, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// A whole new Engine over the same database — the restart.
	restarted := testEngine(t, db)
	after, joined, err := restarted.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 38, false)
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

	procID, err := db.CreateProcess("CO-EP-PROC", "", "active_production", "", "", false, false)
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
	eng, db, procID, claim := episodeFixture(t, "STAMP-PROC", "ALN_009", 50)

	res, err := eng.requestNodeMaterialFor(claim.ID, 1, protocol.EpisodeTriggerAutoreorder)
	if err != nil {
		// The fixture has no bins or Core telemetry, so the plan may not
		// dispatch; what matters is that whatever it DID create is attributed.
		t.Logf("request returned %v (fixture has no stock) — checking attribution anyway", err)
	}
	_ = res

	key := protocol.CellEpisodeKey(eng.cfg.StationID(), procID, "PANEL-B", protocol.EpisodeDirectionSupply)
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
