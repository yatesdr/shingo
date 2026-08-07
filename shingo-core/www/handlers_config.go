package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"shingocore/notify"
)

func (h *Handlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()
	data := map[string]any{
		"Page":   "config",
		"Config": cfg,
		"Saved":  r.URL.Query().Get("saved"),
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
		if v, err := strconv.Atoi(r.FormValue("pg_max_open_conns")); err == nil && v > 0 {
			cfg.Database.Postgres.MaxOpenConns = v
		}
		if v, err := strconv.Atoi(r.FormValue("pg_max_idle_conns")); err == nil && v > 0 {
			cfg.Database.Postgres.MaxIdleConns = v
		}
		if d, err := time.ParseDuration(r.FormValue("pg_conn_max_lifetime")); err == nil && d > 0 {
			cfg.Database.Postgres.ConnMaxLifetime = d
		}
	case "general", "fleet":
		if v := r.FormValue("fleet_base_url"); v != "" || r.Form.Has("fleet_base_url") {
			cfg.RDS.BaseURL = v
			if d, err := time.ParseDuration(r.FormValue("fleet_poll_interval")); err == nil {
				cfg.RDS.PollInterval = d
			}
			if d, err := time.ParseDuration(r.FormValue("fleet_timeout")); err == nil {
				cfg.RDS.Timeout = d
			}
			// Guard the zero: an empty or unparseable field must not silently
			// drop the grace period to 0, which would fail every faulted
			// order on the next poll instead of giving the floor time.
			if d, err := time.ParseDuration(r.FormValue("fleet_fault_grace")); err == nil && d > 0 {
				cfg.RDS.FaultGrace = d
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
	case "fire_alarm":
		cfg.FireAlarm.Enabled = r.FormValue("fa_enabled") == "on"
		cfg.FireAlarm.AutoResumeDefault = r.FormValue("fa_auto_resume") == "on"
	case "notifications":
		cfg.Notifications.Enabled = r.FormValue("notif_enabled") == "on"
		cfg.Notifications.SMTPHost = r.FormValue("notif_smtp_host")
		if p, err := strconv.Atoi(r.FormValue("notif_smtp_port")); err == nil && p > 0 {
			cfg.Notifications.SMTPPort = p
		}
		cfg.Notifications.SMTPTLS = r.FormValue("notif_smtp_tls") == "on"
		cfg.Notifications.SMTPUser = r.FormValue("notif_smtp_user")
		cfg.Notifications.SMTPPassword = r.FormValue("notif_smtp_password")
		cfg.Notifications.FromAddress = r.FormValue("notif_from_address")
		if v, err := strconv.Atoi(r.FormValue("notif_throttle_minutes")); err == nil && v > 0 {
			cfg.Notifications.ThrottleMinutes = v
		}
		var recipients []string
		for i := 0; ; i++ {
			addr := r.FormValue(fmt.Sprintf("notif_recipient_%d", i))
			if addr == "" {
				break
			}
			recipients = append(recipients, addr)
		}
		cfg.Notifications.Recipients = recipients
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
		h.orchestration.ReconfigureDatabase()
	case "general", "fleet":
		h.orchestration.ReconfigureFleet()
	case "services", "messaging":
		h.orchestration.ReconfigureMessaging()
	case "notifications":
		h.orchestration.ReconfigureNotifications()
	}

	log.Printf("config: %s section saved", section)
	http.Redirect(w, r, "/config?saved="+section, http.StatusSeeOther)
}

func (h *Handlers) handleConfigTestEmail(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()
	n := cfg.Notifications

	w.Header().Set("Content-Type", "application/json")

	if n.SMTPHost == "" || n.FromAddress == "" || len(n.Recipients) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "SMTP host, from address, and at least one recipient are required"})
		return
	}

	subject := "Shingo Test Email"
	body := "This is a test email from ShinGo Core.\n\n" +
		"SMTP connectivity verified successfully.\n" +
		"If you received this, notifications are configured correctly.\n" +
		"Time: " + time.Now().Format(time.RFC1123) + "\n"

	addr := fmt.Sprintf("%s:%d", n.SMTPHost, n.SMTPPort)
	var sendErr error
	if n.SMTPTLS {
		sendErr = notify.TLSSend(addr, n.SMTPUser, n.SMTPPassword, n.FromAddress, n.Recipients, subject, body)
	} else {
		sendErr = notify.PlainSend(addr, n.SMTPUser, n.SMTPPassword, n.FromAddress, n.Recipients, subject, body)
	}

	if sendErr != nil {
		log.Printf("config: test email failed: %v", sendErr)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": sendErr.Error()})
		return
	}

	log.Printf("config: test email sent to %d recipient(s)", len(n.Recipients))
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": fmt.Sprintf("Test email sent to %d recipient(s)", len(n.Recipients))})
}

func (h *Handlers) handleConfigTestAlert(w http.ResponseWriter, r *http.Request) {
	alertType := r.URL.Query().Get("type")
	if alertType != "fault" && alertType != "fail" && alertType != "cleared" && alertType != "chain" {
		http.Error(w, "type must be fault, fail, cleared, or chain", http.StatusBadRequest)
		return
	}

	cfg := h.engine.AppConfig()
	n := cfg.Notifications

	w.Header().Set("Content-Type", "application/json")

	if !n.Enabled {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "Notifications are not enabled"})
		return
	}
	if n.SMTPHost == "" || n.FromAddress == "" || len(n.Recipients) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "SMTP host, from address, and at least one recipient are required"})
		return
	}

	addr := fmt.Sprintf("%s:%d", n.SMTPHost, n.SMTPPort)
	sendMail := notify.PlainSend
	if n.SMTPTLS {
		sendMail = notify.TLSSend
	}
	testRobotID := "ROBOT-42"

	if alertType == "chain" {
		msgID := notify.GenerateMessageID("fault-chain-test")
		subject := notify.FaultSubject(testRobotID)
		body := notify.FaultAlert(99999, "test-edge-uuid", "STATION-01", "Simulated fault for chain testing", testRobotID)
		if err := sendMail(addr, n.SMTPUser, n.SMTPPassword, n.FromAddress, n.Recipients, subject, body, notify.WithMessageID(msgID)); err != nil {
			log.Printf("config: test chain fault failed: %v", err)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "Fault email failed: " + err.Error()})
			return
		}

		time.Sleep(2 * time.Second)

		clearSubject := notify.FaultClearedSubject(testRobotID)
		clearBody := notify.FaultClearedAlert(99999, "test-edge-uuid", "STATION-01", testRobotID, "3 m 0 s")
		if err := sendMail(addr, n.SMTPUser, n.SMTPPassword, n.FromAddress, n.Recipients, clearSubject, clearBody,
			notify.WithMessageID(notify.GenerateMessageID("cleared-chain-test")),
			notify.WithInReplyTo(msgID),
			notify.WithReferences(msgID),
		); err != nil {
			log.Printf("config: test chain cleared failed: %v", err)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "Fault sent, but cleared email failed: " + err.Error()})
			return
		}

		log.Printf("config: test chain sent to %d recipient(s)", len(n.Recipients))
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": fmt.Sprintf("Test fault chain sent to %d recipient(s) — check email threading", len(n.Recipients))})
		return
	}

	var subject, body string
	switch alertType {
	case "fault":
		subject = notify.FaultSubject(testRobotID)
		body = notify.FaultAlert(99999, "test-edge-uuid", "STATION-01", "Simulated fault for testing", testRobotID)
	case "fail":
		subject = notify.FailSubject(testRobotID)
		body = notify.FailAlert(99999, "test-edge-uuid", "STATION-01", "SIM_FAULT", "Simulated order failure for testing", testRobotID)
	case "cleared":
		subject = notify.FaultClearedSubject(testRobotID)
		body = notify.FaultClearedAlert(99999, "test-edge-uuid", "STATION-01", testRobotID, "")
	}

	var sendErr error
	if n.SMTPTLS {
		sendErr = notify.TLSSend(addr, n.SMTPUser, n.SMTPPassword, n.FromAddress, n.Recipients, subject, body)
	} else {
		sendErr = notify.PlainSend(addr, n.SMTPUser, n.SMTPPassword, n.FromAddress, n.Recipients, subject, body)
	}

	if sendErr != nil {
		log.Printf("config: test %s alert failed: %v", alertType, sendErr)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": sendErr.Error()})
		return
	}

	log.Printf("config: test %s alert sent to %d recipient(s)", alertType, len(n.Recipients))
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": fmt.Sprintf("Test %s alert sent to %d recipient(s)", alertType, len(n.Recipients))})
}
