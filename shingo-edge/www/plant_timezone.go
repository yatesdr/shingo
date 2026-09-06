package www

import (
	"log"
	"time"

	"shingoedge/config"
	"shingoedge/engine"
)

// plantLocation is edge's single resolution point for the plant's IANA
// timezone, mirroring core's www/plant_timezone.go. Edge's value comes from
// shingoedge.yaml `timezone:` — the SAME key engine.BucketLocation reads for
// hourly-count bucketing, so display and reporting cannot disagree about
// which clock a station is on.
//
// THE LIVE FINDING THIS CLOSES (BRIEF-plant-local-display-2026-09-05 §4):
// both plants' edge configs carry `timezone: ""`, so BucketLocation fell
// back to time.Local — and at Hopkinsville the Pi's OS zone is Eastern
// while the plant wall clock is Central, which has hourly counts bucketing
// one hour off the plant day. Empty here falls back to UTC, NOT time.Local:
// an unconfigured edge then shows UTC (visibly wrong, diagnosable from the
// zone label) rather than silently adopting whatever zone the box happens
// to sit in.
//
// Resolved once in NewRouter, before templates parse; the boot log names
// the source so "which knob made this clock" is answerable from journald.
var plantLocation = time.UTC

// resolvePlantLocation is NewRouter's resolution step. cfg may be nil in
// tests; that keeps UTC, the honest answer for "no config".
func resolvePlantLocation(cfg *config.Config) *time.Location {
	if cfg == nil || cfg.Timezone == "" {
		log.Printf("www: plant timezone UTC (unset; set timezone: in shingoedge.yaml)")
		return time.UTC
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		log.Printf("www: config timezone %q invalid (%v); falling back to UTC", cfg.Timezone, err)
		return time.UTC
	}
	return loc
}

// engineTimezone reads the engine's config, tolerating the test engines
// that construct without one.
func engineTimezone(eng *engine.Engine) string {
	if eng == nil || eng.AppConfig() == nil {
		return ""
	}
	return eng.AppConfig().Timezone
}
