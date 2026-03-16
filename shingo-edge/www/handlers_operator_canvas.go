package www

// handlers_operator_canvas.go — Operator Canvas handlers for Shingo Edge
//
// Unified architecture: uses the andon v4-style canvas renderer (render.js,
// shapes.js, display.js) for all operator screens.
//
// Two modes of operation:
//   1. DESIGNED screens  — saved layout JSON (via the drag-and-drop designer)
//   2. AUTO-GENERATED    — hit /operator/cell/{lineID}, handler builds a default
//      layout from the line's active job-style payloads. No design step needed.
//      This is the "training wheels" entry point for Kurt's operators.
//
// Both modes render through the same operator-display.html template and
// the same canvas renderer + SSE plumbing.
//
// ── Route Registration (add to router.go NewRouter()) ────────────────
//
//  Public pages:
//    r.Get("/operator", h.handleOperatorScreenList)
//    r.Get("/operator/cell/{id}", h.handleOperatorCellDisplay)
//    r.Get("/operator/display/{id}", h.handleOperatorDisplay)
//    r.Get("/operator/designer", h.handleOperatorDesigner)
//
//  Public API (inside r.Route("/api", ...)):
//    r.Get("/orders/active", h.apiGetActiveOrders)
//
//  Admin API (inside admin group, inside r.Route("/api", ...)):
//    r.Post("/operator-screens", h.apiCreateOperatorScreen)
//    r.Get("/operator-screens", h.apiListOperatorScreens)
//    r.Get("/operator-screens/{id}/layout", h.apiGetOperatorScreenLayout)
//    r.Put("/operator-screens/{id}/layout", h.apiSaveOperatorScreenLayout)
//
// ── Nav link (add to header.html) ───────────────────────────────────
//
//   <a href="/operator" class="{{if eq .Page "operator"}}active{{end}}">Operator</a>

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"shingoedge/store"
)

// ── Page Handlers ────────────────────────────────────────────────────

// handleOperatorScreenList shows all production lines (quick launch) plus
// any saved designer screens.
func (h *Handlers) handleOperatorScreenList(w http.ResponseWriter, r *http.Request) {
	lines, err := h.engine.DB().ListProductionLines()
	if err != nil {
		log.Printf("operator: failed to list lines: %v", err)
	}
	screens, err := h.engine.DB().ListOperatorScreens()
	if err != nil {
		log.Printf("operator: failed to list screens: %v", err)
	}
	data := map[string]interface{}{
		"Page":    "operator",
		"Lines":   lines,
		"Screens": screens,
	}
	h.renderTemplate(w, "operator-home.html", data)
}

// handleOperatorCellDisplay auto-generates a screen layout from a
// production line's active job-style payloads and renders it through
// the standard andon canvas display.
//
// Route: GET /operator/cell/{id}   (id = production line ID)
//
// This is the "training wheels" entry point — no designer step required.
func (h *Handlers) handleOperatorCellDisplay(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()

	lineID, err := parseID(r, "id")
	if err != nil {
		http.Error(w, "invalid line ID", http.StatusBadRequest)
		return
	}

	line, err := db.GetProductionLine(lineID)
	if err != nil {
		http.Error(w, "line not found", http.StatusNotFound)
		return
	}
	if line.ActiveJobStyleID == nil {
		http.Error(w, "no active job style — set one in Setup first", http.StatusBadRequest)
		return
	}

	payloads, err := db.ListPayloadsByJobStyle(*line.ActiveJobStyleID)
	if err != nil {
		http.Error(w, "failed to load payloads", http.StatusInternalServerError)
		return
	}
	if len(payloads) == 0 {
		http.Error(w, "no payloads on this job style", http.StatusBadRequest)
		return
	}

	layout := generateDefaultLayout(line, payloads)

	screen := &store.OperatorScreen{
		ID:     0, // ephemeral — not persisted
		Name:   line.Name,
		Slug:   fmt.Sprintf("cell-%d", line.ID),
		Layout: layout,
	}

	data := map[string]interface{}{
		"Page":   "operator-display",
		"Screen": screen,
	}
	h.renderTemplate(w, "operator-display.html", data)
}

// handleOperatorDisplay serves a saved (designed) screen.
// Route: GET /operator/display/{id}   (id = screen ID)
func (h *Handlers) handleOperatorDisplay(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		http.Error(w, "invalid screen id", http.StatusBadRequest)
		return
	}
	screen, err := h.engine.DB().GetOperatorScreen(id)
	if err != nil {
		http.Error(w, "screen not found", http.StatusNotFound)
		return
	}
	data := map[string]interface{}{
		"Page":   "operator-display",
		"Screen": screen,
	}
	h.renderTemplate(w, "operator-display.html", data)
}

// handleOperatorDesigner serves the drag-and-drop screen editor.
func (h *Handlers) handleOperatorDesigner(w http.ResponseWriter, r *http.Request) {
	screenIDStr := r.URL.Query().Get("screen")
	var screen *store.OperatorScreen
	if screenIDStr != "" {
		id, err := strconv.ParseInt(screenIDStr, 10, 64)
		if err == nil {
			screen, _ = h.engine.DB().GetOperatorScreen(id)
		}
	}
	lines, _ := h.engine.DB().ListProductionLines()
	payloads, _ := h.engine.DB().ListPayloads()
	data := map[string]interface{}{
		"Page":     "operator-designer",
		"Screen":   screen,
		"Lines":    lines,
		"Payloads": payloads,
	}
	h.renderTemplate(w, "operator-designer.html", data)
}

// ── API Handlers ─────────────────────────────────────────────────────

func (h *Handlers) apiGetActiveOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.DB().ListActiveOrders()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, orders)
}

func (h *Handlers) apiCreateOperatorScreen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string          `json:"name"`
		Layout json.RawMessage `json:"layout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	slug := fmt.Sprintf("screen-%d", time.Now().UnixNano())
	screenID, err := h.engine.DB().CreateOperatorScreen(req.Name, slug, req.Layout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	screen, err := h.engine.DB().GetOperatorScreen(screenID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, screen)
}

func (h *Handlers) apiListOperatorScreens(w http.ResponseWriter, r *http.Request) {
	screens, err := h.engine.DB().ListOperatorScreens()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, screens)
}

func (h *Handlers) apiGetOperatorScreenLayout(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	screen, err := h.engine.DB().GetOperatorScreen(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(screen.Layout)
}

func (h *Handlers) apiSaveOperatorScreenLayout(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var layout json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&layout); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.engine.DB().UpdateOperatorScreenLayout(id, layout); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"ok":true}`)
}

// ── Auto-layout generator ────────────────────────────────────────────
//
// Builds the same shapes JSON that the designer would save:
//   - 1 Header (top banner with line name)
//   - 1 StatusBar (bottom bar with connection indicator)
//   - N OrderCombo cards (one per payload, auto-positioned)
//
// Card layout rules:
//   - ≤4 payloads → single row
//   - >4 payloads → two rows, ceil(n/2) per row
//   - Cards/fonts scale down as count increases
//   - Centered in the 1920×1080 canvas between header and status bar

const canvasW = 1920
const canvasH = 1080

func generateDefaultLayout(line *store.ProductionLine, payloads []store.Payload) json.RawMessage {
	type shapeConfig map[string]interface{}
	type shape struct {
		ID     string      `json:"id"`
		Type   string      `json:"type"`
		Config shapeConfig `json:"config"`
	}

	var shapes []shape

	headerH := 90.0
	statusBarH := 45.0

	// ── Header ──────────────────────────────────────────────────
	shapes = append(shapes, shape{
		ID:   uuid.NewString(),
		Type: "header",
		Config: shapeConfig{
			"x": 0, "y": 0, "w": canvasW, "h": headerH,
			"text": line.Name,
			"textX": 0.5, "textY": 0.5,
		},
	})

	// ── StatusBar ───────────────────────────────────────────────
	shapes = append(shapes, shape{
		ID:   uuid.NewString(),
		Type: "statusbar",
		Config: shapeConfig{
			"x": 0, "y": canvasH - statusBarH, "w": canvasW, "h": statusBarH,
			"lineName": line.Name,
			"lineId":   line.ID,
			"styleName": "",
		},
	})

	// ── OrderCombo cards ────────────────────────────────────────
	n := len(payloads)
	gap := 20.0
	usableW := float64(canvasW) - gap*2
	usableH := float64(canvasH) - headerH - statusBarH - gap*2
	startY := headerH + gap

	var rows, perRow int
	if n <= 4 {
		rows = 1
		perRow = n
	} else {
		rows = 2
		perRow = int(math.Ceil(float64(n) / 2.0))
	}

	maxCardW := 580.0
	maxCardH := 700.0
	cardW := math.Min(maxCardW, (usableW-gap*float64(perRow-1))/float64(perRow))
	cardH := math.Min(maxCardH, (usableH-gap*float64(rows-1))/float64(rows))

	for i, p := range payloads {
		row := i / perRow
		col := i % perRow

		// How many cards in this row (last row may have fewer)
		cardsInRow := perRow
		if row == rows-1 && n%perRow != 0 && rows > 1 {
			cardsInRow = n % perRow
		}

		rowW := float64(cardsInRow)*cardW + float64(cardsInRow-1)*gap
		offsetX := (float64(canvasW) - rowW) / 2.0
		offsetY := startY + (usableH-float64(rows)*cardH-float64(rows-1)*gap)/2.0

		cx := offsetX + float64(col)*(cardW+gap)
		cy := offsetY + float64(row)*(cardH+gap)

		desc := p.Description
		if desc == "" {
			desc = p.Location
		}

		pct := 0.0
		if p.ProductionUnits > 0 {
			pct = float64(p.Remaining) / float64(p.ProductionUnits) * 100.0
		}

		shapes = append(shapes, shape{
			ID:   uuid.NewString(),
			Type: "ordercombo",
			Config: shapeConfig{
				"x": cx, "y": cy, "w": cardW, "h": cardH,
				"payloadId":       p.ID,
				"payloadCode":     p.PayloadCode,
				"description":     desc,
				"lineId":          line.ID,
				"payloadStatus":   p.Status,
				"remainingPct":    pct,
				"remaining":       p.Remaining,
				"total":           p.ProductionUnits,
				"orderStatus":     "",
				"orderETA":        "",
				"actionLabel":     "REQUEST",
				"actionType":      "retrieve",
				"retrieveEmpty":   p.RetrieveEmpty,
				"backgroundColor": "#1E1E1E",
			},
		})
	}

	data, _ := json.Marshal(shapes)
	return data
}

