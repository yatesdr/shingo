package www

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"shingocore/deviceconfig"
	"shingocore/fleet"
)

// ── The Robots page's Device Config tab ─────────────────────────────────────
//
// Read is public like every other robots read; the pull sits behind
// requireAuth with the robot controls, because it drives real robot traffic
// through the relay. The matrix is computed SERVER-side (BuildMatrix): the
// plurality/absence/tie rules are the hardest logic in the feature and the
// Go suite is where this repo proves things — the browser only renders
// states (SYNTH-round1, 2-of-3 over client-side computation).

// apiRobotDeviceConfig is GET /api/robots/device-config: the fleet matrix
// over every robot's latest stored pull. A robot never pulled is a column
// of yellow cells; a robot the fleet no longer lists keeps its column from
// its stored pull, because the question the page answers is fleet-wide.
func (h *Handlers) apiRobotDeviceConfig(w http.ResponseWriter, r *http.Request) {
	pulls, err := h.engine.DeviceConfigService().LatestPulls(r.Context())
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	vehicles := make([]string, 0, len(pulls))
	seen := map[string]bool{}
	for _, rs := range h.engine.GetAllCachedRobots() {
		if rs.VehicleID != "" && !seen[rs.VehicleID] {
			seen[rs.VehicleID] = true
			vehicles = append(vehicles, rs.VehicleID)
		}
	}
	extra := []string{}
	for v := range pulls {
		if !seen[v] {
			extra = append(extra, v)
		}
	}
	sort.Strings(extra)
	vehicles = append(vehicles, extra...)

	h.jsonOK(w, deviceconfig.BuildMatrix(vehicles, pulls))
}

// The pull's own guards — single-flight and a server-owned relay gap. The
// gap is a SERVER PROPERTY, not client goodwill (SYNTH-round1): any client,
// any double-click, any script gets at most one relay call per 200 ms.
var (
	deviceConfigPullMu        sync.Mutex // one pull end-to-end, process-wide
	deviceConfigLastRelayUnix atomic.Int64
)

// deviceConfigRelayGap is the minimum distance between #1500 relay starts.
const deviceConfigRelayGap = 200 * time.Millisecond

// waitRelayGap blocks until a relay call may start (the RoboKit request-gap
// the brief's pacing rule demands), claiming the slot atomically.
func waitRelayGap() {
	for {
		now := time.Now().UnixNano()
		last := deviceConfigLastRelayUnix.Load()
		if now-last >= int64(deviceConfigRelayGap) {
			if deviceConfigLastRelayUnix.CompareAndSwap(last, now) {
				return
			}
			continue // someone else claimed the slot; re-wait the remainder
		}
		time.Sleep(time.Duration(int64(deviceConfigRelayGap) - (now - last)))
	}
}

// apiRobotDeviceConfigPull is POST /api/robots/device-config/pull
// {"vehicle":"AMR-03"}: ONE robot end-to-end — relay fetch, parse, sanitize,
// fingerprint, persist — returning that pull's shape. The browser loops
// robots 200 ms apart and refreshes the matrix once at the end (the
// round-1 cut: no streaming; a blocking loop and full reload).
//
// The store write happens HERE, never in the adapter: adapters translate
// wire; policy (sanitize-before-store, the md5 provenance) belongs to the
// handler where the layers meet.
func (h *Handlers) apiRobotDeviceConfigPull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Vehicle string `json:"vehicle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Vehicle == "" {
		h.jsonError(w, "body must be {\"vehicle\": \"AMR-01\"}", http.StatusBadRequest)
		return
	}

	// A backend without the capability (the simulator) is a degraded tab,
	// not an error to paper over: the page gates its Pull button on the
	// same assertion and this answers any client that asks anyway.
	reader, ok := h.engine.Fleet().(fleet.DeviceConfigReader)
	if !ok {
		h.jsonError(w, "this fleet backend cannot pull device configs", http.StatusNotImplemented)
		return
	}

	if !deviceConfigPullMu.TryLock() {
		h.jsonError(w, "a device config pull is already running", http.StatusConflict)
		return
	}
	defer deviceConfigPullMu.Unlock()

	waitRelayGap()
	raw, err := reader.ReadDeviceConfig(body.Vehicle)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	model, err := deviceconfig.Parse(raw)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	// CREDENTIALS NEVER REACH DISK. The live #11500 payload carries the
	// ESP32 mesh wifi password in the clear (measured, evidence/README.md):
	// sanitize the tree FIRST, then flatten from it and marshal it — both
	// JSONB columns are clean by construction, and the compared value of a
	// credential row is its fingerprint.
	sum := md5.Sum(raw)
	sanitized, err := deviceconfig.Sanitize(model)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	leaves := deviceconfig.Leaves(model)

	pulledAt := time.Now()
	if err := h.engine.DeviceConfigService().SavePull(
		r.Context(), body.Vehicle, hex.EncodeToString(sum[:]), pulledAt, leaves, sanitized,
	); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{
		"vehicle":   body.Vehicle,
		"pulled_at": pulledAt,
		"params":    len(leaves),
	})
}
