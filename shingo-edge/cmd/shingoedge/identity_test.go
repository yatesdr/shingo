package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/messaging"
)

// identity_test.go — a new Edge just works: the boot mints its station uid
// when the file has none, writes it, and keeps it across restarts; Core's
// register request (a heartbeat from a uid Core has no row for) makes it
// register and reconcile under that uid. Core's half: the register handler
// introduces the row (TestHandleEdgeRegister_ANewEdgeIsIntroducedAndThenKnown).

var stationUIDShape = regexp.MustCompile(`^stn-[0-9a-f]{16}$`)

// freshEdgeConfig is the file a fresh install leaves: brokers and a database,
// no station_uid.
func freshEdgeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shingoedge.yaml")
	body := "database_path: " + filepath.ToSlash(filepath.Join(t.TempDir(), "shingoedge.db")) + "\n" +
		"messaging:\n  kafka:\n    brokers: [\"broker.test:9092\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// bootIdentity is the boot's identity step as main runs it: load the file,
// ensureIdentity, read the one value everything after it is handed.
func bootIdentity(t *testing.T, path string) (*config.Config, string) {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	ensureIdentity(cfg, path)
	return cfg, cfg.StationID()
}

// TestEnsureIdentity_AFreshEdgeMintsOnceAndKeepsIt: a file with no uid boots
// with a minted stn- uid and the file then carries it; the next boot reads it
// back and neither mints nor writes; a second fresh box draws another uid.
func TestEnsureIdentity_AFreshEdgeMintsOnceAndKeepsIt(t *testing.T) {
	t.Parallel()
	path := freshEdgeConfig(t)
	if cfg, err := config.Load(path); err != nil || cfg.StationID() != "" {
		t.Fatalf("the fresh file loads with identity %q (err %v), want none: a default would make the mint "+
			"untestable", cfg.StationID(), err)
	}

	_, uid := bootIdentity(t, path)
	if !stationUIDShape.MatchString(uid) {
		t.Fatalf("first boot runs as %q, want a minted stn- and 16 hex", uid)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if saved.StationUID != uid {
		t.Fatalf("the file carries station_uid %q after the first boot, want the minted %q: an Edge that does not "+
			"write its mint is a new station on every restart", saved.StationUID, uid)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	_, again := bootIdentity(t, path)
	if again != uid {
		t.Errorf("the restart runs as %q, want %q", again, uid)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if string(after) != string(written) || !info2.ModTime().Equal(info.ModTime()) {
		t.Error("the restart rewrote the config: a boot with a uid writes nothing")
	}

	if _, other := bootIdentity(t, freshEdgeConfig(t)); other == uid || !stationUIDShape.MatchString(other) {
		t.Errorf("a second fresh box runs as %q beside %q, want its own stn- uid", other, uid)
	}
}

// inboundRecorder is the heartbeater the inbound router drives, counting what
// it is asked to send.
type inboundRecorder struct{ registers, nodeSyncs, catalogSyncs int }

func (r *inboundRecorder) SendRegister()       { r.registers++ }
func (r *inboundRecorder) RequestNodeSync()    { r.nodeSyncs++ }
func (r *inboundRecorder) RequestCatalogSync() { r.catalogSyncs++ }

// TestInbound_ARegisterRequestRegistersAndReconcilesAsTheMintedUID: Core's
// edge.register_request (what a heartbeat from a uid it has no row for draws)
// reaches the router main builds, and the Edge sends one register and then its
// order reconcile, from the uid it minted at boot.
func TestInbound_ARegisterRequestRegistersAndReconcilesAsTheMintedUID(t *testing.T) {
	t.Parallel()
	cfg, uid := bootIdentity(t, freshEdgeConfig(t))
	if !stationUIDShape.MatchString(uid) {
		t.Fatalf("boot runs as %q, want a minted uid", uid)
	}
	eng := engine.New(engine.Config{AppConfig: cfg, DB: testdb.Open(t), LogFunc: func(string, ...any) {}})
	var sent []*protocol.Envelope
	eng.SetSendFunc(func(env *protocol.Envelope) error {
		sent = append(sent, env)
		return nil
	})
	hb := &inboundRecorder{}
	inbound := buildInboundRouter(eng, messaging.NewEdgeHandler(eng.OrderManager()), hb, func(string, ...any) {})

	env, err := protocol.NewDataEnvelope(protocol.SubjectEdgeRegisterRequest,
		protocol.Address{Role: protocol.RoleCore, Station: "core"},
		protocol.Address{Role: protocol.RoleEdge, Station: uid},
		&protocol.EdgeRegisterRequest{Reason: "station not enrolled"})
	if err != nil {
		t.Fatalf("build register request: %v", err)
	}
	inbound.Dispatch(env, env.Type)

	if hb.registers != 1 {
		t.Errorf("registers sent = %d, want 1", hb.registers)
	}
	var reconciles int
	for _, e := range sent {
		if e.Src.Station != uid {
			t.Errorf("%s left as %q, want the minted %q", e.Type, e.Src.Station, uid)
		}
		var d protocol.Data
		if err := e.DecodePayload(&d); err == nil && d.Subject == protocol.SubjectOrderStatusRequest {
			reconciles++
		}
	}
	if reconciles != 1 {
		t.Errorf("order reconciles sent = %d of %d envelopes, want 1", reconciles, len(sent))
	}
}
