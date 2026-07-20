package messaging

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/router"
)

// TestPlantClaims_MixedVersionNoOp pins the mixed-version no-op gate: an older
// Core that has not registered SubjectPlantClaims logs-and-ignores a plant.claims
// message rather than crashing or mis-dispatching. This is the SubjectRouter's
// unknown-subject path (the same one types.go documents as the
// forward-compatibility mechanism).
//
// The message must still round-trip the envelope codec (a new subject on the
// existing shingo.orders topic, a data envelope) so a future Core that DOES
// register it decodes it cleanly — but the old Core's router just drops it.
func TestPlantClaims_MixedVersionNoOp(t *testing.T) {
	t.Parallel()

	// An "old Core" router: nothing registered for plant.claims.
	oldCoreRouter := router.NewSubject()

	body := &protocol.PlantClaimsReport{
		ProcessID: "SNF2",
		Styles: []protocol.PlantClaimsStyle{{
			StyleID: "A",
			Claims: []protocol.PlantClaim{{
				CoreNodeName: "N1", Role: protocol.ClaimRoleConsume,
				SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "BIN-A",
			}},
		}},
	}
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectPlantClaims,
		protocol.Address{Role: protocol.RoleEdge, Station: "plant-a"},
		protocol.Address{Role: protocol.RoleCore},
		body,
	)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}

	// Decode the outer Data envelope the way the upstream Type-handler does.
	var data protocol.Data
	if err := env.DecodePayload(&data); err != nil {
		t.Fatalf("decode data envelope: %v", err)
	}

	// Capture log output — the old Core logs the no-handler notice and returns.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	oldCoreRouter.Dispatch(env, &data) // must NOT panic, must NOT dispatch

	out := buf.String()
	if !strings.Contains(out, "plant.claims") {
		t.Errorf("expected a logged no-handler notice mentioning plant.claims, got: %s", out)
	}
}

// TestPlantClaims_NewCoreDecodes cleanly pins the positive side: a router that
// HAS registered the subject decodes the body into *protocol.PlantClaimsReport.
// This guards the value-schema round trip the mixed-version-no-op test doesn't
// touch (it only checks the drop path).
func TestPlantClaims_NewCoreDecodes(t *testing.T) {
	t.Parallel()

	got := make(chan *protocol.PlantClaimsReport, 1)
	newCoreRouter := router.NewSubject()
	router.RegisterSubject(newCoreRouter, protocol.SubjectPlantClaims,
		func(_ *protocol.Envelope, r *protocol.PlantClaimsReport) { got <- r })

	body := &protocol.PlantClaimsReport{
		ProcessID: "SNF2", ConfigGen: 7,
		Styles: []protocol.PlantClaimsStyle{{
			StyleID: "A",
			Claims: []protocol.PlantClaim{{
				CoreNodeName: "N1", Role: protocol.ClaimRoleProduce,
				SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "BIN-A",
				AllowedPayloadCodes: []string{"BIN-A"}, UOPCapacity: 100, ReorderPoint: 5,
			}},
		}},
	}
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectPlantClaims,
		protocol.Address{Role: protocol.RoleEdge, Station: "plant-a"},
		protocol.Address{Role: protocol.RoleCore},
		body,
	)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	var data protocol.Data
	if err := env.DecodePayload(&data); err != nil {
		t.Fatalf("decode data envelope: %v", err)
	}
	newCoreRouter.Dispatch(env, &data)

	select {
	case r := <-got:
		if r.ProcessID != "SNF2" || r.ConfigGen != 7 {
			t.Errorf("decoded report = %+v, want process SNF2 config_gen 7", r)
		}
		if len(r.Styles) != 1 || r.Styles[0].StyleID != "A" {
			t.Errorf("decoded styles = %+v, want one style A", r.Styles)
		}
		if len(r.Styles[0].Claims) != 1 {
			t.Fatalf("decoded claims = %+v, want one", r.Styles[0].Claims)
		}
		c := r.Styles[0].Claims[0]
		if c.CoreNodeName != "N1" || c.Role != protocol.ClaimRoleProduce ||
			c.SwapMode != protocol.SwapModeTwoRobot || c.PayloadCode != "BIN-A" ||
			c.UOPCapacity != 100 || c.ReorderPoint != 5 {
			t.Errorf("decoded claim = %+v, want the full schema round-trip", c)
		}
	default:
		t.Fatal("handler not invoked for plant.claims on a registered router")
	}
}
