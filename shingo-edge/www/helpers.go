package www

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
)

// resolveProcessFromQuery reads the "process" query param and returns the
// matching process, falling back to the first process if none specified.
func resolveProcessFromQuery(r *http.Request, processes []domain.Process) *domain.Process {
	if param := r.URL.Query().Get("process"); param != "" {
		if id, err := strconv.ParseInt(param, 10, 64); err == nil {
			for i := range processes {
				if processes[i].ID == id {
					return &processes[i]
				}
			}
		}
	}
	if len(processes) > 0 {
		return &processes[0]
	}
	return nil
}

// loadAnomalyData loads unconfirmed anomalies and builds a reporting point map
// for display in the global anomaly popover. Used by all page handlers.
func loadAnomalyData(h *Handlers) ([]domain.CounterSnapshot, map[int64]map[string]string) {
	anomalies, _ := h.engine.CounterService().ListUnconfirmedAnomalies()
	reportingPoints, _ := h.engine.CounterService().ListReportingPoints()

	rpMap := make(map[int64]map[string]string)
	for _, rp := range reportingPoints {
		rpMap[rp.ID] = map[string]string{
			"PLCName": rp.PLCName,
			"TagName": rp.TagName,
		}
	}

	return anomalies, rpMap
}

// templateFuncs builds the template FuncMap.
//
// Lifted out of NewRouter verbatim (2026-08-19): it was 54 lines and eleven
// closures declared inline in the middle of route registration, which made the
// router read as though template plumbing were part of the routing table. Core
// has had this as a named function in its own helpers.go all along; this is the
// same shape. No closure body changed.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"join": strings.Join,
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "..."
		},
		"divPercent": func(a, b int) float64 {
			if b == 0 {
				return 0
			}
			return float64(a) / float64(b) * 100
		},
		"deref": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"brokerHost": func(s string) string {
			if i := strings.LastIndex(s, ":"); i >= 0 {
				return s[:i]
			}
			return s
		},
		"brokerPort": func(s string) string {
			if i := strings.LastIndex(s, ":"); i >= 0 {
				return s[i+1:]
			}
			return ""
		},
		"buildVer":  func() string { return buildVer },
		"cacheBust": func() string { return fmt.Sprintf("%x", time.Now().UnixNano()) },
		"formatTime": func(t time.Time) template.HTML {
			if t.IsZero() {
				return template.HTML("")
			}
			return template.HTML(`<time data-utc="` + t.UTC().Format(time.RFC3339) + `">` +
				t.UTC().Format("2006-01-02 15:04:05") + ` UTC</time>`)
		},
		"formatTimePtr": func(t *time.Time) template.HTML {
			if t == nil {
				return template.HTML("")
			}
			return template.HTML(`<time data-utc="` + t.UTC().Format(time.RFC3339) + `">` +
				t.UTC().Format("2006-01-02 15:04:05") + ` UTC</time>`)
		},
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		// faultLine renders a faulted order's sentence and live clock via
		// protocol.BuildFaultLine — the same builder Core's board uses, so the
		// two cannot disagree. Empty for anything not faulted.
		"faultLine": func(o domain.Order) template.HTML {
			if o.Status != protocol.StatusFaulted {
				return template.HTML("")
			}
			var ref protocol.TermRef
			if o.FaultRef != nil {
				ref = *o.FaultRef
			}
			var since, deadline time.Time
			if o.FaultSince != nil {
				since = *o.FaultSince
			}
			if o.FaultDeadline != nil {
				deadline = *o.FaultDeadline
			}
			line := protocol.BuildFaultLine(ref, since, deadline, time.Now().UTC(),
				time.Duration(o.FaultNoticeAfterS)*time.Second)
			// Safe: FaultLine.HTML escapes every interpolated value.
			return template.HTML(line.HTML())
		},
	}
}
