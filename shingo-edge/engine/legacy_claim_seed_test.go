package engine

import (
	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// upsertClaimLegacySimple upserts a style node claim, transparently tolerating
// the retired "simple" swap mode. The ingress lockdown made the store allowlist
// reject "simple" as a configurable mode — it survives only as a runtime
// CycleMode descriptor and as legacy DB rows. Many engine tests characterize
// behavior on nodes whose stored claim mode is "simple": the changeover
// planner's bare-move default, the manual_swap guard, produce simple mode. Those
// seeds must still produce a swap_mode='simple' row.
//
// For "simple" it upserts with a configurable placeholder (to pass the
// allowlist), then rewrites swap_mode directly — exactly the pre-lockdown row
// shape the read path still tolerates. Every other mode passes straight through
// to UpsertStyleNodeClaim with identical behavior, including fail-loud on a
// blank mode. Engine test claim seeds route through this shim instead of
// calling UpsertStyleNodeClaim directly (a mechanical sed), so a single seam
// owns the legacy-simple accommodation.
func upsertClaimLegacySimple(db *store.DB, in processes.NodeClaimInput) (int64, error) {
	if in.SwapMode != protocol.SwapModeSimple {
		return db.UpsertStyleNodeClaim(in)
	}
	in.SwapMode = protocol.SwapModeSequential // placeholder to pass the allowlist
	id, err := db.UpsertStyleNodeClaim(in)
	if err != nil {
		return id, err
	}
	if _, err := db.DB.Exec(`UPDATE style_node_claims SET swap_mode='simple' WHERE id=?`, id); err != nil {
		return id, err
	}
	return id, nil
}
