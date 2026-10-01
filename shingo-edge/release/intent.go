package release

import (
	"encoding/json"
	"fmt"
)

// Intent is an act's promise to one leg for one station wait (SHAPE §3.5),
// stored on the order (orders.release_intent). It is written by the commit for
// every leg the act covers, before any envelope; its SentAt is written in the
// same transaction as the release's outbox row. It is consumed when Core
// reports the leg staged at a later station wait, or the leg ends; a Core
// rejection clears it. A held intent is re-planned by the intent worker on its
// wakes.
type Intent struct {
	StationWait int     `json:"station_wait"`
	Purpose     Purpose `json:"purpose"`
	Origin      Origin  `json:"origin"`
	CalledBy    string  `json:"called_by,omitempty"`
	Choices     Choices `json:"choices"`
	SentAt      string  `json:"sent_at,omitempty"`
	// Resent: the one re-send on a same-wait re-stage (Core's faulted no-op)
	// has been spent.
	Resent bool `json:"resent,omitempty"`
}

// Choices is the operator's disposition as data: what was chosen, never a
// number derived from the counter (those are computed at the envelope).
type Choices struct {
	Mode         string         `json:"mode,omitempty"`
	Captures     map[string]int `json:"captures,omitempty"`
	PartialCount *int           `json:"partial_count,omitempty"`
}

// Sent reports whether the intent's release has gone out.
func (i Intent) Sent() bool { return i.SentAt != "" }

// Encode renders the intent for the orders.release_intent column.
func (i Intent) Encode() (string, error) {
	raw, err := json.Marshal(i)
	if err != nil {
		return "", fmt.Errorf("encode release intent: %w", err)
	}
	return string(raw), nil
}

// DecodeIntent reads an orders.release_intent value; "" is no intent.
func DecodeIntent(raw string) (*Intent, error) {
	if raw == "" {
		return nil, nil
	}
	var i Intent
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		return nil, fmt.Errorf("decode release intent: %w", err)
	}
	return &i, nil
}

// StageEffect is what an OrderStaged report does to a leg's intent: a leg
// staged at a later station wait has consumed it; a leg staged again at the
// same wait after its release went out was refused by Core's fleet (the
// faulted no-op), and the release is re-sent once.
type StageEffect int

const (
	StageKeep    StageEffect = iota // nothing to do
	StageConsume                    // clear the intent
	StageResend                     // re-send the release once
)

// OnStaged decides what Core's OrderStaged at station wait ordinal does to
// the intent. A lane wait (ordinal nil) changes nothing.
func (i *Intent) OnStaged(ordinal *int) StageEffect {
	if i == nil || ordinal == nil {
		return StageKeep
	}
	switch {
	case *ordinal > i.StationWait:
		return StageConsume
	case *ordinal == i.StationWait && i.Sent() && !i.Resent:
		return StageResend
	}
	return StageKeep
}
