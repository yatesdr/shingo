package plc

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"shingoedge/config"
)

// TestRace_HandleSSEValueChangeMapInsert pins the single-WLock
// check-and-insert pattern in handleSSEValueChange. The older
// RLock-then-WLock shape let two concurrent SSE events for the
// same new PLC both observe "not in map" under RLock, both grab
// WLock, and both insert — the loser's ManagedPLC was orphaned
// and subsequent writes to mp.Values landed on the orphan.
//
// The race detector catches unsynchronized access to m.plcs
// across any lock-release / lock-reacquire boundary, so a
// regression to the old shape would fail this test under -race.
func TestRace_HandleSSEValueChangeMapInsert(t *testing.T) {
	if !raceEnabled {
		t.Skip("race detector not enabled; this test is meaningful only under -race")
	}
	cfg := config.Defaults()
	emitter := &mockEmitter{}
	mgr := NewManager(nil, cfg, emitter, nil)

	const goroutines = 32
	const iterations = 100

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Each iteration uses a fresh PLC name to maximize the
				// new-PLC insert path. Without the fix, the RLock check
				// returns "not found" for every goroutine simultaneously
				// and they all race the WLock insert.
				name := fmt.Sprintf("race-plc-%d-%d", gID, i)
				payload, _ := json.Marshal(sseValueChange{
					PLC:   name,
					Tag:   "TAG-A",
					Type:  "DINT",
					Value: 42,
				})
				mgr.handleSSEValueChange(string(payload))
			}
		}(g)
	}
	wg.Wait()
}

// TestRace_HandleSSEStatusChangeCrossLockDomain pins the
// cross-lock-domain pattern: writes to mp.Status / mp.Error /
// mp.ProductName / mp.Vendor go under mp.mu.Lock() because reads
// via IsConnected / ReadTag / GetPLCHealth take mp.mu.RLock().
// Writing them under m.mu.Lock() instead (as an older shape did)
// puts readers and writers on different mutexes for the same
// fields — race.
func TestRace_HandleSSEStatusChangeCrossLockDomain(t *testing.T) {
	if !raceEnabled {
		t.Skip("race detector not enabled; this test is meaningful only under -race")
	}
	cfg := config.Defaults()
	emitter := &mockEmitter{}
	mgr := NewManager(nil, cfg, emitter, nil)

	const plcName = "race-status-plc"

	// Seed the PLC so the read side has something to find.
	seed, _ := json.Marshal(sseStatusChange{PLC: plcName, Status: "Connected"})
	mgr.handleSSEStatusChange(string(seed))

	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: alternates Connected/Disconnected status.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			status := "Connected"
			if i%2 == 0 {
				status = "Disconnected"
			}
			payload, _ := json.Marshal(sseStatusChange{
				PLC:    plcName,
				Status: status,
				Error:  "transient",
			})
			mgr.handleSSEStatusChange(string(payload))
		}
	}()

	// Reader: hits IsConnected which reads mp.Status under mp.mu.RLock().
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = mgr.IsConnected(plcName)
		}
	}()

	wg.Wait()
}

// TestRace_HandleSSEHealthCrossLockDomain — same shape as
// TestRace_HandleSSEStatusChangeCrossLockDomain but for mp.Health.
// handleSSEHealth writes mp.Health and GetPLCHealth reads it;
// both must use mp.mu, not m.mu.
func TestRace_HandleSSEHealthCrossLockDomain(t *testing.T) {
	if !raceEnabled {
		t.Skip("race detector not enabled; this test is meaningful only under -race")
	}
	cfg := config.Defaults()
	emitter := &mockEmitter{}
	mgr := NewManager(nil, cfg, emitter, nil)

	const plcName = "race-health-plc"

	// Seed the PLC.
	seed, _ := json.Marshal(sseStatusChange{PLC: plcName, Status: "Connected"})
	mgr.handleSSEStatusChange(string(seed))

	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			payload, _ := json.Marshal(sseHealthUpdate{
				PLC:    plcName,
				Online: i%2 == 0,
				Driver: "test",
				Status: "ok",
			})
			mgr.handleSSEHealth(string(payload))
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = mgr.GetPLCHealth(plcName)
		}
	}()

	wg.Wait()
}
