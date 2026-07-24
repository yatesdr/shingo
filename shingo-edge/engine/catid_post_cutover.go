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
	"strings"
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
	if err != nil || style == nil {
		return
	}
	if len(e.styleCATIDSet(style)) == 0 {
		return // no configured/derivable parts ⇒ nothing to verify against
	}
	e.catidMon.openPostCutoverVerify(processID, changeoverID, style.ID,
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
	// MappedStyle is set only when the live part maps to EXACTLY ONE style, so the
	// station can offer a one-tap corrective changeover. When the part maps to more
	// than one, Candidates names them and HasMapped is false.
	MappedStyleID   int64            `json:"mapped_style_id,omitempty"`
	MappedStyleName string           `json:"mapped_style_name,omitempty"`
	HasMapped       bool             `json:"has_mapped"`
	Candidates      []CATIDCandidate `json:"candidates,omitempty"`
	Message         string           `json:"message"`
}

// PostCutoverFlag returns the process's active post-cutover verification flag, or
// (nil, false) when none. ExpectedStyle is the style the changeover was set to;
// the live part is resolved against every style's CATID set — exactly one match
// gives a one-tap corrective changeover, more than one names the candidates, none
// points the operator at the part-id config. Message is the operator prompt.
func (e *Engine) PostCutoverFlag(processID int64) (*PostCutoverFlag, bool) {
	co, err := e.db.LatestFlaggedChangeover(processID)
	if err != nil || co == nil {
		return nil, false
	}
	expectedName := e.styleName(co.ToStyleID)
	matches := e.stylesForCATID(processID, co.VerifyLiveCATID)

	flag := &PostCutoverFlag{
		ChangeoverID:      co.ID,
		ProcessID:         processID,
		ExpectedStyleID:   co.ToStyleID,
		ExpectedStyleName: expectedName,
		LiveCATID:         co.VerifyLiveCATID,
	}
	msg := fmt.Sprintf("This changeover was set to %s, but the press is reporting %s",
		expectedName, co.VerifyLiveCATID)
	switch len(matches) {
	case 1:
		flag.HasMapped = true
		flag.MappedStyleID = matches[0].ID
		flag.MappedStyleName = matches[0].Name
		msg += fmt.Sprintf(" (%s)", matches[0].Name)
	case 0:
		// maps to no configured style — the operator reviews the part-id config
	default:
		flag.Candidates = make([]CATIDCandidate, len(matches))
		for i, m := range matches {
			flag.Candidates[i] = CATIDCandidate{StyleID: m.ID, StyleName: m.Name}
		}
		msg += fmt.Sprintf(" (matches %s)", strings.Join(matchNames(matches), " or "))
	}
	msg += " — please confirm."
	flag.Message = msg
	return flag, true
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
