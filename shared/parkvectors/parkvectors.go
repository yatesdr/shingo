// Package parkvectors holds every leg the Edge's step builders write for a line
// whose routing runs through a dedicated loader's home, so Core's loader park
// can be pinned against the legs themselves rather than hand copies of them.
//
// The Edge writes the file from its own builders (shingo-edge/engine
// TestParkVectors_AreTheBuildersShapes, which fails when the file is stale and
// says how to regenerate it). Core runs each case through intake and a scan
// pass and pins where the park lands it (shingo-core/dispatch
// TestLoaderPark_EveryBuilderLeg, docker).
//
// Node names are roles, not plant nodes: LINE (the claim's own node, the
// process node every leg names), PAIR (an index press's back position), HOME
// (the line's part's pinned home on the loader), HOME-B (the incoming part's
// home on the same loader), STAGE and OSTAGE (inbound and outbound staging,
// not loader members), OUT (an outbound that is not a loader member), MARKET
// and MARKET-B (markets for the two parts, not loader members). Core maps each
// role onto its fixture's node.
//
// It lives in shared/ for the reason comingvectors does: the one module both
// shingo-core and shingo-edge import, embedded so a missing file is a compile
// error. A leg's steps are kept as the JSON the builders wrote, so this package
// needs no protocol import.
package parkvectors

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed vectors.json
var raw []byte

// Case is one leg as the Edge sends it: the routing it was built from (Config),
// its mode and phase, which leg it is, the order's payload, the process node,
// its partner's case name when the Edge sends it as one of a pair, and its
// steps.
type Case struct {
	Name        string          `json:"name"`
	Config      string          `json:"config"`
	Mode        string          `json:"mode"`
	Phase       string          `json:"phase"`
	Leg         string          `json:"leg"`
	PayloadCode string          `json:"payload_code"`
	ProcessNode string          `json:"process_node"`
	Sibling     string          `json:"sibling,omitempty"`
	Steps       json.RawMessage `json:"steps"`
}

// Cases is the checked-in file, decoded.
func Cases() ([]Case, error) {
	var out []Case
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parkvectors: %w", err)
	}
	return out, nil
}

// Raw is the checked-in file as bytes, for the Edge's staleness check.
func Raw() []byte { return raw }
