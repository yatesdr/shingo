package plc

import (
	"context"
	"sync"
	"testing"

	"shingoedge/config"
)

// TestRace_WarlinkClientSwap pins the RLock-around-m.wl-reads
// pattern in warlink_tags.go's five delegate methods (ReadTagValue,
// WriteTagValue, EnableTagPublishing, DisableTagPublishing,
// FetchAllTags). ReplaceClient swaps m.wl under m.mu.Lock(); any
// delegate that reads m.wl without m.mu.RLock() would race.
func TestRace_WarlinkClientSwap(t *testing.T) {
	if !raceEnabled {
		t.Skip("race detector not enabled; this test is meaningful only under -race")
	}
	cfg := config.Defaults()
	emitter := &mockEmitter{}
	mgr := NewManager(nil, cfg, emitter)
	mgr.wl = &mockWarlinkClient{}

	const iterations = 200
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(2)

	// Reader: hammers ReadTagValue, which dereferences m.wl.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, _ = mgr.ReadTagValue(ctx, "plc-a", "tag-a")
		}
	}()

	// Writer: swaps m.wl under m.mu.Lock().
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			mgr.ReplaceClient(&mockWarlinkClient{})
		}
	}()

	wg.Wait()
}
