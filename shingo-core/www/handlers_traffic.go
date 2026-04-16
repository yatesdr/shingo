package www

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"shingocore/config"
)

func (h *Handlers) handleTraffic(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()
	cfg.Lock()
	groups := make([]map[string]any, len(cfg.CountGroups.Groups))
	for i, g := range cfg.CountGroups.Groups {
		groups[i] = map[string]any{
			"Name":    g.Name,
			"Enabled": g.Enabled,
		}
	}
	cfg.Unlock()

	data := map[string]any{
		"Page":   "traffic",
		"Groups": groups,
		"Saved":  r.URL.Query().Get("saved"),
		"Error":  r.URL.Query().Get("err"),
	}
	h.render(w, r, "traffic.html", data)
}

// apiTrafficGroups returns the current count-group list as JSON.
func (h *Handlers) apiTrafficGroups(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()
	cfg.Lock()
	groups := cfg.CountGroups.Groups
	cfg.Unlock()
	h.jsonOK(w, groups)
}

// handleTrafficSave handles POST /traffic/save — replaces the count_groups.groups
// list from the form, writes the YAML, and hot-reloads the Runner.
func (h *Handlers) handleTrafficSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Collect groups from form. The template generates indexed fields:
	//   group_name_0, group_enabled_0, group_name_1, ...
	// A blank name means the row was deleted or empty — skip it.
	var groups []config.CountGroupConfig
	for i := 0; ; i++ {
		key := "group_name_" + strconv.Itoa(i)
		name := strings.TrimSpace(r.FormValue(key))
		if !r.Form.Has(key) {
			break
		}
		if name == "" {
			continue
		}
		enabled := r.FormValue("group_enabled_"+strconv.Itoa(i)) == "on"
		groups = append(groups, config.CountGroupConfig{Name: name, Enabled: enabled})
	}

	// Update config
	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CountGroups.Groups = groups
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		log.Printf("traffic: save error: %v", err)
		http.Error(w, "Failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.ReconfigureCountGroups()

	log.Printf("traffic: saved %d groups", len(groups))
	http.Redirect(w, r, "/traffic?saved=groups", http.StatusSeeOther)
}

// handleTrafficAdd handles POST /traffic/add — appends a new group with
// the given name (enabled=true) and redirects back.
func (h *Handlers) handleTrafficAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/traffic?err=empty", http.StatusSeeOther)
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	for _, g := range cfg.CountGroups.Groups {
		if g.Name == name {
			cfg.Unlock()
			http.Redirect(w, r, "/traffic?err=duplicate", http.StatusSeeOther)
			return
		}
	}
	cfg.CountGroups.Groups = append(cfg.CountGroups.Groups, config.CountGroupConfig{
		Name:    name,
		Enabled: true,
	})
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		log.Printf("traffic: add error: %v", err)
		http.Error(w, "Failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.ReconfigureCountGroups()
	log.Printf("traffic: added group %q", name)
	http.Redirect(w, r, "/traffic?saved=added", http.StatusSeeOther)
}

// handleTrafficDelete handles POST /traffic/delete — removes a group by name.
func (h *Handlers) handleTrafficDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")

	cfg := h.engine.AppConfig()
	cfg.Lock()
	filtered := cfg.CountGroups.Groups[:0]
	for _, g := range cfg.CountGroups.Groups {
		if g.Name != name {
			filtered = append(filtered, g)
		}
	}
	cfg.CountGroups.Groups = filtered
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		log.Printf("traffic: delete error: %v", err)
		http.Error(w, "Failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.ReconfigureCountGroups()
	log.Printf("traffic: deleted group %q", name)
	http.Redirect(w, r, "/traffic?saved=deleted", http.StatusSeeOther)
}
