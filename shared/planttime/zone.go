package planttime

import (
	"os"
	"time"
)

// EnvZone is the environment variable that names Core's plant zone ahead of
// the yaml `timezone:` key. Kept so deployments that set it never move.
const EnvZone = "PLANT_TIMEZONE"

// DefaultZone is what an unconfigured Core resolves to. UTC because visibly
// wrong beats plausibly right: an unset zone shows up as nobody's plant clock
// rather than as a subtly shifted local one. The reasoning and the per-plant
// deploy precondition are on www's plantLocation.
const DefaultZone = "UTC"

// Where a Zone came from.
const (
	SourceEnv     = "env"
	SourceConfig  = "config"
	SourceDefault = "default"
)

// Zone is Core's resolved plant timezone and the knob that produced it.
//
// It lives here, not in www, because the engine needs the same answer: the
// cleaning loop's active hours are plant-local, and a second resolver in the
// engine would be a second authority that disagrees with every screen the
// first time someone sets PLANT_TIMEZONE. www's plantLocation and the engine
// both call ResolveZone.
type Zone struct {
	Loc    *time.Location
	Source string // SourceEnv, SourceConfig or SourceDefault
	// Invalid is a zone name that was given but did not load; Loc is then the
	// fallback and Source says which.
	Invalid string
}

// Set reports whether someone named the plant's zone and it loaded. A Zone
// that is not Set is UTC by default, which is nobody's wall clock, so a
// plant-local window read against it means nothing.
func (z Zone) Set() bool { return z.Source != SourceDefault }

// ResolveZone resolves Core's plant zone: PLANT_TIMEZONE when it is set, else
// cfgTimezone (the yaml `timezone:`), else UTC.
//
// An env var that is set but does not load falls back to UTC and does NOT
// consult the yaml: the env var is the operator's explicit override, and
// quietly substituting the config's answer for a typo in it would hide the
// typo. A yaml zone that does not load falls back the same way.
//
// Each call loads the location afresh, so a caller that resolves on every
// tick sees a config change without a restart.
func ResolveZone(cfgTimezone string) Zone {
	if env := os.Getenv(EnvZone); env != "" {
		if loc, err := time.LoadLocation(env); err == nil {
			return Zone{Loc: loc, Source: SourceEnv}
		}
		return Zone{Loc: time.UTC, Source: SourceDefault, Invalid: env}
	}
	if cfgTimezone == "" {
		return Zone{Loc: time.UTC, Source: SourceDefault}
	}
	loc, err := time.LoadLocation(cfgTimezone)
	if err != nil {
		return Zone{Loc: time.UTC, Source: SourceDefault, Invalid: cfgTimezone}
	}
	return Zone{Loc: loc, Source: SourceConfig}
}
