package www

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

func (h *Handlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()
	data := map[string]any{
		"Page":          "config",
		"Config": cfg,
		"Saved":         r.URL.Query().Get("saved"),
	}
	h.render(w, r, "config.html", data)
}

func (h *Handlers) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	section := r.FormValue("section")
	cfg := h.engine.AppConfig()

	cfg.Lock()
	switch section {
	case "database":
		cfg.Database.Postgres.Host = r.FormValue("pg_host")
		if p, err := strconv.Atoi(r.FormValue("pg_port")); err == nil {
			cfg.Database.Postgres.Port = p
		}
		cfg.Database.Postgres.Database = r.FormValue("pg_database")
		cfg.Database.Postgres.User = r.FormValue("pg_user")
		if v := r.FormValue("pg_password"); v != "" {
			cfg.Database.Postgres.Password = v
		}
		cfg.Database.Postgres.SSLMode = r.FormValue("pg_sslmode")
	case "general", "fleet":
		if v := r.FormValue("fleet_base_url"); v != "" || r.Form.Has("fleet_base_url") {
			cfg.RDS.BaseURL = v
			if d, err := time.ParseDuration(r.FormValue("fleet_poll_interval")); err == nil {
				cfg.RDS.PollInterval = d
			}
			if d, err := time.ParseDuration(r.FormValue("fleet_timeout")); err == nil {
				cfg.RDS.Timeout = d
			}
		}
	case "services", "messaging":
		var brokers []string
		for i := 0; ; i++ {
			host := r.FormValue(fmt.Sprintf("kafka_host_%d", i))
			if host == "" {
				break
			}
			port := r.FormValue(fmt.Sprintf("kafka_port_%d", i))
			if port == "" {
				port = "9093"
			}
			brokers = append(brokers, host+":"+port)
		}
		cfg.Messaging.Kafka.Brokers = brokers
		cfg.Messaging.Kafka.GroupID = r.FormValue("group_id")
		cfg.Messaging.OrdersTopic = r.FormValue("orders_topic")
		cfg.Messaging.DispatchTopic = r.FormValue("dispatch_topic")
	default:
		cfg.Unlock()
		http.Error(w, "unknown section", http.StatusBadRequest)
		return
	}
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		log.Printf("config: save error: %v", err)
		http.Error(w, "Failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Hot-reload the affected subsystem
	switch section {
	case "database":
		h.engine.ReconfigureDatabase()
	case "general", "fleet":
		h.engine.ReconfigureFleet()
	case "services", "messaging":
		h.engine.ReconfigureMessaging()
	}

	log.Printf("config: %s section saved", section)
	http.Redirect(w, r, "/config?saved="+section, http.StatusSeeOther)
}
