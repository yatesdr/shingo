package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testAccessKey = "AKIA-RECOGNISABLE-ACCESS-000"
	testSecretKey = "SECRET-RECOGNISABLE-VALUE-999"
)

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	return New(Config{
		BaseURL: baseURL, AccessKey: testAccessKey, SecretKey: testSecretKey,
		Timeout: 2 * time.Second,
	})
}

// serve stands up a middleware stub and returns a client pointed at it.
func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return newTestClient(t, srv.URL)
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// ── the class matrix ────────────────────────────────────────────────────
//
// Every row of this table is a different answer to one question: could bytes
// have reached the middleware? That question, not severity, is what decides
// whether a retry is safe, because the API has no idempotency key.

func TestPost_ClassMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		handler   http.HandlerFunc
		wantClass Class
		wantTxID  string
		why       string
	}{
		{
			name:      "2xx with a transaction id",
			handler:   reply(200, `{"TransactionId":"MW-4242"}`),
			wantClass: ClassPosted, wantTxID: "MW-4242",
			why: "acknowledged and named — the only unambiguous success",
		},
		{
			name:      "201 with a numeric id",
			handler:   reply(201, `{"TransactionId":907}`),
			wantClass: ClassPosted, wantTxID: "907",
			why: "a JSON number is still an id; refusing it would strand a posting that landed",
		},
		{
			name:      "2xx with an unreadable body",
			handler:   reply(200, `<html>gateway said ok</html>`),
			wantClass: ClassInflight,
			why:       "it landed and we cannot name it — asking is the only safe next move",
		},
		{
			name:      "2xx with no id in the body",
			handler:   reply(200, `{"status":"accepted"}`),
			wantClass: ClassInflight,
			why:       "same as unreadable: accepted, unnamed",
		},
		{
			name:      "400 validation",
			handler:   reply(400, `{"error":"PartNumber required"}`),
			wantClass: ClassRejected,
			why:       "the body is wrong and will be wrong again",
		},
		{
			name:      "422 validation",
			handler:   reply(422, `{"error":"bad StockLocation"}`),
			wantClass: ClassRejected,
			why:       "same class as 400",
		},
		{
			name:      "401",
			handler:   reply(401, `unauthorized`),
			wantClass: ClassAuthFault,
			why:       "every queued posting would fail identically — halt rather than burn their budgets",
		},
		{
			name:      "403",
			handler:   reply(403, `forbidden`),
			wantClass: ClassAuthFault,
			why:       "same as 401",
		},
		{
			name:      "500",
			handler:   reply(500, `boom`),
			wantClass: ClassRetryableAfterSend,
			why:       "the server HAS the request; it may have booked it before failing",
		},
		{
			name:      "503",
			handler:   reply(503, `unavailable`),
			wantClass: ClassRetryableAfterSend,
			why:       "same as 500",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := serve(t, tc.handler)
			got := c.Post(context.Background(), []byte(`[]`), "sha")
			if got.Class != tc.wantClass {
				t.Errorf("class = %s, want %s — %s", got.Class, tc.wantClass, tc.why)
			}
			if tc.wantTxID != "" && got.TransactionID != tc.wantTxID {
				t.Errorf("transaction id = %q, want %q", got.TransactionID, tc.wantTxID)
			}
		})
	}
}

// TestPost_UnresolvableHostIsBeforeSend: DNS fails before a request line is
// written, so nothing arrived and re-sending cannot duplicate. Getting this
// wrong in the safe direction costs one reconciler query; getting it wrong the
// other way books a transfer twice.
func TestPost_UnresolvableHostIsBeforeSend(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, "http://this-host-does-not-resolve.invalid/api")
	got := c.Post(context.Background(), []byte(`[]`), "sha")
	if got.Class != ClassRetryableBeforeSend {
		t.Errorf("class = %s, want retryable_before_send — DNS fails before any bytes go out", got.Class)
	}
	if got.Err == nil {
		t.Error("a transport failure with no error is a failure nobody can diagnose")
	}
}

// TestPost_RefusedConnectionIsBeforeSend: a closed port answers at dial time.
func TestPost_RefusedConnectionIsBeforeSend(t *testing.T) {
	t.Parallel()
	// Bind and immediately release, so the port is almost certainly dead.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a probe port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	c := newTestClient(t, "http://"+addr)
	got := c.Post(context.Background(), []byte(`[]`), "sha")
	if got.Class != ClassRetryableBeforeSend {
		t.Errorf("class = %s, want retryable_before_send — a refused dial wrote nothing", got.Class)
	}
}

// TestPost_ResponseTimeoutIsAfterSend is the dangerous one. The server HAS the
// request and is simply slow to answer; the client's timeout fires anyway.
// Classifying this as before-send would re-POST a transfer the middleware is in
// the middle of booking.
func TestPost_ResponseTimeoutIsAfterSend(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the request fully, then stall past the client's timeout.
		time.Sleep(600 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, AccessKey: testAccessKey, SecretKey: testSecretKey,
		Timeout: 100 * time.Millisecond})
	got := c.Post(context.Background(), []byte(`[]`), "sha")
	if got.Class != ClassRetryableAfterSend {
		t.Errorf("class = %s, want retryable_after_send — the server already has the request", got.Class)
	}
}

// TestPost_CancelledContextIsAfterSend: a cancellation can land at any point,
// including while the server writes a response. Nothing in the client can tell
// which, so it takes the safe side.
func TestPost_CancelledContextIsAfterSend(t *testing.T) {
	t.Parallel()
	c := serve(t, reply(200, `{"TransactionId":"X"}`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := c.Post(ctx, []byte(`[]`), "sha")
	if got.Class != ClassRetryableAfterSend {
		t.Errorf("class = %s, want retryable_after_send for a cancelled context", got.Class)
	}
}

// ── credentials ─────────────────────────────────────────────────────────

func TestPost_SendsCredentialsAsHeaders(t *testing.T) {
	t.Parallel()
	var gotAccess, gotSecret, gotSHA, gotType string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotAccess = r.Header.Get("x-access-key")
		gotSecret = r.Header.Get("x-secret-key")
		gotSHA = r.Header.Get("x-body-sha256")
		gotType = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"TransactionId":"T"}`))
	})
	c.Post(context.Background(), []byte(`[{"a":1}]`), "abc123")

	if gotAccess != testAccessKey || gotSecret != testSecretKey {
		t.Errorf("credential headers = %q / %q, want the configured keys", gotAccess, gotSecret)
	}
	if gotSHA != "abc123" {
		t.Errorf("x-body-sha256 = %q, want abc123 — the dedup hint must actually be sent", gotSHA)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
}

// TestKeysNeverAppearInAnyReturnedError is the leak guard. last_error is stored
// per posting and read on a diagnostics page, so anything reaching it is
// effectively published. rds/client.go dumps its whole request at debug level;
// this client must not, and must not leak through its error strings either.
func TestKeysNeverAppearInAnyReturnedError(t *testing.T) {
	t.Parallel()

	// Every shape that produces an error: a refusal echoing the request, a
	// 5xx, an auth fault, and a transport failure whose URL carries the key.
	echo := serve(t, func(w http.ResponseWriter, r *http.Request) {
		// A middleware that echoes the headers it received. Hostile, and
		// exactly the case a redaction pass exists for.
		w.WriteHeader(400)
		_, _ = w.Write([]byte("rejected request with x-access-key=" + r.Header.Get("x-access-key") +
			" and x-secret-key=" + r.Header.Get("x-secret-key")))
	})
	results := []PostResult{
		echo.Post(context.Background(), []byte(`[]`), "sha"),
		serve(t, reply(500, "server error")).Post(context.Background(), []byte(`[]`), "sha"),
		serve(t, reply(401, "denied")).Post(context.Background(), []byte(`[]`), "sha"),
	}
	// A URL carrying the key in its path — the shape a *url.Error would echo.
	leaky := New(Config{
		BaseURL:   "http://does-not-resolve.invalid/" + testSecretKey,
		AccessKey: testAccessKey, SecretKey: testSecretKey, Timeout: time.Second,
	})
	results = append(results, leaky.Post(context.Background(), []byte(`[]`), "sha"))

	for i, res := range results {
		if res.Err == nil {
			t.Errorf("result %d has no error to inspect", i)
			continue
		}
		msg := res.Err.Error()
		if strings.Contains(msg, testAccessKey) {
			t.Errorf("result %d leaks the access key: %s", i, msg)
		}
		if strings.Contains(msg, testSecretKey) {
			t.Errorf("result %d leaks the secret key: %s", i, msg)
		}
	}
}

// TestErrorExcerptsAreBounded: last_error is a column, not a log file. An
// endpoint answering with a megabyte of HTML must not put a megabyte in a row.
func TestErrorExcerptsAreBounded(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("x", 200_000)
	c := serve(t, reply(400, huge))
	got := c.Post(context.Background(), []byte(`[]`), "sha")
	if got.Err == nil {
		t.Fatal("a 400 with a body produced no error")
	}
	if len(got.Err.Error()) > maxExcerptBytes+64 {
		t.Errorf("error is %d bytes, want at most ~%d — this is stored per posting",
			len(got.Err.Error()), maxExcerptBytes)
	}
}

// ── body hash ───────────────────────────────────────────────────────────

func TestBodySHA_IsStableAndDistinguishing(t *testing.T) {
	t.Parallel()
	a := BodySHA([]byte(`[{"PartNumber":"A"}]`))
	again := BodySHA([]byte(`[{"PartNumber":"A"}]`))
	b := BodySHA([]byte(`[{"PartNumber":"B"}]`))

	if a != again {
		t.Errorf("the same body hashed to %s then %s", a, again)
	}
	if a == b {
		t.Error("two different bodies hashed the same — the dedup hint would merge them")
	}
	if len(a) != 64 {
		t.Errorf("hash length = %d, want 64 hex characters of sha256", len(a))
	}
}

// ── status GET ──────────────────────────────────────────────────────────

func TestGetByTxID_ThreeValuedAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		handler   http.HandlerFunc
		wantFound bool
		wantErr   bool
		why       string
	}{
		{
			name:      "200 with a record",
			handler:   reply(200, `[{"TransactionId":"MW-1"}]`),
			wantFound: true,
			why:       "the middleware has it — the posting is settled",
		},
		{
			name:      "404",
			handler:   reply(404, `not found`),
			wantFound: false,
			why:       "it does not have it — and ONLY this makes a re-send safe",
		},
		{
			name:      "200 with an empty array",
			handler:   reply(200, `[]`),
			wantFound: false,
			why:       "a not-found in a different costume; reading it as found strands the posting forever",
		},
		{
			name:    "500",
			handler: reply(500, `boom`),
			wantErr: true,
			why:     "COULD NOT ASK. Collapsing this into not-found re-sends a transaction that landed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := serve(t, tc.handler)
			found, err := c.GetByTxID(context.Background(), "MW-1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error — %s", tc.why)
				}
				if found {
					t.Error("found is true alongside an error; a caller reading only found would act on a non-answer")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if found != tc.wantFound {
				t.Errorf("found = %v, want %v — %s", found, tc.wantFound, tc.why)
			}
		})
	}
}

func TestGetByTxID_RefusesAnEmptyID(t *testing.T) {
	t.Parallel()
	c := serve(t, reply(200, `[{"TransactionId":"anything"}]`))
	found, err := c.GetByTxID(context.Background(), "")
	if err == nil {
		t.Error("querying for an empty id must be refused here — otherwise it asks " +
			"'do you have <nothing>' and a permissive endpoint answers yes")
	}
	if found {
		t.Error("found is true for an empty id")
	}
}

func TestGetByTxID_SendsTheIDAndTheCredentials(t *testing.T) {
	t.Parallel()
	var gotQuery, gotAccess string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("TransactionId")
		gotAccess = r.Header.Get("x-access-key")
		_, _ = w.Write([]byte(`[{"x":1}]`))
	})
	if _, err := c.GetByTxID(context.Background(), "MW/42 43"); err != nil {
		t.Fatalf("GetByTxID: %v", err)
	}
	if gotQuery != "MW/42 43" {
		t.Errorf("TransactionId arrived as %q, want the id escaped and decoded intact", gotQuery)
	}
	if gotAccess != testAccessKey {
		t.Error("the status query went out unauthenticated")
	}
}

// ── transport classification, directly ──────────────────────────────────

// The matrix above drives real sockets; this covers the shapes that are hard
// to provoke and easy to get wrong when the error chain changes.
func TestClassifyTransportErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want Class
	}{
		{"dns", &net.DNSError{Err: "no such host", Name: "x.invalid"}, ClassRetryableBeforeSend},
		{"dial refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, ClassRetryableBeforeSend},
		{"read reset", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, ClassRetryableAfterSend},
		{"write reset", &net.OpError{Op: "write", Err: errors.New("broken pipe")}, ClassRetryableAfterSend},
		{"tls handshake", errors.New(`tls: handshake failure`), ClassRetryableBeforeSend},
		{"x509", errors.New(`x509: certificate signed by unknown authority`), ClassRetryableBeforeSend},
		{"deadline", context.DeadlineExceeded, ClassRetryableAfterSend},
		{"unrecognised", errors.New("something nobody has seen before"), ClassRetryableAfterSend},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyTransportErr(tc.err); got != tc.want {
				t.Errorf("classify(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyTransportErr_UnknownFailsSafe states the rule the last row of the
// table above encodes, because it is the one a future edit is most likely to
// "improve": an error nobody recognises must be treated as after-send. Guessing
// before-send on something that actually went out books a transfer twice;
// guessing after-send costs one reconciler query.
func TestClassifyTransportErr_UnknownFailsSafe(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		errors.New(""),
		errors.New("EOF"),
		errors.New("http2: server sent GOAWAY"),
		errors.New("unexpected EOF"),
	} {
		if got := classifyTransportErr(err); got != ClassRetryableAfterSend {
			t.Errorf("classify(%q) = %s, want retryable_after_send — an unrecognised failure "+
				"must not be assumed safe to re-send", err, got)
		}
	}
}
