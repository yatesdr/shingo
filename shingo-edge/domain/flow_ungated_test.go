package domain

import (
	"reflect"
	"testing"
)

// flow_ungated_test.go — a partial claim write speaks every unconditional
// column and no gated one.
//
// THE FAILURE THIS GUARDS IS SILENT AND DESTRUCTIVE IN BOTH DIRECTIONS. A
// caller that edits three columns of a stored claim has to echo the
// unconditional ones, because an empty value there is written as empty — miss
// one and a reorder-point edit blanks a press's part or its destination. And
// it must NOT speak the pointer-gated ones, because those are
// absent-means-untouched — echo one and the same edit reverts whatever another
// writer put there. The replenishment page's reorder edit got both wrong at
// different times, from one hand-kept copy of the column list.

// TestInputFromClaimUngated_SpeaksNoGatedColumn: every pointer field is nil.
//
// BY REFLECTION, not by a list. A list is the thing that was forgotten; this
// fails the day a gated column is added to NodeClaimInput and not to
// InputFromClaimUngated, which is exactly when it needs to.
func TestInputFromClaimUngated_SpeaksNoGatedColumn(t *testing.T) {
	t.Parallel()
	// A claim with every gated column set, so nil out means "deliberately
	// dropped" and not "was empty anyway".
	c := NodeClaim{
		StyleID: 7, CoreNodeName: "PLN_01", PayloadCode: "PIA26",
		InboundSource: "SMN", OutboundDestination: "SMN_DST",
		ChangeoverEvacNodes: []string{"PLN_02"}, ChangeoverEvacDestination: "SMN_EVAC",
		ChangeoverCarryoverDisposition: CarryoverKeepLineside, IndexRobotSupplies: true,
		KeyRoute: []string{"LM10"}, KeyTask: "load", KeepStaged: true,
		ReorderPointSource: "manual", AutoReorder: true, Sequence: 4,
	}
	in := InputFromClaimUngated(c)

	v := reflect.ValueOf(in)
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if v.Field(i).Kind() != reflect.Ptr {
			continue
		}
		if !v.Field(i).IsNil() {
			t.Errorf("%s is spoken by InputFromClaimUngated; it is an absent-means-untouched column, "+
				"so a partial write that echoes it reverts whatever another writer put there. "+
				"Add it to the nil list in InputFromClaimUngated.", f.Name)
		}
	}
}

// TestInputFromClaimUngated_SpeaksEveryUnconditionalColumn: the columns that
// are NOT gated all carry the stored value.
//
// Also by reflection, and this is the half that catches the other failure: a
// column added to NodeClaimInput as unconditional and left out of the echo is
// a column a partial write BLANKS. Deriving from InputFromClaim is what makes
// this pass by construction; the test is what says so out loud.
func TestInputFromClaimUngated_SpeaksEveryUnconditionalColumn(t *testing.T) {
	t.Parallel()
	c := NodeClaim{
		StyleID: 7, CoreNodeName: "PLN_01", Role: "produce", SwapMode: "two_robot",
		PayloadCode: "PIA26", ReorderPoint: 12, InboundStaging: "PLN_02",
		OutboundStaging: "PLN_05", InboundSource: "SMN", OutboundDestination: "SMN_DST",
		AllowedPayloadCodes: []string{"PIA26"}, AutoRequestPayload: "PIA26",
		EvacuateOnChangeover: true, PairedCoreNode: "PLN_02", SecondPairedCoreNode: "PLN_03",
		AutoConfirm: true, LinesideSoftThreshold: 3, ReuseCompatibleBins: true, AutoPush: true,
		Source: ClaimSourceHMI, CalledBy: "Press 400",
	}
	full, ungated := InputFromClaim(c), InputFromClaimUngated(c)

	fv, uv := reflect.ValueOf(full), reflect.ValueOf(ungated)
	for i := 0; i < fv.NumField(); i++ {
		f := fv.Type().Field(i)
		if fv.Field(i).Kind() == reflect.Ptr {
			continue // the gated half; the test above owns it
		}
		if !reflect.DeepEqual(fv.Field(i).Interface(), uv.Field(i).Interface()) {
			t.Errorf("%s is %v on a full write and %v on a partial one — an unconditional column, "+
				"so a partial write that does not echo it writes it EMPTY",
				f.Name, fv.Field(i).Interface(), uv.Field(i).Interface())
		}
	}
}
