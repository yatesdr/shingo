// Package loadervectors holds the checked-in cases that pin Core's loader
// replenishment decisions: which windows an inbound carrier may go to, how many
// carriers a loop needs, and where an unloader's full goes when a release
// offers it a part (Consume).
//
// They began as the equivalence between Core and the Edge, which answered the
// same questions from separate code; each side's tests read this file. The
// Edge's implementation went when every carrier a loader asks for became
// Core's to count and place (the loader retrieve ask), so the vectors pin the
// one implementation left, against the behaviour the plant actually ran.
//
// It lives in shared/ for the reason it began there: the one module both
// shingo-core and shingo-edge import (integration's sizing check reads it
// too). Embedding rather than reading a path keeps the tests independent of
// where they are run from, and makes a missing file a compile error instead of
// a skipped test.
//
// ADDING A CASE: add it here and Core must satisfy it. CHANGING an expected
// result is a behaviour change to the plant.
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

// ConsumeWindow is one window of an unloader as it stands before a consume
// case's releases arrive: what stands on it, and what is already on its way.
type ConsumeWindow struct {
	Node string `json:"node"`
	// StandingFull is the part of a full standing on the window, "" for none.
	StandingFull string `json:"standing_full,omitempty"`
	// StandingEmpty is an empty carrier standing on the window.
	StandingEmpty bool `json:"standing_empty,omitempty"`
	// InboundFull is the part of a live full-in already delivering to the
	// window, "" for none.
	InboundFull string `json:"inbound_full,omitempty"`
}

// ConsumeCase pins where an unloader's fulls go when releases offer it parts:
// the side cycle's full-in (U1), on a shared_window consume loader.
type ConsumeCase struct {
	Name string `json:"name"`
	// Why this case exists. Read it before changing an expectation.
	Why           string `json:"why"`
	FunnelWindows bool   `json:"funnel_windows,omitempty"`
	// Payloads is the unloader's payload set.
	Payloads []string `json:"payloads"`
	// Windows are the unloader's windows, in the loader's order, as they stand.
	Windows []ConsumeWindow `json:"windows"`
	// Events is the part each release offers, in order.
	Events []string `json:"events"`
	// Burst: the events arrive at once, each on its own goroutine, rather than
	// one after another. A burst case's answer must not depend on which wins.
	Burst bool `json:"burst,omitempty"`
	// WantNew is every full the events ordered: window -> part. Windows the
	// events ordered nothing to are absent.
	WantNew map[string]string `json:"want_new"`
}

// Vectors is the whole file.
type Vectors struct {
	Targets []TargetCase  `json:"targets"`
	Sizing  []SizingCase  `json:"sizing"`
	Consume []ConsumeCase `json:"consume"`
}

// Load decodes the embedded vectors. It returns an error rather than panicking
// so a caller in a test can fail with its own context.
func Load() (Vectors, error) {
	var v Vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		return Vectors{}, fmt.Errorf("decode loader golden vectors: %w", err)
	}
	if len(v.Targets) == 0 || len(v.Sizing) == 0 || len(v.Consume) == 0 {
		return Vectors{}, fmt.Errorf("loader golden vectors: got %d target, %d sizing and %d consume cases; an empty set would pass every test",
			len(v.Targets), len(v.Sizing), len(v.Consume))
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
