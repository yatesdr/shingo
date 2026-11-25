package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// CancelIntent is a cancel this station asked Core for and Core has not yet
// ended (orders.cancel_intent, JSON; "" is none). The final brief's Part 5: a
// cancel of an order whose status is Core's versioned word writes this and
// its order.cancel in one transaction, and the row keeps Core's status until
// Core's versioned word ends it; the terminal write clears it in the
// statement that clears release_intent. Each field names its reader.
type CancelIntent struct {
	// Reason and Actor are the cancel's as sent (protocol.OrderCancel): the
	// resend on the reconcile's live word sends both again (orders
	// resendCancel), so an idle rule's cancel sent again stays the Edge's.
	Reason string `json:"reason"`
	Actor  string `json:"actor,omitempty"`
	// By is who asked, for what follows Core's end of the order: CancelByPerson
	// writes the person's pause there (orders coreEnded); CancelByDecided
	// leaves the keep-staged keeper's next pass to the flow that decided the
	// order's spot (engine handleNodeOrderCompleted). "" for the other doors.
	By string `json:"by,omitempty"`
	// Env is the id of the envelope that carried the cancel last: Core's
	// refusal is a reply to it (its CorID), and only a refusal that matches it
	// is written (orders.RefuseCancel).
	Env string `json:"env"`
	// SentAt is when the cancel was asked (RFC 3339, UTC): the Orders page's
	// clock beside "Cancel sent", and the resend's age.
	SentAt string `json:"sent_at"`
	// Refused is Core's sentence when it answered that it cannot cancel
	// (an order.error refusal): the station JSON's cancel_refused and the
	// Orders page's line under the row. "" while unanswered.
	Refused string `json:"refused,omitempty"`
}

// The two askers whose follow-on reads By.
const (
	// CancelByPerson is the Orders page's cancel: a person said stop.
	CancelByPerson = "person"
	// CancelByDecided is a cancel a changeover's START or CANCEL asked for an
	// order whose spot it decides itself: the keep-staged keeper's pass is
	// the flow's, not the cancel's end's.
	CancelByDecided = "decided"
)

// Unanswered reports whether the cancel is out and Core has said nothing
// that ended or refused it.
func (c CancelIntent) Unanswered() bool { return c.Env != "" && c.Refused == "" }

// SentTime is SentAt read, or the zero time when it does not read.
func (c CancelIntent) SentTime() time.Time {
	t, err := time.Parse(time.RFC3339Nano, c.SentAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Encode renders the intent for the orders.cancel_intent column.
func (c CancelIntent) Encode() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode cancel intent: %w", err)
	}
	return string(raw), nil
}

// DecodeCancelIntent reads an orders.cancel_intent value; "" is none.
func DecodeCancelIntent(raw string) (*CancelIntent, error) {
	if raw == "" {
		return nil, nil
	}
	var c CancelIntent
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("decode cancel intent: %w", err)
	}
	return &c, nil
}
