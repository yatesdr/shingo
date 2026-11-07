package backup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"shingoedge/config"
	"shingoedge/internal/testdb"
)

// TestPinS3Attempts_TheDefaultRetriesAFailedRequest pins the default that
// SetS3Attempts overrides. The storage answers its first request 500, which the
// client retries, and every later one 403, which it does not. The default
// storage (the client's own retries) asks twice; one attempt asks once. A
// default that stopped retrying would read 1 in the first case.
func TestPinS3Attempts_TheDefaultRetriesAFailedRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		attempts int // 0: SetS3Attempts never called
		want     int64
	}{
		{"default_retries_the_500", 0, 2},
		{"one_attempt_asks_once", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if hits.Add(1) == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusForbidden)
			}))
			defer srv.Close()

			svc := NewService(testdb.Open(t), &config.Config{}, t.TempDir()+"/config.yaml", "test", t.Logf)
			if tc.attempts != 0 {
				svc.SetS3Attempts(tc.attempts)
			}
			err := svc.TestConfig(context.Background(), config.BackupS3Config{
				Endpoint: srv.URL, Bucket: "pin-bucket", Region: "us-east-1",
				AccessKey: "PINKEY", SecretKey: "PINSECRET", UsePathStyle: true,
			})
			if err == nil {
				t.Fatal("TestConfig against a failing storage passed")
			}
			if got := hits.Load(); got != tc.want {
				t.Errorf("storage asked %d times, want %d (err: %v)", got, tc.want, err)
			}
		})
	}
}
