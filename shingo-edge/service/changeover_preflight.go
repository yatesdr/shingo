// Edge-side preflight inventory gate. Builds the to-style's required
// payload list from style_node_claims, calls Core's preflight endpoint,
// and returns the missing payload list so StartProcessChangeover can
// refuse to begin a changeover when bins are absent.

package service

import (
	"context"
	"fmt"

	"shingoedge/store"
)

// PreflightCorePoster is the narrow interface PreflightChecker requires
// from the Core HTTP client. Held as an interface (not the concrete
// *engine.CoreClient) to break the engine→service→engine import cycle:
// service is consumed by engine, and engine wires the concrete client
// into PreflightChecker at construction time.
type PreflightCorePoster interface {
	Available() bool
	PreflightInventory(station string, payloads []string) (*PreflightCoreResult, error)
}

// PreflightCoreResult mirrors the wire shape of engine.PreflightResult.
// Defined here so the interface above doesn't drag the engine package
// back into service.
type PreflightCoreResult struct {
	Missing   []string
	Available []PreflightCoreAvailability
}

// PreflightCoreAvailability mirrors engine.PreflightAvailability.
type PreflightCoreAvailability struct {
	PayloadCode string
	BinCount    int
}

// PreflightChecker holds the dependencies needed to gate a changeover on
// upstream bin availability.
type PreflightChecker struct {
	db         *store.DB
	coreClient PreflightCorePoster
	station    string
}

// NewPreflightChecker constructs a PreflightChecker.
func NewPreflightChecker(db *store.DB, coreClient PreflightCorePoster, station string) *PreflightChecker {
	return &PreflightChecker{db: db, coreClient: coreClient, station: station}
}

// PreflightInventoryCheck collects the to-style's required payload codes
// (skipping the empty-bin sentinel "__empty__") and asks Core whether each
// has at least one available bin in the supermarket. Returns the missing
// subset.
//
// nil missing slice + nil error means everything is available. A non-empty
// missing list is the operator-visible diagnostic.
//
// If Core is unavailable the call returns an error rather than degrading
// to "all available" — a preflight that silently passes when the source
// of truth is unreachable defeats the gate.
func (p *PreflightChecker) PreflightInventoryCheck(ctx context.Context, toStyleID int64) ([]string, error) {
	if p.coreClient == nil || !p.coreClient.Available() {
		return nil, fmt.Errorf("preflight: core API not configured")
	}
	claims, err := p.db.ListStyleNodeClaims(toStyleID)
	if err != nil {
		return nil, fmt.Errorf("preflight: list claims: %w", err)
	}
	seen := make(map[string]struct{}, len(claims))
	payloads := make([]string, 0, len(claims))
	for _, c := range claims {
		code := c.PayloadCode
		if code == "" || code == "__empty__" {
			continue
		}
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		payloads = append(payloads, code)
	}
	if len(payloads) == 0 {
		return nil, nil
	}
	result, err := p.coreClient.PreflightInventory(p.station, payloads)
	if err != nil {
		return nil, fmt.Errorf("preflight: core call: %w", err)
	}
	return result.Missing, nil
}
