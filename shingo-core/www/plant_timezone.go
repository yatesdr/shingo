package www

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// defaultPlantTimezone is the zone a config that predates the `timezone:`
// key resolves to. Both plants' wall clocks are Central, so this keeps
// Hopkinsville correct today only by default — the install script seeds
// the explicit key so the correctness stops being an accident.
const defaultPlantTimezone = "America/Chicago"

// plantLocation is the plant's IANA timezone, resolved once from the
// PLANT_TIMEZONE env var (default America/Chicago). The dashboards follow a
// plant-local-at-server convention (Q-004): timestamps are stored UTC, but
// bare YYYY-MM-DD date filters from the URL resolve in THIS zone — so "Today"
// means the plant's calendar day, not the server's (which runs UTC). Without
// this, a CST plant on a UTC server saw "Today" start at 6pm the prior day.
//
// Display rendering is plant-local for every viewer (shared/planttime), and
// plantLocation is the one resolution point for it on core: date filters,
// day truncation, and the formatTime template helper all read this var.
var plantLocation = loadPlantLocation()

func loadPlantLocation() *time.Location {
	name := os.Getenv("PLANT_TIMEZONE")
	if name == "" {
		name = defaultPlantTimezone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Printf("www: PLANT_TIMEZONE %q invalid (%v); falling back to UTC", name, err)
		return time.UTC
	}
	return loc
}

// applyPlantTimezoneConfig folds the yaml `timezone:` field in at router
// construction, before any request is served. Precedence: PLANT_TIMEZONE
// env (applied at package init — kept working so existing deployments
// don't move on upgrade), then the config field, then the default. The log
// names the SOURCE so a wrong zone is diagnosable from the journal —
// "which knob made this clock" is the first question at a plant.
func applyPlantTimezoneConfig(cfgTimezone string) {
	if env := os.Getenv("PLANT_TIMEZONE"); env != "" {
		log.Printf("www: plant timezone %s (from PLANT_TIMEZONE env)", plantLocation)
		return
	}
	if cfgTimezone == "" {
		log.Printf("www: plant timezone %s (default; set timezone: in shingocore.yaml)", plantLocation)
		return
	}
	loc, err := time.LoadLocation(cfgTimezone)
	if err != nil {
		log.Printf("www: config timezone %q invalid (%v); keeping %s", cfgTimezone, err, plantLocation)
		return
	}
	plantLocation = loc
	log.Printf("www: plant timezone %s (from config)", plantLocation)
}

// plantDayStart truncates t to midnight in the plant timezone. parseMissionFilter
// normalizes its date filters to UTC, so truncating in the raw (UTC) location
// lands on the wrong calendar day for a non-UTC plant — e.g. "today 23:59
// plant-local" (≈05:00Z) truncates to tomorrow's UTC midnight. Always reduce in
// plantLocation so the resulting day matches the plant's calendar.
func plantDayStart(t time.Time) time.Time {
	t = t.In(plantLocation)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, plantLocation)
}

// apiPlantTimezone returns the configured plant timezone so the frontend can
// label date ranges ("Today (CST)") and, eventually, defer window math to the
// server.
func (h *Handlers) apiPlantTimezone(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"tz": plantLocation.String()})
}
