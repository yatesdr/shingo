package www

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (h *Handlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()
	data := map[string]any{
		"Page":          "config",
		"Authenticated": h.isAuthenticated(r),
		"Config":        cfg,
		"Saved":         r.URL.Query().Get("saved"),
	}
	h.render(w, "config.html", data)
}

func (h *Handlers) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	section := r.FormValue("section")
	cfg := h.engine.AppConfig()

	switch section {
	case "rds":
		cfg.RDS.BaseURL = r.FormValue("rds_base_url")
		if d, err := time.ParseDuration(r.FormValue("rds_poll_interval")); err == nil {
			cfg.RDS.PollInterval = d
		}
		if d, err := time.ParseDuration(r.FormValue("rds_timeout")); err == nil {
			cfg.RDS.Timeout = d
		}
	case "messaging":
		cfg.Messaging.Backend = r.FormValue("msg_backend")
		cfg.Messaging.MQTT.Broker = r.FormValue("mqtt_broker")
		if p, err := strconv.Atoi(r.FormValue("mqtt_port")); err == nil {
			cfg.Messaging.MQTT.Port = p
		}
		cfg.Messaging.MQTT.ClientID = r.FormValue("mqtt_client_id")
		brokers := r.FormValue("kafka_brokers")
		if brokers != "" {
			cfg.Messaging.Kafka.Brokers = splitTrim(brokers, ",")
		} else {
			cfg.Messaging.Kafka.Brokers = []string{}
		}
		cfg.Messaging.OrdersTopic = r.FormValue("orders_topic")
		cfg.Messaging.DispatchTopicPrefix = r.FormValue("dispatch_topic_prefix")
	case "redis":
		cfg.Redis.Address = r.FormValue("redis_address")
		cfg.Redis.Password = r.FormValue("redis_password")
		if d, err := strconv.Atoi(r.FormValue("redis_db")); err == nil {
			cfg.Redis.DB = d
		}
	case "general":
		cfg.FactoryID = r.FormValue("factory_id")
	default:
		http.Error(w, "unknown section", http.StatusBadRequest)
		return
	}

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		log.Printf("config: save error: %v", err)
		http.Error(w, "Failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Hot-reload the affected subsystem
	switch section {
	case "rds":
		h.engine.ReconfigureRDS()
	case "messaging":
		h.engine.ReconfigureMessaging()
	}

	log.Printf("config: %s section saved", section)
	http.Redirect(w, r, "/config?saved="+section, http.StatusSeeOther)
}

func splitTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
