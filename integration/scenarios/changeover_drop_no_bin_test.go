// A changeover drop whose removal Core fails because it has no bin to take,
// end to end across the two modules: the station clears the line on Core's
// code for it, and shows an error when the failure carries no code, as every
// v0.18.0 Core's does.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"shingo/protocol"

	"shingoedge/domain"
)

// failedTells hands Core's messages to the Edge and records the code on every
// failed order.update among them. v0180 then takes the code out of that
// update, as a v0.18.0 Core's tell has none; its order.error, which carries
// the code, goes as Core sent it, as v0.18.0's does.
func failedTells(c *ksrCell, v0180 bool) *[]string {
	codes := []string{}
	c.bus.RewriteCoreToEdge(func(raw []byte) []byte {
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil || env.Type != protocol.TypeOrderUpdate {
			return raw
		}
		var u protocol.OrderUpdate
		mustNil(c.t, env.DecodePayload(&u), "an order.update from Core")
		if u.Status != string(protocol.StatusFailed) {
			return raw
		}
		codes = append(codes, u.ErrorCode)
		if !v0180 {
			return raw
		}
		var body map[string]json.RawMessage
		mustNil(c.t, json.Unmarshal(env.Payload, &body), "the failed tell's keys")
		delete(body, "error_code")
		p, err := json.Marshal(body)
		mustNil(c.t, err, "the failed tell without its code")
		env.Payload = p
		out, err := env.Encode()
		mustNil(c.t, err, "the envelope without the code")
		return out
	})
	return &codes
}

// The line leaves the style at the changeover: style B claims nothing there,
// and A's claim marks it for a tool change, so the cutover waits for the line
// to be clear. Core fails the line's removal with the code and sentence its
// dispatcher writes when a removal has no bin to take (no_bin, "complex order
// N has no source pickup"), through the lifecycle call that dispatcher makes.
// The failure is written by hand because no plan the Edge sends a drop's
// removal reaches it: a removal whose line Core finds empty is skipped, and
// the skip clears the task by its own message (order.skipped).
//
// From this Core the failed order.update carries the code and the station
// ends the task line_cleared with its note, which opens the cutover. From a
// v0.18.0 Core the same failure's update carries none (its order.error does)
// and the task ends at error, as it does at the plants today; the cutover
// stays shut.
func TestScenario_ChangeoverDrop_CoresNoBinFailureClearsTheLine(t *testing.T) {
	t.Parallel()
	for _, m := range []struct {
		name     string
		v0180    bool
		state    domain.NodeTaskState
		note     string
		gateOpen bool
	}{
		{"this Core, which sends the code", false, domain.NodeTaskLineCleared,
			"evac auto-cleared: no bin to remove at " + ksrLine, true},
		{"a v0.18.0 Core, which sends none", true, domain.NodeTaskError, "", false},
	} {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSingleRobot,
				plain: true, dropB: true})
			codes := failedTells(c, m.v0180)

			co, err := c.edge.Engine.StartProcessChangeover(c.processID, c.styleB, "ksr", "")
			if err != nil {
				c.dump(m.name)
				t.Fatalf("START refused: %v", err)
			}
			c.tick()
			task, err := c.edge.DB.GetChangeoverNodeTaskByNode(co.ID, c.nodeID)
			mustNil(t, err, "the line's task")
			if task.Situation != string(domain.SituationDrop) || task.State != domain.NodeTaskEmptyRequested ||
				task.OldMaterialReleaseOrderID == nil {
				c.notReached(fmt.Sprintf("the line's task is %s at %s with removal %v, want a drop at %s with one",
					task.Situation, task.State, task.OldMaterialReleaseOrderID, domain.NodeTaskEmptyRequested))
			}
			evac := *c.edgeRow(*task.OldMaterialReleaseOrderID)
			c.eventually(m.name+": the removal at Core, live, and the station on Core's version of it", func() bool {
				core, err := c.core.eng.DB().GetOrderByUUID(evac.UUID)
				return err == nil && core != nil && !protocol.IsTerminal(core.Status) && c.edgeRow(evac.ID).CoreVersion > 0
			})

			core := c.coreOf(evac)
			mustNil(t, c.core.eng.Dispatcher().Lifecycle().Fail(core, core.Requester(), string(protocol.TermNoBin),
				fmt.Sprintf("complex order %d has no source pickup", core.ID)), "Core fails the removal")
			c.eventually(m.name+": the removal failed at the station", func() bool {
				return c.edgeRow(evac.ID).Status == protocol.StatusFailed
			})

			// The input: Core sent one failed update, with its code.
			if !slices.Equal(*codes, []string{string(protocol.TermNoBin)}) {
				c.dump(m.name)
				t.Fatalf("Core's failed updates carried codes %q, want one, no_bin", *codes)
			}
			task, err = c.edge.DB.GetChangeoverNodeTaskByNode(co.ID, c.nodeID)
			mustNil(t, err, "the line's task after the failure")
			if task.State != m.state || task.SkipNote != m.note {
				c.dump(m.name)
				t.Errorf("the line's task is %s with note %q, want %s with note %q", task.State, task.SkipNote,
					m.state, m.note)
			}
			ok, blockers, err := c.edge.Engine.ChangeoverGateStatus(c.processID)
			mustNil(t, err, "the cutover gate")
			if ok != m.gateOpen {
				t.Errorf("the cutover gate open = %v (blockers %+v), want %v", ok, blockers, m.gateOpen)
			}
		})
	}
}
