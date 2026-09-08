// Package client is the HTTP transport to the CMS middleware.
//
// It does one round trip at a time and returns what happened. No goroutines,
// no retry loop, no sleep — the poster owns all of that, because the decision
// about whether to send something again depends on durable state this package
// cannot see.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Class is what a caller is allowed to conclude from an attempt.
//
// THE SPLIT THAT MATTERS IS BEFORE-SEND VERSUS AFTER-SEND, not "error" versus
// "success". This API has no idempotency key, so the only question that decides
// whether a retry is safe is whether any bytes could have reached the
// middleware. A DNS failure and a read timeout are both "the POST failed" and
// they are opposite answers to that question: one cannot have arrived, the
// other may already have been booked.
type Class int

const (
	// ClassRetryableAfterSend — 5xx, or a timeout that may have fired while
	// waiting for a response to a request the server already has. It MAY have
	// been booked. The row stays inflight and the reconciler asks.
	//
	// FIRST, SO THAT IT IS THE ZERO VALUE, and the placement is the point
	// rather than an ordering preference. A bare PostResult{} has to mean
	// something, and the safe thing for it to mean is "this may have landed":
	// a row left inflight costs one reconciler query. With ClassPosted at iota
	// 0, a future caller who forgot to set Class would settle a posting that
	// was never sent — and a settled posting is never looked at again. Every
	// return in this file sets Class explicitly, so nothing changes today.
	ClassRetryableAfterSend Class = iota
	// ClassPosted — 2xx with a transaction id. Done.
	ClassPosted
	// ClassInflight — 2xx whose body we could not read an id out of. The
	// middleware accepted something; we do not know what it called it. Never
	// re-POST: ask.
	ClassInflight
	// ClassRejected — 4xx validation. The body is wrong and will be wrong
	// again. Terminal.
	ClassRejected
	// ClassAuthFault — 401/403, or a request this client could not even build.
	// Every subsequent row would fail the same way, so this must halt the
	// sender rather than burn the retry budget of every queued posting against
	// a fault nobody has fixed yet.
	ClassAuthFault
	// ClassRetryableBeforeSend — the request never left. DNS, TLS handshake,
	// connection refused. Safe to send again: nothing arrived.
	ClassRetryableBeforeSend
)

func (c Class) String() string {
	switch c {
	case ClassPosted:
		return "posted"
	case ClassInflight:
		return "inflight"
	case ClassRejected:
		return "rejected"
	case ClassAuthFault:
		return "auth_fault"
	case ClassRetryableBeforeSend:
		return "retryable_before_send"
	case ClassRetryableAfterSend:
		return "retryable_after_send"
	}
	return "unknown"
}

// PostResult is one attempt's outcome.
type PostResult struct {
	Class         Class
	TransactionID string
	HTTPStatus    int
	// Err is the transport or decode failure, if any. Already redacted.
	Err error
}

// Config is what the client needs to talk to the middleware.
type Config struct {
	BaseURL   string
	AccessKey string
	SecretKey string
	Timeout   time.Duration
	// InsecureSkipVerify disables TLS certificate verification for this
	// client. See CMSConfig.InsecureSkipVerify for why a site would set it and
	// what it costs; the short version is that it authenticates NOTHING about
	// the far end, on a connection carrying the site's CMS credentials.
	InsecureSkipVerify bool
}

// Client posts inventory transactions and asks after them.
type Client struct {
	baseURL string
	access  string
	secret  string
	http    *http.Client
}

// DefaultTimeout is the shipped HTTP timeout, and the ONLY place the number
// lives. config's CMSDefaults and Validate both read it from here rather than
// spelling it again — three copies of one number is three places to retune and
// two of them to forget.
//
// It is not "wait forever", which is the http.Client default and is how a
// poster goroutine disappears.
const DefaultTimeout = 30 * time.Second

// New builds a client. A zero timeout gets DefaultTimeout.
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	httpc := &http.Client{Timeout: timeout}
	if cfg.InsecureSkipVerify {
		// Clone the stdlib's transport rather than building a bare one, so
		// connection pooling, proxy handling and the HTTP/2 upgrade stay
		// whatever Go ships. ONLY the trust decision changes; a hand-rolled
		// &http.Transport{} would silently drop the rest.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // site-local opt-in; see CMSConfig.InsecureSkipVerify
		httpc.Transport = tr
	}
	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		access:  cfg.AccessKey,
		secret:  cfg.SecretKey,
		http:    httpc,
	}
}

// BodySHA is the sha256 of a canonical body, hex-encoded.
//
// It is offered to the middleware as a dedup hint and used locally to record
// exactly what was sent. It is only meaningful because wire.Build serialises a
// given set of rows identically every time — a body that permuted between
// attempts would hash differently and the hint would be worthless.
func BodySHA(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Post sends one transaction array.
//
// The result's Class is the caller's whole decision. Post deliberately does not
// retry, sleep, or decide anything about the future: whether a retry is safe
// depends on durable state (has this posting already been marked inflight?)
// that lives in the database, not here.
func (c *Client) Post(ctx context.Context, body []byte, bodySHA string) PostResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		// A CONFIG FAULT, NOT A REFUSAL, and the difference decides whether a
		// yaml edit can repair it. This fires when base_url will not parse,
		// which is identical for every posting in the queue. Classified
		// rejected — as it was — the first drain marked every posting
		// terminally rejected, forever, for a typo, and rejected rows are
		// never retried. AuthFault mutes the sender and leaves the rows
		// pending, which is the same shape of fault and the same right answer.
		return PostResult{Class: ClassAuthFault, Err: fmt.Errorf("build request: %w", redactErr(err, c.access, c.secret))}
	}
	req.Header.Set("Content-Type", "application/json")
	c.authenticate(req)
	if bodySHA != "" {
		// Asked of IT as a dedup key: it closes the crash-after-accept window
		// that nothing else can. Harmless if the middleware ignores it.
		req.Header.Set("x-body-sha256", bodySHA)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return PostResult{Class: classifyTransportErr(err), Err: redactErr(err, c.access, c.secret)}
	}
	defer resp.Body.Close()

	// Bounded read: an endpoint that answers with a gigabyte must not be able
	// to exhaust a core's memory through the error path.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return PostResult{
			Class:      ClassAuthFault,
			HTTPStatus: resp.StatusCode,
			Err:        fmt.Errorf("POST %d: %s", resp.StatusCode, excerpt(raw, c.access, c.secret)),
		}
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if readErr != nil {
			// The middleware accepted it; we lost the answer on the way back.
			// That is exactly the inflight case — it landed, we cannot name it.
			return PostResult{Class: ClassInflight, HTTPStatus: resp.StatusCode,
				Err: fmt.Errorf("read accepted response: %w", redactErr(readErr, c.access, c.secret))}
		}
		id := transactionID(raw)
		if id == "" {
			return PostResult{Class: ClassInflight, HTTPStatus: resp.StatusCode,
				Err: fmt.Errorf("accepted with no transaction id in %s", excerpt(raw, c.access, c.secret))}
		}
		return PostResult{Class: ClassPosted, TransactionID: id, HTTPStatus: resp.StatusCode}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return PostResult{
			Class:      ClassRejected,
			HTTPStatus: resp.StatusCode,
			Err:        fmt.Errorf("POST %d: %s", resp.StatusCode, excerpt(raw, c.access, c.secret)),
		}
	default:
		// 5xx and anything else: the request was received and the server said
		// it could not answer. It may or may not have booked it first.
		return PostResult{
			Class:      ClassRetryableAfterSend,
			HTTPStatus: resp.StatusCode,
			Err:        fmt.Errorf("POST %d: %s", resp.StatusCode, excerpt(raw, c.access, c.secret)),
		}
	}
}

// GetByTxID asks whether the middleware holds a transaction.
//
// This is the only safe way out of `inflight`. Its answer is three-valued and
// the caller must treat it that way: found, not-found, and "could not ask".
// Collapsing the third into not-found would re-send a transaction that landed.
func (c *Client) GetByTxID(ctx context.Context, transactionID string) (found bool, err error) {
	if transactionID == "" {
		return false, errors.New("cannot query the middleware for an empty transaction id")
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return false, fmt.Errorf("parse base url: %w", redactErr(err, c.access, c.secret))
	}
	// Set on the parsed query rather than concatenated onto the string.
	// base_url is a value someone hand-edits at cutover, and one that already
	// carried a query string would get a second "?" and ask for nothing.
	q := u.Query()
	q.Set("TransactionId", transactionID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, fmt.Errorf("build status request: %w", redactErr(err, c.access, c.secret))
	}
	c.authenticate(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return false, redactErr(err, c.access, c.secret)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// A 200 with an empty result set is a not-found in a different
		// costume. Treating it as found would strand the posting forever.
		return !emptyResult(raw), nil
	default:
		return false, fmt.Errorf("GET %d: %s", resp.StatusCode, excerpt(raw, c.access, c.secret))
	}
}

// maxResponseBytes bounds what an error message can be built from.
const maxResponseBytes = 64 << 10

// maxExcerptBytes bounds what reaches last_error, which is stored per posting.
const maxExcerptBytes = 500

// authenticate sets the credential headers.
//
// PER-REQUEST, AND NEVER LOGGED. rds/client.go dumps its full request body at
// debug level, which is a leak the moment a credential moves into a body; this
// client has no such path and no debug dump at all. If one is ever added, it
// must not be given these headers.
func (c *Client) authenticate(req *http.Request) {
	req.Header.Set("x-access-key", c.access)
	req.Header.Set("x-secret-key", c.secret)
}

// classifyTransportErr decides whether bytes could have reached the middleware.
//
// The distinction is not cosmetic and it is not about severity. A DNS failure,
// a refused connection or a TLS handshake failure all happen BEFORE a request
// line is written — nothing arrived, so sending again cannot duplicate. A
// timeout is the dangerous one: http.Client's timeout covers the whole
// exchange, so it fires just as readily while waiting for a response to a
// request the server already has. Anything that might be that is treated as
// after-send, which costs a reconciler round trip and cannot double-book.
func classifyTransportErr(err error) Class {
	if err == nil {
		return ClassRetryableAfterSend
	}
	// A cancelled or expired context can fire at ANY point, including while
	// waiting for a response the server is already writing. Nothing here can
	// tell which, so it takes the safe side.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ClassRetryableAfterSend
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ClassRetryableBeforeSend
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// A failure while DIALLING is before-send. A failure while reading or
		// writing an established connection is not.
		if opErr.Op == "dial" {
			return ClassRetryableBeforeSend
		}
		return ClassRetryableAfterSend
	}
	// TLS handshake failures arrive as a plain error from the handshake, which
	// happens before any application bytes are written.
	if strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:") {
		return ClassRetryableBeforeSend
	}
	// Unknown: assume the worst. Guessing before-send on something that was
	// actually after-send is how a transfer gets booked twice, and the cost of
	// guessing the other way is one reconciler query.
	return ClassRetryableAfterSend
}

// transactionID pulls the middleware's id out of a response body, tolerating
// the several shapes a JSON API might use for it. Empty means "not found",
// which the caller treats as inflight rather than as success.
func transactionID(raw []byte) string {
	// UseNumber, NOT the default. encoding/json decodes every JSON number into
	// a float64 when the destination is `any`, and a float64 carries 53 bits
	// of mantissa — so an id above 9007199254740992 comes back ROUNDED, and
	// the value the reconciler would later ask the middleware about is not the
	// value the middleware gave us. json.Number keeps the literal text, which
	// is all this function ever wanted: the id is an opaque token to us.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return ""
	}
	for _, k := range []string{"TransactionId", "TransactionID", "transaction_id", "transactionId", "id"} {
		if v, ok := obj[k]; ok {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t
				}
			case json.Number:
				return t.String()
			}
		}
	}
	return ""
}

// emptyResult reports whether a 2xx status body says "nothing matched".
//
// A JSON array is the common shape and an empty one is unambiguous. Anything
// else is treated as a hit: guessing "empty" would requeue a posting the
// middleware actually holds, which is the one outcome this whole design is
// built to avoid.
func emptyResult(raw []byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "[]" || trimmed == "null" {
		return true
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err == nil {
		return len(arr) == 0
	}
	return false
}

// excerpt renders a bounded, redacted slice of a response body for last_error.
//
// THE CUT IS ON A RUNE BOUNDARY, NOT A BYTE ONE. last_error is a Postgres
// `text` column and Postgres rejects invalid UTF-8 outright, so a response body
// with a multi-byte rune straddling byte 500 produced a value the database
// refused — a write failure in the one path built to record failures, taking
// the reason for the original failure down with it. The middleware's body is
// not ours and there is nothing stopping it carrying a name with an accent in
// it.
func excerpt(raw []byte, secrets ...string) string {
	s := strings.TrimSpace(string(raw))
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "<redacted>")
		}
	}
	if len(s) <= maxExcerptBytes {
		return s
	}
	// Back up to the start of the rune straddling the limit. Every UTF-8
	// continuation byte is 10xxxxxx and no rune starts with one, so this walks
	// back at most three bytes.
	cut := maxExcerptBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// redactErr scrubs credentials from an error before it can reach a log or a
// last_error column.
//
// Go's *url.Error carries the request URL, and a middleware that ever moves a
// key into a query string would put one there without this package changing.
// Redacting the error text is cheap and does not depend on that never
// happening.
func redactErr(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	redacted := msg
	for _, secret := range secrets {
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "<redacted>")
		}
	}
	if redacted == msg {
		return err
	}
	return errors.New(redacted)
}
