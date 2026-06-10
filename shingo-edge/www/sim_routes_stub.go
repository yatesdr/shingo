//go:build !sim

package www

import "github.com/go-chi/chi/v5"

// registerSimRoutes is a no-op in production builds; the dev sim control
// endpoints exist only under -tags sim (see sim_routes.go).
func (h *Handlers) registerSimRoutes(r chi.Router) {}
