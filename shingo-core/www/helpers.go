package www

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shingo/protocol"
	"shingo/shared"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/engine"
	"shingocore/fleet"
)

// binMoveStatus turns a refused bin move into the status it deserves.
//
// The engine decides which of the three kinds a refusal is, because that is a
// judgement about the plant — a full destination is a conflict, a mistyped node
// is a bad request — and the sentence a person reads is built in the same
// place. This is the whole of what the HTTP layer adds: a number.
//
// Anything that is not a BinMoveError is a fault by definition; it came from
// somewhere that did not classify itself.
func binMoveStatus(err error) int {
	var bm *engine.BinMoveError
	if errors.As(err, &bm) {
		switch bm.Kind {
		case engine.BinMoveBadRequest:
			return http.StatusBadRequest
		case engine.BinMoveConflict:
			return http.StatusConflict
		}
	}
	return http.StatusInternalServerError
}

// resolveDropoff loads the node something is being sent to, answering the
// request itself and returning nil if the name is not one.
//
// Naming a node that does not exist is the caller getting their input wrong,
// so it is a 400. The engineer's door returns a plain error for the same
// failure and its wrapper turns everything unrecognised into a 500 — a typo
// there reports that the server is broken.
func (h *Handlers) resolveDropoff(w http.ResponseWriter, nodeName string) *domain.Node {
	destNode, err := h.engine.NodeService().GetByName(nodeName)
	if err != nil {
		h.jsonError(w, "destination node not found: "+nodeName, http.StatusBadRequest)
		return nil
	}
	return destNode
}

// iconSpriteHTML is the vendored Lucide sprite (shared/icons.svg), read once at
// init and injected into layout.html via {{iconSprite}} so page markup can
// reference symbols with <use href="#icon-…">. It's a trusted first-party asset,
// so template.HTML (not user data) is safe here.
var iconSpriteHTML = func() template.HTML {
	b, err := fs.ReadFile(shared.Files, "icons.svg")
	if err != nil {
		return ""
	}
	return template.HTML(b)
}()

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

// canCancelStatus reports whether a cancel/terminate control should render
// for an order in this status. It is engine.TerminateOrder's own rejection
// gate, so a button can only appear where the handler would actually accept
// it. Exposed to templates as {{canCancel}} and to the enriched-order API as
// `can_cancel`, so the server-rendered list rows and the client-rendered
// manifest agree by construction.
//
// This replaced hand-listed status denylists that had drifted from the
// engine: the list view missed "skipped" (terminal, so Cancel was a dead
// button on every skipped row) and the detail view also left Terminate up
// for delivered and confirmed. Deriving it from the predicates means adding
// a status to protocol/status.go can't reopen the gap.
func canCancelStatus(s protocol.Status) bool {
	return !dispatch.IsPostDelivery(s) && !protocol.IsTerminal(s)
}

// canHardReleaseOrder reports whether the Core-side hard release (W3) should
// render for this order — computed here, like canCancelStatus, so the button
// cannot appear where the handler would refuse it.
//
// ── ONLY FOR A WAIT CORE OWNS ─────────────────────────────────────────────
//
// A STATION-owned wait is released from the station's own board, by the person
// who can see whether the cell is clear. Offering a Core-side override for one
// would invite an engineer to advance a robot into a cell somebody is working
// in, from a screen that cannot show them the cell — and it would do it in the
// one case where the ordinary path is not even broken.
//
// So the hatch is scoped to the waits Core is responsible for advancing: a lane
// wait whose evaluator is wedged is a wedge only Core can clear, and that is
// exactly the shape this stream kept meeting.
//
// An untagged wait (pre-ruling plan, still draining) reads as station-owned and
// therefore does NOT get the button — the conservative direction, and consistent
// with dispatch.IsStationWait, which is the single place that rule lives.
//
// AND THAT SCOPING IS WHAT KEEPS THE SEQUENTIAL SHAPE OUT OF THIS HATCH. Every
// position of a sequential A/B changeover opens with a station wait at its own
// node, held until the operator releases it from the board in front of him —
// which is the guarded click, the one that refuses to strip a position the line
// is still pulling from. A Core-side hard release would be that click with the
// guard removed and the aisle out of sight. Nothing special is written here for
// it: it is station-owned, so it is already out.
func canHardReleaseOrder(o *domain.Order) bool {
	if o == nil || o.Status != protocol.StatusStaged {
		return false
	}
	return dispatch.CoreOwnsWaitAt(o.StepsJSON, o.WaitIndex)
}

// stationNamer resolves an opaque station identity to the operator's label.
// Satisfied by *service.NodeService; an interface here so the template layer
// does not reach into the service package for one method, and so tests can
// render pages without a database.
type stationNamer interface {
	StationName(station string) string
}

// templateFuncs builds the shared FuncMap.
//
// namer may be nil — that is the no-database case (tests, template parse
// checks) and it renders raw station ids, which is exactly what the fallback
// does when a station has no registry row. There is deliberately no separate
// "no namer" rendering path to keep in sync with the real one.
func templateFuncs(namer stationNamer) template.FuncMap {
	return template.FuncMap{
		"simMode":    simModeEnabled, // dev speed top-strip gate; false in prod builds
		"cacheBust":  func() string { return fmt.Sprintf("%x", time.Now().UnixNano()) },
		"iconSprite": func() template.HTML { return iconSpriteHTML },
		"canCancel":  canCancelStatus,

		// stationName renders the operator's label for a station identity.
		//
		// USE THIS FOR AN EDGE STATION AND NOTHING ELSE. A SEER fleet station
		// (mission_events.robot_station, mission-detail.html's "Station"
		// column) is a different namespace that must never be looked up here.
		// Core's own order sources — core-operator, core-direct, core-test — and
		// the '*' broadcast address have no registry row and fall through to
		// themselves, which is correct and is why there is no error case.
		"stationName": func(station string) string {
			if namer == nil {
				return station
			}
			return namer.StationName(station)
		},
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
			switch protocol.Status(status) {
			case protocol.StatusPending, protocol.StatusSourcing:
				return "bg-yellow-100 text-yellow-800"
			case protocol.StatusDispatched:
				return "bg-blue-100 text-blue-800"
			case protocol.StatusInTransit:
				return "bg-indigo-100 text-indigo-800"
			case protocol.StatusDelivered, protocol.StatusConfirmed:
				return "bg-green-100 text-green-800"
			case protocol.StatusFailed:
				return "bg-red-100 text-red-800"
			case protocol.StatusCancelled:
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
		// f2 prints a localization confidence exactly as the vendor publishes
		// it. Two decimals and no rescaling to a percentage: the figure comes
		// from an upstream system and an operator comparing this tile against
		// RoboShop should see the same number, not a derived one.
		//
		// NEGATIVE ZERO IS NORMALISED, and it is not hypothetical: Springfield
		// AMR-04 reported -0 fifteen times in the first two minutes of
		// collection, while driving and on task. IEEE keeps the sign, so
		// "%.2f" renders it "-0.00" — which reads as a broken display rather
		// than as the genuine near-total loss of localization it represents,
		// and the first thing anyone does with a display they distrust is
		// ignore it. The STORED value keeps its sign; only the rendering is
		// normalised. (-0 == 0 is true, so this catches exactly that case and
		// leaves every real value alone.)
		"f2": func(f float64) string {
			if f == 0 {
				f = 0
			}
			return fmt.Sprintf("%.2f", f)
		},
		// confidenceBand maps a localization confidence onto the health-chip
		// vocabulary. The cuts are the VENDOR'S OWN operator thresholds
		// (rds-user-manual.pdf: >0.8 green, >0.3 yellow, else red), so this
		// HMI and the fleet manager agree rather than offering a second
		// opinion about the same number.
		//
		// Reuses .chip-ok/.chip-near/.chip-below rather than styling a new
		// pill: their inks are the measured --chip-ink-* values (4.60–4.63:1
		// worst case) and a hand-rolled colour would not be. Per the chip
		// ruling in docs/ui-style-guide.md, the pill is only exempt from the
		// 3:1 boundary floor because it PRINTS ITS VALUE — the number inside
		// is the meaning, and this must never become an icon-only chip.
		"confidenceBand": func(f float64) string {
			switch {
			case f >= 0.80:
				return "chip-ok"
			case f >= 0.30:
				return "chip-near"
			default:
				return "chip-below"
			}
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
