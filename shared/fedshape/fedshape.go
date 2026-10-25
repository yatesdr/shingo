// Package fedshape holds the case table that pins one shape test on both
// sides: is a claim a press fed from behind? Its paired position is set and is
// no core node of a claim of the same process and style, so the paired
// position is the outside cart's and the press draws its totes from it. Core
// asks it in SQL (store/bins FedClaimSQL); the Edge asks it in Go
// (domain.FedShape, SEND BACK's line). Each side's test reads this table, so
// the two cannot drift.
//
// The shape is role-blind: a consume press of the shape reads fed. The roles
// SEND BACK serves are the line's own test (domain.SendBackLine), not this
// one's.
//
// The shape is mode-blind and the table carries no mode: the Edge's claim save
// stores no paired position on a mode that names none, so a row of the table
// is a claim as the save stores it, and the Edge's mode-gated definition
// (NodeClaim.DependentPositions) answers every case the same
// (store.TestFedShapeIsTheDependentPositions_ThroughTheSave, which names the
// mode each case is saved under).
//
// It lives in shared/ for the reason loadervectors does: the one module both
// shingo-core and shingo-edge import. ADDING A CASE: add it here, name the
// mode the Edge saves it under in that test, and both sides must satisfy it.
package fedshape

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed vectors.json
var raw []byte

// Claim is one claim of a case's process and running style.
type Claim struct {
	Node         string `json:"node"`
	Role         string `json:"role"`
	Paired       string `json:"paired,omitempty"`
	SecondPaired string `json:"second_paired,omitempty"`
}

// Case is one process's running-style claims and the one asked about.
type Case struct {
	Name string `json:"name"`
	// Why this case exists. Read it before changing an expectation.
	Why    string  `json:"why"`
	Claims []Claim `json:"claims"`
	// Ask is the core node of the claim asked about.
	Ask     string `json:"ask"`
	WantFed bool   `json:"want_fed"`
}

// MustLoad returns the cases, panicking on a malformed file: the file is
// embedded, so a bad one is a build-time mistake.
func MustLoad() []Case {
	var out []Case
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(fmt.Sprintf("fedshape: vectors.json: %v", err))
	}
	return out
}

// AskClaim returns the case's claim asked about.
func (c Case) AskClaim() Claim {
	for _, cl := range c.Claims {
		if cl.Node == c.Ask {
			return cl
		}
	}
	return Claim{}
}
