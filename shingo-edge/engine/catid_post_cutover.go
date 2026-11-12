// catid_post_cutover.go — post-cutover CATID verification.
//
// Within a short window after any cutover completes (operator-started, PLC, or
// auto-armed), the CATID monitor checks whether the press's live part id matches
// the new active style's expected_catid. If it still disagrees at the end of the
// window, the changeover is flagged for operator confirmation on the station —
// "set to style A, but the press reports style B (or nothing)". It never blocks
// beyond the existing request-path mismatch guard; it is a confirmation prompt.
// It clears when the press later reports a part of style A (the die changed
// after the robots finished), and the station can Dismiss it.

package engine

import (
	"fmt"
	"log"
	"strings"
	"time"

	"shingo/protocol/clock"
)

// postCutoverVerifyWindow is how long after a cutover completes the monitor
// watches the press's live part id before flagging a disagreement. Var so tests
// can compress it.
var postCutoverVerifyWindow = 2 * time.Minute

// openPostCutoverVerify starts the post-cutover watch for a just-completed
// changeover on a monitored process. It opens with the new active style's set
// EMPTY too: at its close it records the PLC's part on the row (end_catid),
// the evidence a pin is suggested from (suggestCATIDPin); it flags only a
// non-empty set the part is not in.
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
	e.catidMon.openPostCutoverVerify(processID, changeoverID, style.ID, e.styleCATIDSet(style),
		clock.Now().Add(postCutoverVerifyWindow))
}

// liveIdentity is the monitor's confirmed CATID for the process when it names
// a part.
func (e *Engine) liveIdentity(processID int64) (string, bool) {
	if e.catidMon == nil {
		return "", false
	}
	return e.catidMon.liveCATID(processID)
}

// confirmedCATID is liveIdentity read from the monitor's published snapshot,
// "" when it names no part: lock-free, for a caller under countMu, which
// ranks above the monitor's lock (the lift's logged strokes).
func (e *Engine) confirmedCATID(processID int64) string {
	if e.catidMon == nil {
		return ""
	}
	snap, ok := e.catidMon.snapshot(processID)
	if !ok || !isIdentity(snap.value) {
		return ""
	}
	return snap.value
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
	// CanUpdateCATID is whether the flag's Update CATID (ConfirmPostCutoverFlag)
	// would land: the live value names a part and no other style of the process
	// runs it (CheckStyleCATIDPin, the check the Update makes). The station shows
	// the button only then: a part another style runs is that style's, and the
	// Update would be refused (HK P400 2026-11-11, the KK21 part offered to LK41).
	CanUpdateCATID bool   `json:"can_update_catid"`
	Message        string `json:"message"`
}

// PostCutoverFlag returns the process's active post-cutover verification flag, or
// (nil, false) when none. ExpectedStyle is the style the changeover was set to;
// the live part is resolved against every style's CATID set — exactly one match
// names the style to change over to, more than one names the candidates, none
// leaves the part free for Update CATID (CanUpdateCATID). Message is the
// operator prompt.
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
	msg := fmt.Sprintf("This changeover was set to %s, but the process is reporting %s",
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
	flag.CanUpdateCATID = isIdentity(co.VerifyLiveCATID) &&
		e.CheckStyleCATIDPin(processID, co.ToStyleID, co.VerifyLiveCATID) == nil
	return flag, true
}

// ConfirmPostCutoverFlag is the flag's Confirm: the operator says the part the
// PLC reports IS the changeover's to-style's. The flagged value joins the
// to-style's pin (AddStyleCATID: its set plus the value, refused when another
// style holds it — then the corrective changeover the flag offers is the way),
// and the flag clears. No-op when nothing is flagged.
func (e *Engine) ConfirmPostCutoverFlag(processID int64) error {
	co, err := e.db.LatestFlaggedChangeover(processID)
	if err != nil || co == nil {
		return err
	}
	if isIdentity(co.VerifyLiveCATID) {
		if err := e.AddStyleCATID(processID, co.ToStyleID, co.VerifyLiveCATID, "operator",
			fmt.Sprintf("confirmed the post-cutover flag on changeover %d", co.ID)); err != nil {
			return err
		}
	}
	return e.ClearPostCutoverFlag(processID)
}

// ClearPostCutoverFlag clears the process's post-cutover verification flag
// without writing any pin: a new changeover resolves it (the operator is acting
// on the mismatch). No-op when nothing is flagged.
func (e *Engine) ClearPostCutoverFlag(processID int64) error {
	co, err := e.db.LatestFlaggedChangeover(processID)
	if err != nil || co == nil {
		return err
	}
	return e.clearFlagOn(processID, co.ID)
}

// clearFlagOn is every clear's one write: the flag off changeover coID, and
// the event that takes the banner down on every station of the process.
func (e *Engine) clearFlagOn(processID, coID int64) error {
	if err := e.db.SetChangeoverVerifyMismatch(coID, ""); err != nil {
		return err
	}
	if e.Events != nil {
		e.Events.Emit(Event{Type: EventChangeoverVerifyCleared,
			Payload: CATIDVerifyClearedEvent{ProcessID: processID, ChangeoverID: coID}})
	}
	return nil
}

// DismissPostCutoverFlag is the flag's Dismiss: the operator has seen it, and
// it comes down on every screen of the process, writing no pin and starting
// nothing. Only the prompt goes: the request guard (guardCatidMismatch) still
// refuses material for a part the running style does not run. No-op when
// nothing is flagged.
func (e *Engine) DismissPostCutoverFlag(processID int64) error {
	co, err := e.db.LatestFlaggedChangeover(processID)
	if err != nil || co == nil {
		return err
	}
	e.logFn("post-cutover flag on changeover %d dismissed at the station: set to %s, the process reported %s",
		co.ID, e.styleName(co.ToStyleID), co.VerifyLiveCATID)
	return e.clearFlagOn(processID, co.ID)
}

// clearAnsweredPostCutoverFlag clears a standing flag the press has answered:
// the PLC now reports liveCATID, a part of styleID, and the flag is on a
// changeover to styleID. The watch that raises a flag closes when it flags, so
// before this a press changed after shingo's cutover (the robots done before
// the die change) kept its banner until someone confirmed a part that was
// never wrong, and Confirm is refused when that part is the old style's (HK
// P400 2026-11-11). Called by the monitor on a confirm edge, with cm.mu held,
// as the watch's own clear is.
func (e *Engine) clearAnsweredPostCutoverFlag(processID, styleID int64, liveCATID string) {
	co, err := e.db.LatestFlaggedChangeover(processID)
	if err != nil || co == nil || co.ToStyleID != styleID {
		return
	}
	if err := e.clearFlagOn(processID, co.ID); err != nil {
		log.Printf("plc-catid: clear the answered post-cutover flag on changeover %d: %v", co.ID, err)
		return
	}
	log.Printf("CATID post-cutover flag on changeover %d cleared: process %d now reports %s, a part of style %d",
		co.ID, processID, liveCATID, styleID)
}
