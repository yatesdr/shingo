package www

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"shingocore/rds"
)

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"timeAgo": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				m := int(d.Minutes())
				if m == 1 {
					return "1 minute ago"
				}
				return fmt.Sprintf("%d minutes ago", m)
			case d < 24*time.Hour:
				h := int(d.Hours())
				if h == 1 {
					return "1 hour ago"
				}
				return fmt.Sprintf("%d hours ago", h)
			default:
				days := int(d.Hours() / 24)
				if days == 1 {
					return "1 day ago"
				}
				return fmt.Sprintf("%d days ago", days)
			}
		},
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Format("2006-01-02 15:04:05")
		},
		"formatTimePtr": func(t *time.Time) string {
			if t == nil {
				return "-"
			}
			return t.Format("2006-01-02 15:04:05")
		},
		"statusColor": func(status string) string {
			switch status {
			case "pending", "sourcing":
				return "bg-yellow-100 text-yellow-800"
			case "dispatched":
				return "bg-blue-100 text-blue-800"
			case "in_transit":
				return "bg-indigo-100 text-indigo-800"
			case "delivered", "confirmed", "completed":
				return "bg-green-100 text-green-800"
			case "failed":
				return "bg-red-100 text-red-800"
			case "cancelled":
				return "bg-gray-100 text-gray-800"
			default:
				return "bg-gray-100 text-gray-800"
			}
		},
		"nodeTypeIcon": func(nodeType string) string {
			switch nodeType {
			case "storage":
				return "archive"
			case "line_side":
				return "inbox"
			case "staging":
				return "layers"
			case "charging":
				return "battery-charging"
			default:
				return "map-pin"
			}
		},
		"upper": strings.ToUpper,
		"lower": strings.ToLower,
		"add": func(a, b int) int {
			return a + b
		},
		"deref": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"robotState": func(r rds.RobotStatus) string {
			if r.ConnectionStatus == 0 {
				return "offline"
			}
			if r.IsError || r.RbkReport.Emergency || r.RbkReport.Blocked {
				return "error"
			}
			if r.ProcBusiness {
				return "busy"
			}
			if !r.Dispatchable {
				return "paused"
			}
			return "ready"
		},
		"pct": func(f float64) string {
			return fmt.Sprintf("%.0f", f)
		},
		"f1": func(f float64) string {
			return fmt.Sprintf("%.1f", f)
		},
		"payloadStatusColor": func(status string) string {
			switch status {
			case "available":
				return "badge-available"
			case "in_transit":
				return "badge-in_transit"
			case "at_line":
				return "badge-at_line"
			case "empty":
				return "badge-empty"
			case "hold":
				return "badge-hold"
			default:
				return ""
			}
		},
		"nodeColor": func(count, capacity int) string {
			if capacity <= 0 || count <= 0 {
				return "background:#e9ecef;color:#495057;"
			}
			f := float64(count) / float64(capacity)
			if f > 1 {
				f = 1
			}
			// Interpolate gray (#e9ecef) → purple (#7c3aed)
			r := int(233 - 109*f)
			g := int(236 - 178*f)
			b := int(239 - 2*f)
			tc := "#495057"
			if f > 0.45 {
				tc = "#fff"
			}
			return fmt.Sprintf("background:rgb(%d,%d,%d);color:%s;", r, g, b, tc)
		},
	}
}
