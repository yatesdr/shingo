// Package comingvectors holds the checked-in cases that pin "a carrier is
// coming to X" across the wire: every leg shape the Edge's step builders write,
// and the plain orders, each with its protocol.OrderPlacesBinAt answer at every
// node the leg names.
//
// The Edge writes the file from its own builders (shingo-edge/engine
// TestComingVectors_AreTheBuildersShapes, which fails when the file is stale and
// says how to regenerate it); Core reads it in a docker test over stored rows
// (shingo-core/store TestComingVectors_CoreAgreesOverStoredRows), so Core's one
// statement (orders.CarrierComingTo: its SQL prefilter and its Go decision)
// gives every answer the Edge's builders imply.
//
// It lives in shared/ for the reason loadervectors does: the one module both
// shingo-core and shingo-edge import. Embedding rather than reading a path
// keeps both tests independent of where they run, and makes a missing file a
// compile error instead of a skipped test. A leg's steps are kept as the JSON
// the Edge's builders wrote and Core stores as steps_json, so this package
// needs no protocol import (shared builds alone, GOWORK=off).
package comingvectors

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed vectors.json
var raw []byte

// Case is one leg: its shape's name, how it is stored (its order type, its
// steps, its delivery node) and, for every node it names, whether it leaves a
// carrier there.
type Case struct {
	Name         string          `json:"name"`
	OrderType    string          `json:"order_type"`
	Steps        json.RawMessage `json:"steps,omitempty"`
	DeliveryNode string          `json:"delivery_node,omitempty"`
	Places       map[string]bool `json:"places"`
}

// Cases is the checked-in file, decoded.
func Cases() ([]Case, error) {
	var out []Case
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("comingvectors: %w", err)
	}
	return out, nil
}

// Raw is the checked-in file as bytes, for the Edge's staleness check.
func Raw() []byte { return raw }
