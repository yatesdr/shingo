//go:build docker

package www

import (
	"testing"

	"shingocore/store/loaders"
)

// TestApiCreateLoader_KeepsTheKindTheOperatorChose
//
// The loader form asks what KIND of loader this is — single window, multi
// window, or dedicated homes — and it asks it first, as the question that
// decides which other questions appear. "Single window" is funnel_windows:
// one window filled at a time rather than carriers spread across all of them.
//
// The client sends it on create. loaderPayload derives
// `funnel_windows: state.kind === 'single_window'` and its own comment calls
// itself "the one place state becomes a request body, so create and edit
// cannot drift on what they send." They do not drift on what they send. The
// SERVER drifted on what it read: apiCreateLoader's request struct had no
// such field, so the value was decoded into nothing and the loader was
// created spread.
//
// What the operator saw: answer the headline question with "Single window",
// press Create, and the modal reopens — it re-reads the created loader — now
// showing "Multi window". No error, no warning. The only way to get what they
// asked for was to notice the flip and save a second time through Update,
// which does accept the field.
//
// The asymmetry was defended on the grounds that a loader is created before
// its windows are dragged in, so at creation there is nothing to funnel. That
// was true when the control was a checkbox halfway down a form about members.
// It stopped being true when the kind became the first question, because the
// kind is a property of the loader, not of its members — the same way layout
// is, which create has always accepted.
func TestApiCreateLoader_KeepsTheKindTheOperatorChose(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	rr := postJSON(t, h.apiCreateLoader, "/api/loader/create", map[string]any{
		"name":          "FUNNEL-CREATE",
		"role":          "produce",
		"layout":        "shared_window",
		"replenishment": "operator",
		// The operator answered "Single window".
		"funnel_windows": true,
	})
	if rr.Code != 200 {
		t.Fatalf("create returned %d, body=%s", rr.Code, rr.Body.String())
	}

	got, err := loaders.GetLoaderByName(db.DB, "FUNNEL-CREATE", "produce")
	if err != nil || got == nil {
		t.Fatalf("read back the created loader: %v", err)
	}
	if !got.FunnelWindows {
		t.Errorf("funnel_windows = false, want true — the operator chose Single window on the "+
			"form's first question, the client sent it, and create dropped it. The loader they "+
			"just made spreads carriers across every window instead of filling one at a time, "+
			"and the form reopens showing Multi window with no error (loader id=%d)", got.ID)
	}
}

// The other half of the same round trip: NOT choosing single window has to keep
// meaning multi window. A create path that accepted the field but defaulted it
// wrong would pass the test above and still be broken, in the direction that
// affects every loader anyone has ever made rather than the ones made since.
func TestApiCreateLoader_DefaultsToSpreadWhenTheKindIsNotSingleWindow(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	rr := postJSON(t, h.apiCreateLoader, "/api/loader/create", map[string]any{
		"name":          "SPREAD-CREATE",
		"role":          "produce",
		"layout":        "shared_window",
		"replenishment": "operator",
		// funnel_windows deliberately absent, which is what every client that
		// predates the field sends, and what "Multi window" sends today.
	})
	if rr.Code != 200 {
		t.Fatalf("create returned %d, body=%s", rr.Code, rr.Body.String())
	}

	got, err := loaders.GetLoaderByName(db.DB, "SPREAD-CREATE", "produce")
	if err != nil || got == nil {
		t.Fatalf("read back the created loader: %v", err)
	}
	if got.FunnelWindows {
		t.Errorf("funnel_windows = true, want false — an absent field must mean spread, which "+
			"is what every loader at every plant is today (loader id=%d)", got.ID)
	}
}
