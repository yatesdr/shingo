package engine

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"shingo/protocol"
	"shingoedge/store/processes"
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
	req := protocol.ClearHomeRequest{HomeNode: node.CoreNodeName, Station: e.cfg.StationID()}
	ans, status, err := e.sendHomeClear(req, func(late *protocol.ClearHomeResponse, lateStatus int) {
		if sentence, lerr := e.afterHomeClear(nodeID, node, claim, late, lateStatus); lerr != nil {
			log.Printf("home_clear: node %s, answered late: %v", node.Name, lerr)
		} else {
			log.Printf("home_clear: node %s, answered late: %s", node.Name, sentence)
		}
	})
	if err != nil {
		return "", fmt.Errorf("clear home %s: %w", node.Name, err)
	}
	return e.afterHomeClear(nodeID, node, claim, ans, status)
}

// sendHomeClear is the home clear's one send: an ask (clear_home, which Core
// answers once per envelope and replays on a repeat) when the last ack names
// it, the clear-home door otherwise, for one release. Either way the answer is
// the door's body at its status. ErrAskPending past AskWait; late then takes
// the answer when it comes, in this process.
func (e *Engine) sendHomeClear(req protocol.ClearHomeRequest, late func(*protocol.ClearHomeResponse, int)) (*protocol.ClearHomeResponse, int, error) {
	if !e.CoreAnswers(protocol.AskClearHome) {
		return e.coreClient.ClearHome(req)
	}
	a, err := e.AskLate(protocol.AskClearHome, req, AskWait, func(a *protocol.EdgeAnswer) {
		if ans, status, derr := homeClearAnswer(a); derr == nil {
			late(ans, status)
		} else {
			log.Printf("home_clear: %s, answered late: %v", req.HomeNode, derr)
		}
	})
	if err != nil {
		return nil, 0, err
	}
	return homeClearAnswer(a)
}

// homeClearAnswer is a clear_home answer as the door answered: its body (the
// ClearHomeResponse at every status) and its status.
func homeClearAnswer(a *protocol.EdgeAnswer) (*protocol.ClearHomeResponse, int, error) {
	var ans protocol.ClearHomeResponse
	if len(a.Body) > 0 {
		if err := json.Unmarshal(a.Body, &ans); err != nil {
			return nil, a.Status, fmt.Errorf("decode clear_home answer: %w", err)
		}
	} else {
		ans.Status, ans.Error = "error", a.Sentence
	}
	if len(ans.Rows) == 0 {
		ans.Rows = a.Rows
	}
	return &ans, a.Status, nil
}

// afterHomeClear is the home clear's follow-up on Core's answer: the zero
// recorded on the seat whenever Core made it, a refusal's rows into the bins
// copy before its sentence, the board's sentence for the arm Core chose.
func (e *Engine) afterHomeClear(nodeID int64, node *processes.Node, claim *processes.NodeClaim, ans *protocol.ClearHomeResponse, status int) (string, error) {
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
