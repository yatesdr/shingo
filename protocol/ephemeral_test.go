package protocol_test

import (
	"bytes"
	"encoding/json"
	"log"
	"slices"
	"testing"
	"time"

	"shingo/protocol"
)

// ephemeralKey is the shared signing key both sides of these tests hold.
var ephemeralKey = []byte("ephemeral-test-key")

// positionsFor builds Core's robot.positions for one station, as Core's poll
// does: NewEnvelope on the type (its exp from EphemeralTTL), then encoded and
// signed as the messaging client publishes it.
func positionsFor(t *testing.T, station string, body *protocol.RobotPositions) (*protocol.Envelope, []byte) {
	t.Helper()
	env, err := protocol.NewEnvelope(protocol.TypeRobotPositions,
		protocol.Address{Role: protocol.RoleCore, Station: "core"},
		protocol.Address{Role: protocol.RoleEdge, Station: station}, body)
	if err != nil {
		t.Fatalf("build robot.positions: %v", err)
	}
	return env, signed(t, env)
}

func signed(t *testing.T, env *protocol.Envelope) []byte {
	t.Helper()
	raw, err := env.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := protocol.Sign(raw, ephemeralKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return out
}

// forStation is the Edge's filter: robot.positions addressed to it, nothing
// else.
func forStation(station string) protocol.FilterFunc {
	return func(h *protocol.RawHeader) bool {
		return h.Type == protocol.TypeRobotPositions && h.Dst.Station == station
	}
}

// TestEphemeral_PositionsRoundTripSignedAndAddressed pins the wire both ends
// share: what Core's poll builds is what the addressed Edge decodes, path order
// and all, with t at millisecond precision and the watch's shape as D3 names it.
func TestEphemeral_PositionsRoundTripSignedAndAddressed(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 9, 30, 0, 123_456_789, time.UTC).Truncate(time.Millisecond)
	body := &protocol.RobotPositions{T: at, IntervalMS: 2000, Orders: []protocol.RobotPosition{
		{OrderUUID: "u-1", RobotID: "AMR-01", X: 1.25, Y: -3.5, Heading: 1.5708, Path: []string{"LM7", "LM31", "AP32"}},
		{OrderUUID: "u-2", RobotID: "AMR-02", X: 0, Y: 0, Heading: 0},
	}}
	_, wire := positionsFor(t, "edge.a", body)

	env, ok := protocol.DecodeEphemeral(wire, ephemeralKey, forStation("edge.a"))
	if !ok {
		t.Fatal("the addressed Edge dropped a fresh, signed robot.positions")
	}
	var got protocol.RobotPositions
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !got.T.Equal(at) || got.IntervalMS != 2000 || len(got.Orders) != 2 {
		t.Fatalf("decoded %+v, want t=%s interval 2000 and two orders", got, at)
	}
	if !slices.Equal(got.Orders[0].Path, []string{"LM7", "LM31", "AP32"}) || got.Orders[0].RobotID != "AMR-01" {
		t.Errorf("first order = %+v, want AMR-01 on [LM7 LM31 AP32] in that order", got.Orders[0])
	}

	// The wire keys, as D3 spells them: a pathless robot carries no path key,
	// and t carries milliseconds and no more.
	var raw struct {
		P struct {
			T      string           `json:"t"`
			Orders []map[string]any `json:"orders"`
		} `json:"p"`
	}
	inner, err := protocol.VerifyAndUnwrap(wire, ephemeralKey)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if err := json.Unmarshal(inner, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if raw.P.T != "2026-11-14T09:30:00.123Z" {
		t.Errorf("t on the wire = %q, want 2026-11-14T09:30:00.123Z", raw.P.T)
	}
	for _, k := range []string{"order_uuid", "robot_id", "x", "y", "heading", "path"} {
		if _, ok := raw.P.Orders[0][k]; !ok {
			t.Errorf("an order with a route has no %q key: %v", k, raw.P.Orders[0])
		}
	}
	if _, has := raw.P.Orders[1]["path"]; has {
		t.Errorf("a robot with no route carries a path key: %v", raw.P.Orders[1])
	}

	// The watch, the other direction, decoded by Core's filter.
	w, err := protocol.NewEnvelope(protocol.TypeRobotWatch,
		protocol.Address{Role: protocol.RoleEdge, Station: "edge.a"},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.RobotWatch{StationID: "edge.a", OrderUUIDs: []string{"u-1", "u-2"}})
	if err != nil {
		t.Fatalf("build robot.watch: %v", err)
	}
	wenv, ok := protocol.DecodeEphemeral(signed(t, w), ephemeralKey, func(h *protocol.RawHeader) bool {
		return h.Type == protocol.TypeRobotWatch && h.Dst.Role == protocol.RoleCore
	})
	if !ok {
		t.Fatal("Core dropped a fresh, signed robot.watch")
	}
	var watch protocol.RobotWatch
	if err := wenv.DecodePayload(&watch); err != nil || watch.StationID != "edge.a" ||
		!slices.Equal(watch.OrderUUIDs, []string{"u-1", "u-2"}) || wenv.Src.Station != "edge.a" {
		t.Errorf("robot.watch decoded as %+v from %q (%v), want edge.a watching [u-1 u-2]", watch, wenv.Src.Station, err)
	}
}

// TestEphemeral_ReadableForTenSecondsAfterItWasStamped pins the owner's 10 s
// expiry as behaviour: a message read 9 s after it was stamped is taken, one
// read 11 s after is dropped. The message is aged by moving its own stamps
// back, which is what a late read looks like to the reader's clock.
func TestEphemeral_ReadableForTenSecondsAfterItWasStamped(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		age  time.Duration
		want bool
	}{{0, true}, {9 * time.Second, true}, {11 * time.Second, false}, {time.Hour, false}} {
		env, _ := positionsFor(t, "edge.a", &protocol.RobotPositions{IntervalMS: 2000, Orders: []protocol.RobotPosition{}})
		env.Timestamp = env.Timestamp.Add(-c.age)
		env.ExpiresAt = env.ExpiresAt.Add(-c.age)
		if _, ok := protocol.DecodeEphemeral(signed(t, env), ephemeralKey, forStation("edge.a")); ok != c.want {
			t.Errorf("read %s after its stamp: taken = %v, want %v", c.age, ok, c.want)
		}
	}
	w, err := protocol.NewEnvelope(protocol.TypeRobotWatch,
		protocol.Address{Role: protocol.RoleEdge, Station: "edge.a"}, protocol.Address{Role: protocol.RoleCore},
		&protocol.RobotWatch{StationID: "edge.a"})
	if err != nil {
		t.Fatalf("build robot.watch: %v", err)
	}
	if got := w.ExpiresAt.Sub(w.Timestamp); got != protocol.EphemeralTTL {
		t.Errorf("robot.watch is stamped to expire %s after it was made, want the same %s as robot.positions", got, protocol.EphemeralTTL)
	}
}

// TestEphemeral_EveryDropIsSilent pins D4: each message a reader of the topic
// drops — past its exp, with no exp, for another station, of the other type,
// unsigned, signed with another key, not JSON — is dropped with no log line.
// The shared Ingestor logs an expired message and a bad signature; this
// reader must not. Not parallel: it captures the process log.
func TestEphemeral_EveryDropIsSilent(t *testing.T) {
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })

	fresh := func() *protocol.Envelope {
		env, _ := positionsFor(t, "edge.a", &protocol.RobotPositions{IntervalMS: 2000, Orders: []protocol.RobotPosition{}})
		return env
	}
	expired := fresh()
	expired.ExpiresAt = time.Now().Add(-time.Second)
	noExp := fresh()
	noExp.ExpiresAt = time.Time{}
	raw, err := fresh().Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	otherKey, err := protocol.Sign(raw, []byte("another key"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	watch, err := protocol.NewEnvelope(protocol.TypeRobotWatch,
		protocol.Address{Role: protocol.RoleEdge, Station: "edge.b"},
		protocol.Address{Role: protocol.RoleEdge, Station: "edge.a"}, &protocol.RobotWatch{StationID: "edge.b"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	drops := protocol.ExpiredDrops()
	for name, wire := range map[string][]byte{
		"past its exp":        signed(t, expired),
		"no exp":              signed(t, noExp),
		"another station's":   func() []byte { _, w := positionsFor(t, "edge.b", &protocol.RobotPositions{}); return w }(),
		"the other type":      signed(t, watch),
		"unsigned":            raw,
		"signed with another": otherKey,
		"not JSON":            []byte("\x00garbage"),
	} {
		if _, ok := protocol.DecodeEphemeral(wire, ephemeralKey, forStation("edge.a")); ok {
			t.Errorf("%s message was taken, want dropped", name)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("dropping ephemeral messages wrote to the log:\n%s", buf.String())
	}
	if got := protocol.ExpiredDrops(); got != drops {
		t.Errorf("an expired ephemeral message moved the durable topics' expired-drop count (%d -> %d)", drops, got)
	}
	if _, ok := protocol.DecodeEphemeral(signed(t, fresh()), ephemeralKey, forStation("edge.a")); !ok {
		t.Error("control: a fresh message addressed to edge.a was dropped")
	}
}
