package service

import (
	"errors"
	"strings"
	"testing"
)

// TestFolderShadow_CountsTheFalsePositivesNotTheTruePositives pins the window's
// POLARITY, which is the one thing about an instrument that cannot be checked
// by reading it.
//
// ── THE DEFECT THIS EXISTS TO STOP COMING BACK ────────────────────────────
//
// The window's job is to measure how often the seven `BinID == nil` sites fire
// on a COORDINATOR, whose NULL bin_id is permanent and correct. Twelve of those
// read "Core degraded" for a whole rig run against zero real defects, and the
// count is what the stage-1 cutover is meant to quote.
//
// It counted the opposite. NoteFolderShadow took a binIDIsNil parameter and
// tested `binIDIsNil != ownsNoCargo`, which reads like a symmetric comparison
// of two answers — but all seven callers are inside their own nil branch and
// passed the constant `true`, so it reduced to `!ownsNoCargo`, and
// OrderOwnsNoCargo returns TRUE for a coordinator. So it tallied and shouted
// about the firings that were RIGHT and stayed silent on every one that was
// wrong.
//
// A number that means the reverse of its label is worse than no number: the
// window would have ended by justifying the opposite of the cutover, with
// evidence attached.
func TestFolderShadow_CountsTheFalsePositivesNotTheTruePositives(t *testing.T) {
	ResetFolderShadow()
	t.Cleanup(ResetFolderShadow)

	// A coordinator: owns legs, NULL bin_id is correct, the site was WRONG.
	NoteFolderShadow(FolderSiteDeliverySettle, 1, true, nil)
	// An ordinary order: no legs, NULL bin_id is a real fault, the site was RIGHT.
	NoteFolderShadow(FolderSiteDeliverySettle, 2, false, nil)
	NoteFolderShadow(FolderSiteDeliverySettle, 3, false, nil)

	if got := FolderShadowTally()[FolderSiteDeliverySettle]; got != 1 {
		t.Errorf("coordinator (false-positive) count = %d, want 1. This is the number the cutover "+
			"quotes; if it is 2 the polarity has inverted again and the window is measuring the "+
			"firings that were correct", got)
	}
	if got := FolderShadowOrdinary()[FolderSiteDeliverySettle]; got != 2 {
		t.Errorf("ordinary (true-positive) count = %d, want 2. The cutover has to KEEP these — a "+
			"predicate swap that dropped them too would trade a noisy guard for no guard", got)
	}
	if got := FolderShadowSampled()[FolderSiteDeliverySettle]; got != 3 {
		t.Errorf("sampled = %d, want 3", got)
	}
}

// TestFolderShadow_AnUnreadableRowIsNotAnAnswer: a read failure counts as a
// sample that could not be taken and lands in NEITHER column. Folding "I could
// not ask" into either one is how a window comes back clean because it went
// blind.
func TestFolderShadow_AnUnreadableRowIsNotAnAnswer(t *testing.T) {
	ResetFolderShadow()
	t.Cleanup(ResetFolderShadow)

	NoteFolderShadow(FolderSiteTagVerify, 7, false, errors.New("db down"))

	if got := FolderShadowTally()[FolderSiteTagVerify]; got != 0 {
		t.Errorf("coordinator count = %d, want 0 on a read failure", got)
	}
	if got := FolderShadowOrdinary()[FolderSiteTagVerify]; got != 0 {
		t.Errorf("ordinary count = %d, want 0 on a read failure — the isCoordinator argument is "+
			"undefined when the read failed and must not be believed", got)
	}
	if got := FolderShadowSampled()[FolderSiteTagVerify]; got != 1 {
		t.Errorf("sampled = %d, want 1 — the attempt happened and the window is one short here", got)
	}
}

// TestFolderShadow_UnsampledIsNotClean: a site nothing reached and a site that
// never fired wrongly both produce a zero, and those are opposite conclusions.
// The report has to make them look different, or the cutover reads "clean" off
// a path the run never exercised.
func TestFolderShadow_UnsampledIsNotClean(t *testing.T) {
	ResetFolderShadow()
	t.Cleanup(ResetFolderShadow)

	if got := FolderShadowLine(); got != "NO SITE SAMPLED YET" {
		t.Errorf("empty window line = %q, want the explicit no-sample sentence", got)
	}
	if r := FolderShadowReport(); !strings.Contains(r, "NO SITE WAS SAMPLED") {
		t.Errorf("empty window report does not say so:\n%s", r)
	}

	NoteFolderShadow(FolderSiteRecoveryReapply, 9, false, nil)
	r := FolderShadowReport()
	if !strings.Contains(r, "1 sampled") {
		t.Errorf("report omits the sampled count, so a zero cannot be read:\n%s", r)
	}
	if strings.Contains(FolderShadowLine(), "NO SITE SAMPLED") {
		t.Error("the line still claims nothing was sampled after a sample")
	}
}
