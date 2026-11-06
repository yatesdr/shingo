package rds

import (
	"encoding/json"
	"fmt"
	"strings"

	"shingocore/fleet"
)

// APIError is RDS ANSWERING with a refusal — a non-zero envelope code, or an HTTP
// error status — as opposed to a transport failure, where nothing answered. It
// implements fleet.VendorError, which is how dispatch tells "the robot system
// refused this" from "the robot system is not responding" (both read the second
// until 2026-11-04).
//
// Error() is byte-for-byte the text these two paths produced as fmt.Errorf
// strings before the type existed ("rds error %d: %s", "rds HTTP %d: %s"), so log
// lines and anything matching on them read the same.
type APIError struct {
	// HTTPStatus is the response status for an HTTP-level refusal; 0 when the
	// envelope's code carried it.
	HTTPStatus int
	// Code and Msg are RDS's envelope code and msg — parsed from the body for an
	// HTTP refusal when the body is the usual {"code":…,"msg":…} envelope.
	Code int
	Msg  string
	// Body is the raw response body of an HTTP refusal, kept for Error().
	Body string
}

func (e *APIError) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("rds HTTP %d: %s", e.HTTPStatus, e.Body)
	}
	return fmt.Sprintf("rds error %d: %s", e.Code, e.Msg)
}

// VendorCode is RDS's own code, or 0 when an HTTP refusal carried none.
func (e *APIError) VendorCode() int { return e.Code }

// VendorText is RDS's msg; for an HTTP refusal with no envelope it is the
// status and the body, trimmed, so the sentence still names something to look up.
func (e *APIError) VendorText() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.HTTPStatus == 0 {
		return ""
	}
	if body := strings.TrimSpace(e.Body); body != "" {
		return fmt.Sprintf("HTTP %d: %s", e.HTTPStatus, truncate([]byte(body), 80))
	}
	return fmt.Sprintf("HTTP %d", e.HTTPStatus)
}

// APIError is the fleet layer's VendorError for RDS.
var _ fleet.VendorError = (*APIError)(nil)

// httpError builds the APIError for an HTTP refusal, lifting RDS's envelope out
// of the body when there is one.
func httpError(status int, body []byte) *APIError {
	e := &APIError{HTTPStatus: status, Body: string(body)}
	var env Response
	if json.Unmarshal(body, &env) == nil {
		e.Code, e.Msg = env.Code, strings.TrimSpace(env.Msg)
	}
	return e
}
