package seerrds

import (
	"errors"
	"testing"

	"shingocore/fleet"
	"shingocore/rds"
)

// fakeTrackerEmitter records the last EmitOrderStatusChanged call so tests
// can verify the bridge passes all arguments through untouched and that
// the rds.OrderDetail is converted to a fleet.OrderSnapshot.
type fakeTrackerEmitter struct {
	calls         int
	orderID       int64
	vendorOrderID string
	oldStatus     string
	newStatus     string
	robotID       string
	detail        string
	snapshot      *fleet.OrderSnapshot
}

func (f *fakeTrackerEmitter) EmitOrderStatusChanged(orderID int64, vendorOrderID, oldStatus, newStatus, robotID, detail string, snapshot *fleet.OrderSnapshot) {
	f.calls++
	f.orderID = orderID
	f.vendorOrderID = vendorOrderID
	f.oldStatus = oldStatus
	f.newStatus = newStatus
	f.robotID = robotID
	f.detail = detail
	f.snapshot = snapshot
}

// TestEmitterBridge_ForwardsArgsAndMapsSnapshot verifies the bridge passes
// every scalar through unchanged and maps the *rds.OrderDetail to a
// *fleet.OrderSnapshot with correct fields.
func TestEmitterBridge_ForwardsArgsAndMapsSnapshot(t *testing.T) {
	fake := &fakeTrackerEmitter{}
	b := &emitterBridge{emitter: fake}

	detail := &rds.OrderDetail{
		ID:      "order-88",
		Vehicle: "AMB-02",
		State:   rds.StateRunning,
		Blocks: []rds.BlockDetail{
			{BlockID: "blk-1", Location: "STATION-A", State: rds.StateRunning},
		},
		Errors: []rds.OrderMessage{{Code: 5, Desc: "minor", Times: 1, Timestamp: 123}},
	}

	b.EmitOrderStatusChanged(
		42,                          // orderID
		"order-88",                  // rdsOrderID
		string(rds.StateCreated),    // oldStatus
		string(rds.StateRunning),    // newStatus
		"AMB-02",                    // robotID
		"state transitioned",        // detail
		detail,
	)

	if fake.calls != 1 {
		t.Fatalf("emitter call count = %d, want 1", fake.calls)
	}
	if fake.orderID != 42 {
		t.Errorf("orderID = %d, want 42", fake.orderID)
	}
	if fake.vendorOrderID != "order-88" {
		t.Errorf("vendorOrderID = %q, want order-88", fake.vendorOrderID)
	}
	if fake.oldStatus != string(rds.StateCreated) {
		t.Errorf("oldStatus = %q, want CREATED", fake.oldStatus)
	}
	if fake.newStatus != string(rds.StateRunning) {
		t.Errorf("newStatus = %q, want RUNNING", fake.newStatus)
	}
	if fake.robotID != "AMB-02" {
		t.Errorf("robotID = %q, want AMB-02", fake.robotID)
	}
	if fake.detail != "state transitioned" {
		t.Errorf("detail = %q, want 'state transitioned'", fake.detail)
	}

	// Snapshot must be mapped, not forwarded as the raw rds type.
	if fake.snapshot == nil {
		t.Fatal("snapshot = nil, want non-nil fleet.OrderSnapshot")
	}
	if fake.snapshot.VendorOrderID != "order-88" {
		t.Errorf("snapshot.VendorOrderID = %q, want order-88", fake.snapshot.VendorOrderID)
	}
	if fake.snapshot.State != string(rds.StateRunning) {
		t.Errorf("snapshot.State = %q, want RUNNING", fake.snapshot.State)
	}
	if fake.snapshot.Vehicle != "AMB-02" {
		t.Errorf("snapshot.Vehicle = %q, want AMB-02", fake.snapshot.Vehicle)
	}
	if len(fake.snapshot.Blocks) != 1 || fake.snapshot.Blocks[0].BlockID != "blk-1" {
		t.Errorf("snapshot.Blocks = %+v, want single block blk-1", fake.snapshot.Blocks)
	}
	if len(fake.snapshot.Errors) != 1 || fake.snapshot.Errors[0].Code != 5 {
		t.Errorf("snapshot.Errors = %+v, want single msg code=5", fake.snapshot.Errors)
	}
}

// TestEmitterBridge_NilDetailPassesNilSnapshot verifies that when the poller
// passes nil for the order detail, the bridge forwards nil rather than a
// zero-valued snapshot.
func TestEmitterBridge_NilDetailPassesNilSnapshot(t *testing.T) {
	fake := &fakeTrackerEmitter{}
	b := &emitterBridge{emitter: fake}

	b.EmitOrderStatusChanged(7, "order-7", "", string(rds.StateFailed), "", "gone", nil)

	if fake.calls != 1 {
		t.Fatalf("emitter call count = %d, want 1", fake.calls)
	}
	if fake.snapshot != nil {
		t.Errorf("snapshot = %+v, want nil when orderDetail is nil", fake.snapshot)
	}
	if fake.newStatus != string(rds.StateFailed) {
		t.Errorf("newStatus = %q, want FAILED", fake.newStatus)
	}
}

// TestEmitterBridge_MultipleCalls verifies per-call state isolation — each
// call replaces the captured fields rather than aggregating.
func TestEmitterBridge_MultipleCalls(t *testing.T) {
	fake := &fakeTrackerEmitter{}
	b := &emitterBridge{emitter: fake}

	b.EmitOrderStatusChanged(1, "a", "", string(rds.StateRunning), "", "", nil)
	b.EmitOrderStatusChanged(2, "b", string(rds.StateRunning), string(rds.StateFinished), "AMB-X", "done",
		&rds.OrderDetail{ID: "b", State: rds.StateFinished})

	if fake.calls != 2 {
		t.Fatalf("calls = %d, want 2", fake.calls)
	}
	if fake.orderID != 2 || fake.vendorOrderID != "b" {
		t.Errorf("last-call ids = (%d, %q), want (2, b)", fake.orderID, fake.vendorOrderID)
	}
	if fake.snapshot == nil || fake.snapshot.State != string(rds.StateFinished) {
		t.Errorf("last-call snapshot = %+v, want State=FINISHED", fake.snapshot)
	}
}

// fakeOrderIDResolver lets tests drive ResolveVendorOrderID with either a
// success response or a synthetic error, and counts invocations.
type fakeOrderIDResolver struct {
	calls   int
	lastArg string
	ret     int64
	err     error
}

func (f *fakeOrderIDResolver) ResolveVendorOrderID(vendorOrderID string) (int64, error) {
	f.calls++
	f.lastArg = vendorOrderID
	return f.ret, f.err
}

// TestResolverBridge_ForwardsAndReturns verifies the bridge forwards the
// rds order ID string unmodified and returns whatever the underlying
// fleet.OrderIDResolver returns.
func TestResolverBridge_ForwardsAndReturns(t *testing.T) {
	fake := &fakeOrderIDResolver{ret: 12345}
	b := &resolverBridge{resolver: fake}

	got, err := b.ResolveRDSOrderID("rds-order-xyz")
	if err != nil {
		t.Fatalf("ResolveRDSOrderID err = %v, want nil", err)
	}
	if got != 12345 {
		t.Errorf("orderID = %d, want 12345", got)
	}
	if fake.calls != 1 {
		t.Errorf("resolver call count = %d, want 1", fake.calls)
	}
	if fake.lastArg != "rds-order-xyz" {
		t.Errorf("resolver lastArg = %q, want rds-order-xyz", fake.lastArg)
	}
}

// TestResolverBridge_PropagatesError verifies that a resolver error is
// returned unwrapped to the caller — the bridge must not swallow it.
func TestResolverBridge_PropagatesError(t *testing.T) {
	sentinel := errors.New("not found")
	fake := &fakeOrderIDResolver{ret: 0, err: sentinel}
	b := &resolverBridge{resolver: fake}

	got, err := b.ResolveRDSOrderID("missing")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel %v", err, sentinel)
	}
	if got != 0 {
		t.Errorf("orderID = %d, want 0 when resolver errors", got)
	}
	if fake.lastArg != "missing" {
		t.Errorf("resolver lastArg = %q, want 'missing'", fake.lastArg)
	}
}

// TestResolverBridge_EmptyInput documents current behavior: an empty rds
// order ID is forwarded verbatim rather than rejected at the bridge layer.
func TestResolverBridge_EmptyInput(t *testing.T) {
	fake := &fakeOrderIDResolver{ret: 0}
	b := &resolverBridge{resolver: fake}

	_, _ = b.ResolveRDSOrderID("")
	if fake.calls != 1 {
		t.Errorf("resolver call count = %d, want 1 (bridge should not short-circuit)", fake.calls)
	}
	if fake.lastArg != "" {
		t.Errorf("resolver lastArg = %q, want empty string", fake.lastArg)
	}
}

// Compile-time assertions that the bridge types still satisfy the rds-side
// interfaces. If these break, the poller will refuse to accept them.
var (
	_ rds.PollerEmitter   = (*emitterBridge)(nil)
	_ rds.OrderIDResolver = (*resolverBridge)(nil)
)
