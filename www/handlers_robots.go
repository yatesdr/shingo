package www

import (
	"log"
	"net/http"
)

func (h *Handlers) handleRobots(w http.ResponseWriter, r *http.Request) {
	robots, err := h.engine.RDSClient().GetRobotsStatus()
	if err != nil {
		log.Printf("robots: RDS error: %v", err)
	}
	data := map[string]any{
		"Page":          "robots",
		"Robots":        robots,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "robots.html", data)
}
