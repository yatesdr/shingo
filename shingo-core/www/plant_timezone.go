package www

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// plantLocation is the plant's IANA timezone, resolved once from the
// PLANT_TIMEZONE env var (default America/Chicago). The dashboards follow a
// plant-local-at-server convention (Q-004): timestamps are stored UTC, but
// bare YYYY-MM-DD date filters from the URL resolve in THIS zone — so "Today"
// means the plant's calendar day, not the server's (which runs UTC). Without
// this, a CST plant on a UTC server saw "Today" start at 6pm the prior day.
var plantLocation = loadPlantLocation()

func loadPlantLocation() *time.Location {
	name := os.Getenv("PLANT_TIMEZONE")
	if name == "" {
		name = "America/Chicago"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Printf("www: PLANT_TIMEZONE %q invalid (%v); falling back to UTC", name, err)
		return time.UTC
	}
	return loc
}

// apiPlantTimezone returns the configured plant timezone so the frontend can
// label date ranges ("Today (CST)") and, eventually, defer window math to the
// server.
func (h *Handlers) apiPlantTimezone(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"tz": plantLocation.String()})
}
