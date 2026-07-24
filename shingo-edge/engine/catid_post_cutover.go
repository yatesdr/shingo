// catid_post_cutover.go — post-cutover CATID verification.
//
// Within a short window after any cutover completes (operator-started, PLC, or
// auto-armed), the CATID monitor checks whether the press's live part id matches
// the new active style's expected_catid. If it still disagrees at the end of the
// window, the changeover is flagged for operator confirmation on the station —
// "set to style A, but the press reports style B (or nothing)". It never blocks
// beyond the existing request-path mismatch guard; it is a confirmation prompt.

package engine

import (
	"fmt"
	"time"
)

// postCutoverVerifyWindow is how long after a cutover completes the monitor
// watches the press's live part id before flagging a disagreement. Var so tests
// can compress it.
var postCutoverVerifyWindow = 2 * time.Minute

// openPostCutoverVerify starts the post-cutover watch for a just-completed
// changeover. Silent (no watch) when the new active style has no expected_catid —
// there is nothing to verify against.
func (e *Engine) openPostCutoverVerify(processID, changeoverID int64) {
	if e.catidMon == nil {
		return
	}
	proc, err := e.db.GetProcess(processID)
	if err != nil || proc == nil || proc.ActiveStyleID == nil {
		return
	}
	style, err := e.db.GetStyle(*proc.ActiveStyleID)
	if err != nil || style == nil || style.ExpectedCATID == "" {
		return
	}
	e.catidMon.openPostCutoverVerify(processID, changeoverID, style.ID, style.ExpectedCATID,
		time.Now().Add(postCutoverVerifyWindow))
}

// PostCutoverFlag is the operator-facing post-cutover verification flag for a
// process.
type PostCutoverFlag struct {
	ChangeoverID      int64  `json:"changeover_id"`
	ProcessID         int64  `json:"process_id"`
	ExpectedStyleID   int64  `json:"expected_style_id"`
	ExpectedStyleName string `json:"expected_style_name"`
	LiveCATID         string `json:"live_catid"`
	MappedStyleID     int64  `json:"mapped_style_id,omitempty"`
	MappedStyleName   string `json:"mapped_style_name,omitempty"`
	HasMapped         bool   `json:"has_mapped"`
	Message           string `json:"message"`
}

// PostCutoverFlag returns the process's active post-cutover verification flag, or
// (nil, false) when none. ExpectedStyle is the style the changeover was set to;
// MappedStyle is the style the live part id actually matches when it maps to one
// (enabling a one-tap corrective changeover). Message is the operator prompt.
func (e *Engine) PostCutoverFlag(processID int64) (*PostCutoverFlag, bool) {
	co, err := e.db.LatestFlaggedChangeover(processID)
	if err != nil || co == nil {
		return nil, false
	}
	expectedName := e.styleName(co.ToStyleID)
	mappedID, mappedName, hasMapped := e.styleForCATID(processID, co.VerifyLiveCATID)

	msg := fmt.Sprintf("This changeover was set to %s, but the press is reporting %s",
		expectedName, co.VerifyLiveCATID)
	if hasMapped {
		msg += fmt.Sprintf(" (%s)", mappedName)
	}
	msg += " — please confirm."

	return &PostCutoverFlag{
		ChangeoverID:      co.ID,
		ProcessID:         processID,
		ExpectedStyleID:   co.ToStyleID,
		ExpectedStyleName: expectedName,
		LiveCATID:         co.VerifyLiveCATID,
		MappedStyleID:     mappedID,
		MappedStyleName:   mappedName,
		HasMapped:         hasMapped,
		Message:           msg,
	}, true
}

// ClearPostCutoverFlag clears the process's post-cutover verification flag — the
// operator confirmed or resolved it. No-op when nothing is flagged.
func (e *Engine) ClearPostCutoverFlag(processID int64) error {
	co, err := e.db.LatestFlaggedChangeover(processID)
	if err != nil || co == nil {
		return err
	}
	return e.db.SetChangeoverVerifyMismatch(co.ID, "")
}
