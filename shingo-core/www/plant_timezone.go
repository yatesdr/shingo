package www

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"shingo/shared/planttime"
)

// An unconfigured core resolves to planttime.DefaultZone. The resolution itself
// is planttime.ResolveZone, which the engine reads too, so the cleaning loop's
// plant-local active hours and every screen here are on one clock.
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

// plantLocation is the plant's IANA timezone, resolved once from the
// PLANT_TIMEZONE env var (default UTC, planttime.DefaultZone above), then from
// the yaml `timezone:` at router construction. The dashboards follow a
// plant-local-at-server convention (Q-004): timestamps are stored UTC, but
// bare YYYY-MM-DD date filters from the URL resolve in THIS zone — so "Today"
// means the plant's calendar day, not the server's (which runs UTC). Without
// this, a CST plant on a UTC server saw "Today" start at 6pm the prior day.
//
// Display rendering is plant-local for every viewer (shared/planttime), and
// plantLocation is the one resolution point for it on core: date filters,
// day truncation, and the formatTime template helper all read this var.
var plantLocation = loadPlantLocation()

// plantZone is plantLocation with where it came from, so a screen can say
// whether the plant's zone was ever set (the Cleaning tab's active hours mean
// nothing on a default UTC). Written beside plantLocation, never apart.
var plantZone = planttime.ResolveZone("")

func loadPlantLocation() *time.Location {
	z := planttime.ResolveZone("")
	if z.Invalid != "" {
		log.Printf("www: PLANT_TIMEZONE %q invalid; falling back to UTC", z.Invalid)
	}
	return z.Loc
}

// applyPlantTimezoneConfig folds the yaml `timezone:` field in at router
// construction, before any request is served. Precedence: PLANT_TIMEZONE
// env (applied at package init — kept working so existing deployments
// don't move on upgrade), then the config field, then the default. The log
// names the SOURCE so a wrong zone is diagnosable from the journal —
// "which knob made this clock" is the first question at a plant.
func applyPlantTimezoneConfig(cfgTimezone string) {
	z := planttime.ResolveZone(cfgTimezone)
	switch {
	case z.Source == planttime.SourceEnv || os.Getenv(planttime.EnvZone) != "":
		log.Printf("www: plant timezone %s (from PLANT_TIMEZONE env)", z.Loc)
	case z.Invalid != "":
		log.Printf("www: config timezone %q invalid; keeping %s", z.Invalid, z.Loc)
	case z.Source == planttime.SourceDefault:
		log.Printf("www: plant timezone %s (default; set timezone: in shingocore.yaml)", z.Loc)
	default:
		log.Printf("www: plant timezone %s (from config)", z.Loc)
	}
	plantLocation, plantZone = z.Loc, z
}

// OfferedPlantTimezone is the plant zone Core offers its Edges on every
// heartbeat ack (main.go → CoreDataService.SetPlantTimezone): the zone Core was
// EXPLICITLY told, by the same precedence applyPlantTimezoneConfig uses for
// Core's own screens. PLANT_TIMEZONE when set, else the yaml `timezone:`, else
// "" — never planttime.DefaultZone, which is nobody's answer
// (protocol.EdgeHeartbeatAck.Timezone says why). So Core's screens and the
// Edges with no zone of their own show the same clock (C9).
//
// An invalid PLANT_TIMEZONE is offered as written: Core's screens fall back to
// UTC, and an Edge refuses a zone that does not parse (adoptPlantTimezone), so
// no default goes out in its place.
func OfferedPlantTimezone(cfgTimezone string) string {
	if env := os.Getenv("PLANT_TIMEZONE"); env != "" {
		return env
	}
	return cfgTimezone
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
