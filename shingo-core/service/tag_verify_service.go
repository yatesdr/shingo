package service

import (
	"shingocore/store"
)

// TagVerifyService exposes best-effort QR tag verification for an
// order's bin. Cross-aggregate by nature — looks up the order
// (orders), checks the bin's label (bins), and writes audit entries
// (audit). Tag mismatches never block orders; the service returns
// match=true on every "couldn't determine" branch and logs the
// outcome so dispatch can keep running while operators reconcile
// out-of-band.
//
// Phase 6.1 added this service as the canonical caller surface for
// VerifyTag, which was previously sitting at the top-level *store.DB
// (store/tag_verify.go). Cross-aggregate orchestrations live in the
// service layer post-Phase 6.0; the *store.DB shim stays only until
// Phase 6.4 migrates store/tag_verify_test.go onto this service.
type TagVerifyService struct {
	db *store.DB
}

// NewTagVerifyService constructs a TagVerifyService wrapping the
// shared *store.DB. The constructor takes nothing else — order,
// bin, and audit lookups all flow through the embedded *store.DB
// pending Phase 6.5's DTO extraction.
func NewTagVerifyService(db *store.DB) *TagVerifyService {
	return &TagVerifyService{db: db}
}

// VerifyTag performs the best-effort tag check.
//
// Branches:
//   - order not found             → match=true ("accepting scan")
//   - order has no bin assigned   → match=true ("accepting scan")
//   - bin not found               → match=true ("accepting scan")
//   - bin label empty (first scan)→ match=true, label is learned, audit "tag_scanned"
//   - bin label matches tag       → match=true, audit "tag_scanned"
//   - bin label differs from tag  → match=false (still proceeds), audit "tag_mismatch"
//
// Returns match=true even on mismatch/missing (best-effort: never
// blocks orders; downstream reconciliation handles inconsistencies).
//
// Phase 6.1 introduced this method as a thin delegate; Phase 6.4a
// moved the body in from the (now-deleted) outer
// store/tag_verify.go::VerifyTag.
func (s *TagVerifyService) VerifyTag(orderUUID, tagID, location string) *store.TagVerifyResult {
	order, err := s.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return &store.TagVerifyResult{Match: true, Detail: "order not found — accepting scan"}
	}

	if order.BinID == nil {
		return &store.TagVerifyResult{Match: true, Detail: "no bin assigned — accepting scan"}
	}

	bin, err := s.db.GetBin(*order.BinID)
	if err != nil {
		return &store.TagVerifyResult{Match: true, Detail: "bin not found — accepting scan"}
	}

	if bin.Label == "" {
		// Learn the tag on first scan by updating bin label.
		bin.Label = tagID
		s.db.UpdateBin(bin)
		s.db.AppendAudit("bin", bin.ID, "tag_scanned", "", "tag learned from scan: "+tagID, "system")
		return &store.TagVerifyResult{Match: true, Detail: "tag learned: " + tagID}
	}

	if bin.Label == tagID {
		s.db.AppendAudit("bin", bin.ID, "tag_scanned", "", "tag verified at "+location, "system")
		return &store.TagVerifyResult{Match: true, Detail: "tag match"}
	}

	// Tag mismatch — best-effort: log but proceed.
	s.db.AppendAudit("bin", bin.ID, "tag_mismatch", bin.Label, tagID, "system")
	return &store.TagVerifyResult{Match: false, Expected: bin.Label, Detail: "tag mismatch — proceeding (best-effort)"}
}
