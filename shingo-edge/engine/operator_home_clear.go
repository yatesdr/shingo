package engine

import (
	"fmt"
	"log"
	"net/http"

	"shingo/protocol"
)

// operator_home_clear.go — the station's clear of a dedicated loader's home.
//
// The Edge asks once and Core does the rest (engine/clear_home.go there): the
// checks (a pinned home of a dedicated loader, a carrier with parts on it, a
// buffer partial of the home's part), the orders — Core's compound parent and
// its plain moves, pushed back here as plain rows — and the zero of the home's
// carrier. The Edge creates no order and arms no intent. What it records is
// the zero, as every CLEAR does (carrierClearedInPlace), so the home reads
// known-empty under the carrier's new generation stamp at once.
//
// The home's runtime order pointer is NOT written: Core authored the orders,
// and no reader of that pointer decides anything for a loader home (the
// station's loader board draws the home's orders from its rows).

// HomeClearRefused is Core's one-sentence refusal of a home clear: the one
// live clear per home (409). The confirm shows it in place of its buttons.
type HomeClearRefused struct{ Sentence string }

func (r *HomeClearRefused) Error() string { return r.Sentence }

// HomeClearInvalid is Core's answer that the node is no home it can clear
// (400): not a dedicated loader's pinned home, no carrier with parts, no
// partial of its part that no other order holds, or no open spot and windows
// that cannot take the trade. Core is the one validator; the Edge does not
// pre-check.
type HomeClearInvalid struct{ Sentence string }

func (r *HomeClearInvalid) Error() string { return r.Sentence }

// The board's sentences for a recorded clear, one per arm Core answers. They
// share their opening; an arm this Edge does not know is told the opening
// alone, which is true whatever Core is doing (the zero is recorded).
const (
	homeClearSaid     = "Full recorded"
	homeClearRecorded = homeClearSaid + " · "
	homeClearSpot     = homeClearRecorded + "Shingo takes the empty to a buffer spot and brings the partial home"
	homeClearDemand   = homeClearRecorded + "a waiting empty pull takes the bin and Shingo brings the partial home"
	// HomeClearTrade is the trade's: no pull and no open spot, so two robots
	// each lift one bin and set it where the other lifted.
	HomeClearTrade = homeClearRecorded + "two robots trade the empty and the partial"
)

// homeClearSentence is the board's sentence for the arm Core chose.
func homeClearSentence(arm string) string {
	switch arm {
	case protocol.ClearArmSpot:
		return homeClearSpot
	case protocol.ClearArmDemand:
		return homeClearDemand
	case protocol.ClearArmTrade:
		return HomeClearTrade
	}
	return homeClearSaid
}

// ClearLoaderHome asks Core to clear the home at nodeID and returns the
// board's one sentence for what Core did.
func (e *Engine) ClearLoaderHome(nodeID int64) (string, error) {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return "", err
	}
	ans, status, err := e.coreClient.ClearHome(protocol.ClearHomeRequest{
		HomeNode: node.CoreNodeName, Station: e.cfg.StationID()})
	if err != nil {
		return "", fmt.Errorf("clear home %s: %w", node.Name, err)
	}
	// The zero stands whenever Core made it (every confirm past the fence and
	// its checks, whatever arm): record it.
	if ans.DeltaEpoch != 0 {
		if err := e.carrierClearedInPlace(nodeID, node.CoreNodeName, persistedClaimID(claim), ans.BinID, ans.DeltaEpoch); err != nil {
			log.Printf("home_clear: record the zero at node %d: %v", nodeID, err)
		}
	}
	if status != http.StatusOK || ans.Status != "ok" {
		// The refusal redraws the home (and the spot) from Core's rows before
		// its sentence.
		e.ApplyNodeBinsRows(ans.Rows)
		switch {
		case status == http.StatusConflict && ans.Error != "":
			return "", &HomeClearRefused{Sentence: ans.Error}
		case status == http.StatusBadRequest && ans.Error != "":
			return "", &HomeClearInvalid{Sentence: ans.Error}
		}
		return "", fmt.Errorf("clear home %s: %s", node.Name, coreErrorText("", ans.Error, status))
	}
	log.Printf("home_clear: node %s cleared by Core (%s arm, job %s)", node.Name, ans.Arm, ans.ParentUUID)
	return homeClearSentence(ans.Arm), nil
}
