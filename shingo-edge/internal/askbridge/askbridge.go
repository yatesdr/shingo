// Package askbridge is the one table the Edge's test bridges carry asks
// with: the kinds a test's stub Core answers over HTTP, and the machine
// door's path each is posted to (Core answers each act through that door's
// one body, so the stub's answer to the path is Core's answer to the ask).
// The engine's and www's bridges (core_bridge_test.go in each) both read it;
// they kept two copies before. engine's TestAskKinds_EveryKindIsSentAndDeclared
// pins its keys inside protocol.AskKinds.
package askbridge

import "shingo/protocol"

// Paths is the bridged kinds, each with its door's path.
var Paths = map[string]string{
	protocol.AskBinLoad:     "/api/telemetry/bin-load",
	protocol.AskBinCount:    "/api/telemetry/bin-count",
	protocol.AskBinClear:    "/api/telemetry/bin-clear",
	protocol.AskQualityHold: "/api/telemetry/bin-quality-hold",
	protocol.AskClearHome:   "/api/telemetry/clear-home",
}
