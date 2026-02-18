package www

import (
	"net/http"
)

func (h *Handlers) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	auditLog, _ := h.engine.DB().ListAuditLog(50)

	rdsOK := false
	rdsVersion := ""
	if ping, err := h.engine.RDSClient().Ping(); err == nil && ping != nil {
		rdsOK = true
		rdsVersion = ping.Version
	}

	msgOK := h.engine.MsgClient().IsConnected()
	pollerCount := h.engine.Poller().ActiveCount()

	data := map[string]any{
		"Page":          "diagnostics",
		"AuditLog":      auditLog,
		"RDSOK":         rdsOK,
		"RDSVersion":    rdsVersion,
		"MessagingOK":   msgOK,
		"PollerActive":  pollerCount,
		"SSEClients":    h.eventHub.ClientCount(),
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "diagnostics.html", data)
}
