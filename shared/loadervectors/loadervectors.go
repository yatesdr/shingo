// Package loadervectors holds the checked-in cases that pin Core's loader
// replenishment decisions against the Edge's.
//
// Two subsystems now answer the same two questions — which windows an inbound
// carrier may go to, and how many carriers a loop needs — and they answer them
// from separate code in separate modules. The vectors are what stops those two
// answers drifting: each module's tests read this file and assert its own
// implementation against the same expected results.
//
// THE VECTORS ARE THE PERMANENT GATE, not a migration aid. The obvious
// alternative — a live test that runs both implementations and compares them —
// dies in the commit it is meant to protect, because the cutover deletes the
// Edge implementation. Vectors outlive it: after the Edge path is gone they go
// on pinning Core against the behaviour the plant actually ran.
//
// It lives in shared/ because it is the one module both shingo-core and
// shingo-edge already import. Embedding rather than reading a path keeps both
// tests independent of where they are run from, and makes a missing file a
// compile error instead of a skipped test.
//
// ADDING A CASE: add it here and both sides must satisfy it. CHANGING an
// expected result is a behaviour change to the plant and needs both
// implementations changed with it — that is the point of the file.
package loadervectors

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed vectors.json
var raw []byte

// Home is one member of a loader: a window of a shared loader, or a dedicated
// position. Mirrors what both sides store.
type Home struct {
	Node      string `json:"node"`
	Payload   string `json:"payload,omitempty"`
	SortOrder int    `json:"sort_order,omitempty"`
	Kind      string `json:"kind,omitempty"` // "" or "home" or "buffer"
}

// SharedPayload is one entry in a shared loader's allowed payload set.
type SharedPayload struct {
	Code         string `json:"code"`
	UOPThreshold int    `json:"uop_threshold,omitempty"`
}

// LoaderShape is the loader config a target case is evaluated against.
type LoaderShape struct {
	Layout        string          `json:"layout"`
	FunnelWindows bool            `json:"funnel_windows,omitempty"`
	Homes         []Home          `json:"homes"`
	PayloadSet    []SharedPayload `json:"payload_set,omitempty"`
}

// TargetCase pins one delivery-target answer.
type TargetCase struct {
	Name string `json:"name"`
	// Why this case exists. Read it before changing an expectation.
	Why    string      `json:"why"`
	Loader LoaderShape `json:"loader"`
	// Member is the specific member node the triggering signal named, or "".
	Member string `json:"member,omitempty"`
	// Payload being replenished; "" is the payload-agnostic case.
	Payload string `json:"payload"`
	// WantNodes is the delivery set, IN ORDER. Order matters: the funnel case
	// takes the first, and the spreading case fills free windows in this order.
	WantNodes []string `json:"want_nodes"`
	// WantBudget is how many carriers may be inbound across that set at once.
	WantBudget int `json:"want_budget"`
	// CoreOnly marks a case the Edge cannot express, with the reason. The Edge's
	// constructors refuse some configurations outright, so those shapes can only
	// be asked of Core. The Edge test skips them and reports the reason rather
	// than silently passing.
	CoreOnly string `json:"core_only,omitempty"`
}

// SizingCase pins one "how many carriers" answer.
type SizingCase struct {
	Name           string `json:"name"`
	Why            string `json:"why"`
	Threshold      int    `json:"threshold"`
	CurrentUOP     int    `json:"current_uop"`
	PerBinCapacity int    `json:"per_bin_capacity"`
	WantBins       int    `json:"want_bins"`
	// WantOutcome is "ok", "at_threshold", or "no_per_bin_capacity".
	WantOutcome string `json:"want_outcome"`
}

// Vectors is the whole file.
type Vectors struct {
	Targets []TargetCase `json:"targets"`
	Sizing  []SizingCase `json:"sizing"`
}

// Load decodes the embedded vectors. It returns an error rather than panicking
// so a caller in a test can fail with its own context.
func Load() (Vectors, error) {
	var v Vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		return Vectors{}, fmt.Errorf("decode loader golden vectors: %w", err)
	}
	if len(v.Targets) == 0 || len(v.Sizing) == 0 {
		return Vectors{}, fmt.Errorf("loader golden vectors: got %d target and %d sizing cases; an empty set would pass every test",
			len(v.Targets), len(v.Sizing))
	}
	return v, nil
}

// MustLoad is Load for tests that have nothing useful to add to the error.
func MustLoad() Vectors {
	v, err := Load()
	if err != nil {
		panic(err)
	}
	return v
}
