package www

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// defaultPlantTimezone is what an unconfigured core resolves to.
//
// IT IS UTC BECAUSE VISIBLY WRONG BEATS PLAUSIBLY RIGHT. It used to be
// America/Chicago, which kept both existing plants correct BY ACCIDENT — their
// wall clocks are Central and nobody had to say so. That is a defect dressed as
// a convenience: a plant whose clock is not Central would have rendered every
// timestamp, and every "Today" filter, silently shifted, with nothing on any
// screen to say the zone had never been set. Future sites are expected to be
// Eastern and others, so that day was coming.
//
// UTC is nobody's plant clock, which is the point: an unset zone now shows up
// as an obviously foreign one instead of a subtly wrong local one, and edges
// report their zone to /edges where blank renders as a warn badge.
//
// DEPLOY PRECONDITION, PER PLANT: `timezone:` must be set in that site's
// shingocore.yaml BEFORE this reaches it. A plant still relying on the old
// default lands on UTC — not just in the labels, but in the bare YYYY-MM-DD
// filters below, which would make "Today" start at 19:00 the previous evening.
// Set the key first, then ship the default; never the other way round.
//
// Status 2026-09-08: Hopkinsville is set (America/Chicago). SPRINGFIELD IS NOT
// — it is still running on the old default and is Central, so it needs the key
// before a core deploy carries this there.
const defaultPlantTimezone = "UTC"

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
