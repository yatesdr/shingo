package rds

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// A config save swaps the HTTP client while the poller is mid-request. Under
// -race (the gate's race step) a read of the client outside the lock
// Reconfigure takes is a data race.
func TestClient_ReconfigureWhileRequestingIsNotARace(t *testing.T) {
	t.Parallel()
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ret_code":0}`))
	})
	defer srv.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = client.get("/robotsStatus", nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			client.Reconfigure(srv.URL, time.Duration(1+i%3)*time.Second)
		}
	}()
	wg.Wait()
}
