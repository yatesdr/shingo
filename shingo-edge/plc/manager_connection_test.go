package plc

import (
	"context"
	"io"
	"testing"

	"shingoedge/config"
)

type mockWarlinkClient struct {
	plcs []WarlinkPLC
	tags map[string]map[string]WarlinkTag
}

func (m *mockWarlinkClient) ListPLCs(ctx context.Context) ([]WarlinkPLC, error) {
	return m.plcs, nil
}

func (m *mockWarlinkClient) ListTags(ctx context.Context, plcName string) (map[string]WarlinkTag, error) {
	return m.tags[plcName], nil
}

func (m *mockWarlinkClient) ListAllTags(ctx context.Context, plcName string) ([]WarlinkTagInfo, error) {
	return nil, nil
}

func (m *mockWarlinkClient) SetTagPublishing(ctx context.Context, plcName, tagName string, enabled bool) error {
	return nil
}

func (m *mockWarlinkClient) ReadTagValue(ctx context.Context, plcName, tagName string) (interface{}, error) {
	if t, ok := m.tags[plcName][tagName]; ok {
		return t.Value, nil
	}
	return nil, nil
}

func (m *mockWarlinkClient) WriteTagValue(ctx context.Context, plcName, tagName string, value interface{}) error {
	return nil
}

func (m *mockWarlinkClient) OpenEventStream(ctx context.Context) (io.ReadCloser, error) {
	return nil, nil
}

func TestWarlinkPollTreatsConnectionLevelTagErrorsAsDisconnected(t *testing.T) {
	cfg := config.Defaults()
	emitter := &mockEmitter{}
	mgr := NewManager(nil, cfg, emitter)
	mgr.wl = &mockWarlinkClient{
		plcs: []WarlinkPLC{{Name: "logix_L7", Status: "Connected"}},
		tags: map[string]map[string]WarlinkTag{
			"logix_L7": {
				"logix_L7.Access_Card_Last_Record": {
					PLC:   "logix_L7",
					Name:  "logix_L7.Access_Card_Last_Record",
					Type:  "DINT",
					Error: "ReadMultiple: SendUnitDataTransaction: not connected",
				},
			},
		},
	}

	mgr.warlinkPollTick()

	if mgr.IsConnected("logix_L7") {
		t.Fatal("expected plc to be treated as disconnected")
	}
	if _, err := mgr.ReadTag("logix_L7", "Access_Card_Last_Record"); err == nil {
		t.Fatal("expected disconnected read to fail")
	}
}

func TestConnectionErrorFromTagsRequiresAllTagsToFailAtConnectionLevel(t *testing.T) {
	tags := map[string]WarlinkTag{
		"a": {Error: "ReadMultiple: SendUnitDataTransaction: not connected"},
		"b": {Value: 12},
	}
	if err := connectionErrorFromTags(tags); err != "" {
		t.Fatalf("expected mixed tags not to be treated as plc disconnect, got %q", err)
	}
}
