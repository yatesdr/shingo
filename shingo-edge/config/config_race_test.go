package config

import (
	"sync"
	"testing"
)

// TestRace_AdoptLeavesUnchangedFieldsAlone: background loops read the live
// config WITHOUT the lock (e.cfg.Web.AutoConfirm, ApplyWarLinkConfig's
// e.cfg.WarLink, cfg.StationID() users). A swap that rewrote every field
// raced each of them, even writing the same value. Adopt writes only the
// leaves the save changed, so a reader of an unchanged field is not raced.
// Meaningful under -race (go test -race -run TestRace_ ./config/).
func TestRace_AdoptLeavesUnchangedFieldsAlone(t *testing.T) {
	live := Defaults()
	live.Messaging.Kafka.Brokers = []string{"b1:9092"}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Lock-free reads of fields the save below does not change.
			_ = live.Web.AutoConfirm
			_ = live.WarLink.Host
			_ = live.StationUID
			_ = len(live.Messaging.Kafka.Brokers)
		}
	}()
	for i := 0; i < 50; i++ {
		draft := live.Clone()
		draft.CoreAPI = "http://core.test/" + string(rune('a'+i%26))
		live.Adopt(draft)
	}
	close(stop)
	wg.Wait()
	live.RLock()
	defer live.RUnlock()
	if live.CoreAPI == "" || live.WarLink.Host != "localhost" || len(live.Messaging.Kafka.Brokers) != 1 {
		t.Errorf("after adopt: core=%q host=%q brokers=%v", live.CoreAPI, live.WarLink.Host, live.Messaging.Kafka.Brokers)
	}
}
