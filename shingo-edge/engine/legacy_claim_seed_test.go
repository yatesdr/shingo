package engine

import (
	"database/sql"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// upsertClaimRetiredMode upserts a style node claim, transparently tolerating a
// swap mode that may no longer be PERSISTED. Two modes are retired, for two
// different reasons, and both still exist as stored rows the read paths
// characterize:
//
//	simple       retired by the ingress lockdown. It survives as a runtime
//	             CycleMode descriptor and as legacy DB rows. The changeover
//	             planner's bare-move default, the manual_swap guard and produce
//	             simple mode are all characterized on nodes whose stored claim
//	             mode is "simple".
//	manual_swap  retired by the loader ownership move. Core owns loader
//	             configuration and SynthClaim serves it, so a stored loader
//	             claim is a second authority; SetCoreLoaders quarantines any
//	             that appear and the upsert allowlist no longer accepts one.
//	             The READ paths that tolerate such a row are still live —
//	             PayloadsForLoader unions over stored claims, loadActiveNode's
//	             short-circuit prefers one, the board renders one — and the
//	             tests below are what pin them.
//
// For a retired mode it upserts with a configurable placeholder (to pass the
// allowlist), then rewrites swap_mode directly — exactly the pre-lockdown row
// shape the read path still tolerates. Every other mode passes straight through
// to UpsertStyleNodeClaim with identical behavior, including fail-loud on a
// blank mode. Engine test claim seeds route through this shim instead of
// calling UpsertStyleNodeClaim directly, so a single seam owns both
// accommodations rather than each test carrying its own raw UPDATE.
//
// A SEED HERE IS NOT A CLAIM THAT A PLANT HAS ONE. Neither retired mode can be
// written through the production path any more; a row that reaches these tables
// is a legacy row or a fixture. The seam exists so that stays visible.
//
// It is also where a seed's capacity lands, for the same "one seam" reason —
// see seedClaimCapacity.
func upsertClaimRetiredMode(db *store.DB, in processes.NodeClaimInput) (int64, error) {
	if err := seedClaimCapacity(db, in); err != nil {
		return 0, err
	}
	retired := in.SwapMode == protocol.SwapModeSimple || in.SwapMode == protocol.SwapModeManualSwap
	if !retired {
		return db.UpsertStyleNodeClaim(in)
	}
	want := in.SwapMode
	in.SwapMode = protocol.SwapModeSequential // placeholder to pass the allowlist
	id, err := db.UpsertStyleNodeClaim(in)
	if err != nil {
		return id, err
	}
	if _, err := db.DB.Exec(`UPDATE style_node_claims SET swap_mode=? WHERE id=?`, string(want), id); err != nil {
		return id, err
	}
	return id, nil
}

// withManualSwap restores the loader mode to a walk over
// protocol.ConfigurableSwapModes().
//
// THE PERSISTED SET AND THE RUNTIME SET USED TO BE THE SAME SET, and every
// mode-walking test in this package keyed on the persisted one because of it.
// They are different now: manual_swap left ConfigurableSwapModes when the
// loader ownership move retired it as a stored value, but a claim still CARRIES
// it at runtime — domain.Loader.SynthClaim stamps it on every Core-owned loader
// window, and those claims reach the dispatch and changeover builders like any
// other.
//
// So a walker asking "which modes can reach this builder" must add it back, or
// it silently stops covering a live path and goes green for the wrong reason.
// pressPositionSwapMode was already the first mode in that position — synthesized
// by the press-index fan-out, rejected by UpsertClaim, still emitting cell legs —
// and is still appended by hand at the sites that need it, so a walk's
// composition here is exactly what it was plus the mode it just lost.
func withManualSwap(modes []protocol.SwapMode) []protocol.SwapMode {
	return append(modes, protocol.SwapModeManualSwap)
}

// seedClaimCapacity puts the capacity a seed asks for where the claim now
// reads it.
//
// UOPCapacity on the input used to be persisted on the claim row, so a seed
// that set it got it back. It is a dead column now — capacity is resolved from
// payload_catalog on every read, keyed on payload_code — so the same seed would
// read back 0 and every test about a full bin would be testing an empty one.
// Rather than make each of the twenty-odd engine seeds say the same thing, the
// one seam they all route through writes the catalog row the claim will resolve
// against.
//
// Nothing to do for a seed that names no payload or asks for no capacity: a
// zero capacity is what an unknown payload resolves to anyway.
func seedClaimCapacity(db *store.DB, in processes.NodeClaimInput) error {
	if in.PayloadCode == "" || in.UOPCapacity == 0 {
		return nil
	}
	var id int64
	err := db.DB.QueryRow(`SELECT id FROM payload_catalog WHERE code=?`, in.PayloadCode).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		_, err = db.DB.Exec(`INSERT INTO payload_catalog (id, name, code, uop_capacity, updated_at)
			VALUES ((SELECT COALESCE(MAX(id),0)+1 FROM payload_catalog), ?, ?, ?, datetime('now'))`,
			in.PayloadCode, in.PayloadCode, in.UOPCapacity)
		return err
	case err != nil:
		return err
	}
	_, err = db.DB.Exec(`UPDATE payload_catalog SET uop_capacity=? WHERE id=?`, in.UOPCapacity, id)
	return err
}
