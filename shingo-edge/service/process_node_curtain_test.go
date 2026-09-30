package service

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// The curtain interlock's node setter refuses what the gate would refuse on
// every release: an enabled interlock with no chosen polarity, or with a
// pointer missing. A refused write stamps nothing; a disabled write stores
// whatever it is given, trimmed.
func TestSetNodeCurtain_Refusals(t *testing.T) {
	db := testdb.Open(t)
	svc := NewProcessService(db)

	pid, err := db.CreateProcess("SYN-CURTAIN-SVC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nid, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "SYN-FG-SVC-1", Name: "SYN-FG-SVC-1", Enabled: true})
	testutil.MustNoErr(t, err, "create node")
	f := false

	if err := svc.SetNodeCurtain(nid, true, "PRESS-PLC", "FG_CURTAIN", nil); !errors.Is(err, processes.ErrCurtainPolarityUnset) {
		t.Errorf("enabled with nil polarity: err = %v, want ErrCurtainPolarityUnset", err)
	}
	if err := svc.SetNodeCurtain(nid, true, "  ", "FG_CURTAIN", &f); !errors.Is(err, processes.ErrCurtainPointersMissing) {
		t.Errorf("enabled with a blank PLC: err = %v, want ErrCurtainPointersMissing", err)
	}
	n, err := db.GetProcessNode(nid)
	testutil.MustNoErr(t, err, "get node")
	if n.CurtainEnabled || n.CurtainPLCName != "" || n.CurtainTagName != "" || n.CurtainSafeValue != nil {
		t.Fatalf("refused writes stamped the node: %+v", n)
	}

	// Disabled with no polarity is fine: nothing is gated.
	testutil.MustNoErr(t, svc.SetNodeCurtain(nid, false, " PRESS-PLC ", " FG_CURTAIN ", nil), "disabled, polarity unset")
	n, err = db.GetProcessNode(nid)
	testutil.MustNoErr(t, err, "get node")
	if n.CurtainEnabled || n.CurtainPLCName != "PRESS-PLC" || n.CurtainTagName != "FG_CURTAIN" || n.CurtainSafeValue != nil {
		t.Fatalf("disabled write = %+v, want trimmed pointers, polarity unset", n)
	}
}
