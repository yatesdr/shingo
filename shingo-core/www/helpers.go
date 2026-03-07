package www

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shingocore/fleet"
)

func (h *Handlers) jsonOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (h *Handlers) jsonSuccess(w http.ResponseWriter) {
	h.jsonOK(w, map[string]string{"status": "ok"})
}

func (h *Handlers) jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// parseIDParam extracts an int64 ID from query param or form value.
// Returns 0, false on error (writes JSON 400 response).
func (h *Handlers) parseIDParam(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	s := r.URL.Query().Get(key)
	if s == "" {
		s = r.FormValue(key)
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		h.jsonError(w, "invalid "+key, http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// parseJSON decodes the request body as JSON into dst.
// Returns false on error (writes JSON 400 response).
func (h *Handlers) parseJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		h.jsonError(w, "invalid request", http.StatusBadRequest)
		return false
	}
	return true
}

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
		"formatTime": func(t time.Time) template.HTML {
			if t.IsZero() {
				return template.HTML("-")
			}
			return template.HTML(`<time data-utc="` + t.UTC().Format(time.RFC3339) + `">` +
				t.UTC().Format("2006-01-02 15:04:05") + ` UTC</time>`)
		},
		"formatTimePtr": func(t *time.Time) template.HTML {
			if t == nil {
				return template.HTML("-")
			}
			return template.HTML(`<time data-utc="` + t.UTC().Format(time.RFC3339) + `">` +
				t.UTC().Format("2006-01-02 15:04:05") + ` UTC</time>`)
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
		"robotState": func(r fleet.RobotStatus) string {
			return r.State()
		},
		"pct": func(f float64) string {
			return fmt.Sprintf("%.0f", f)
		},
		"f1": func(f float64) string {
			return fmt.Sprintf("%.1f", f)
		},
		"splitBroker": func(broker string) [2]string {
			parts := strings.SplitN(broker, ":", 2)
			if len(parts) == 2 {
				return [2]string{parts[0], parts[1]}
			}
			return [2]string{broker, "9093"}
		},
		"nodeColor": func(count, _ int) string {
			return "" // Styling handled via tile state CSS classes
		},
	}
}
