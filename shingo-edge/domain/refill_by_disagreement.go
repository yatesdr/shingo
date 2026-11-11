package domain

import (
	"fmt"
	"strings"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// StyleRefillBy is one style's answer to who brings a press's next carrier.
type StyleRefillBy struct {
	Style    string
	RefillBy protocol.RefillBy
}

// RefillByDisagreementFinding is the warning the desktop editor shows on a
// press whose styles disagree about which robot brings its next carrier, and
// the save's answer carries: shown, never refused (the setting describes the
// cell's hardware, so a plant sets it the same on every style, and refusing
// would make it unsettable, since changing a press of four styles takes four
// saves). want is this style's answer and others the styles that differ; no
// others is no finding (ok false).
func RefillByDisagreementFinding(coreNode string, want protocol.RefillBy, others []StyleRefillBy) (NodeFinding, bool) {
	if len(others) == 0 {
		return NodeFinding{}, false
	}
	says := make([]string, 0, len(others))
	for _, o := range others {
		says = append(says, fmt.Sprintf("%s says Robot %d", o.Style, o.RefillBy.RobotNumber()))
	}
	return NodeFinding{
		CoreNodeName: coreNode, Side: flowspec.SideTo, Field: flowspec.RefillBy, Severity: SeverityWarning,
		Message: fmt.Sprintf("This style says Robot %d brings %s its next carrier, and %s. It describes the press's "+
			"robots, so the press runs differently by style: set it the same on every style",
			want.RobotNumber(), coreNode, strings.Join(says, ", ")),
	}, true
}
