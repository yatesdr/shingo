package www

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"shingo/protocol/auth"
	"shingocore/config"
	"shingocore/notify"
)

// The Core Configuration page: one page, one Save (docs/ui-style-guide/
// 33-settings-pages.md). GET /config renders it; PUT /api/config is its one
// save door; POST /api/config/test-database, /config/test-email,
// /config/test-alert and /config/password are its actions, none of which
// touches the draft or the file.

func (h *Handlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig().Clone()
	h.render(w, r, "config.html", map[string]any{
		"Page":   "config",
		"Config": cfg,
		"V":      h.configView(cfg),
	})
}

// configView is what the page shows beyond the raw config: durations as
// typed, live words for the section titles, whether a secret is saved (never
// the secret), and the restart notice.
type configView struct {
	TZEnv           string // PLANT_TIMEZONE, when set: the row is read-only
	ShownZone       string // the zone Core's own screens are using
	MessagingWords  string
	MessagingOK     bool
	DatabaseWords   string
	DatabaseOK      bool
	PollText        string
	TimeoutText     string
	GraceText       string
	ThrottleText    string
	LifetimeText    string
	MaxOpen         int
	MaxIdle         int
	DBPasswordSaved bool
	SMTPPassSaved   bool
	SSLModes        []string
	RestartJSON     string
}

func (h *Handlers) configView(cfg *config.Config) configView {
	pg := cfg.Database.Postgres
	v := configView{
		TZEnv:           os.Getenv("PLANT_TIMEZONE"),
		ShownZone:       plantLocation.String(),
		PollText:        durationText(cfg.RDS.PollInterval),
		TimeoutText:     durationText(cfg.RDS.Timeout),
		GraceText:       durationText(cfg.RDS.FaultGrace),
		ThrottleText:    durationText(time.Duration(cfg.Notifications.ThrottleMinutes) * time.Minute),
		LifetimeText:    durationText(pg.ConnMaxLifetime),
		MaxOpen:         pg.MaxOpenConns,
		MaxIdle:         pg.MaxIdleConns,
		DBPasswordSaved: pg.Password != "",
		SMTPPassSaved:   cfg.Notifications.SMTPPassword != "",
		SSLModes:        sslModes,
	}
	// The pool's own defaults (store.OpenWithoutMigrate), shown when unset,
	// as the form page did.
	if v.MaxOpen <= 0 {
		v.MaxOpen = 25
	}
	if v.MaxIdle <= 0 {
		v.MaxIdle = 10
	}
	if pg.ConnMaxLifetime <= 0 {
		v.LifetimeText = "5 min"
	}
	if mc := h.engine.MsgClient(); mc != nil {
		v.MessagingOK = mc.IsConnected()
		v.MessagingWords = map[bool]string{true: "connected", false: "not connected"}[v.MessagingOK]
	}
	if hs := h.engine.HealthService(); hs != nil {
		if _, ok := hs.PoolStats(); ok {
			v.DatabaseOK = hs.PingDB() == nil
			v.DatabaseWords = map[bool]string{true: "connected", false: "not answering"}[v.DatabaseOK]
		}
	}
	restart, _ := json.Marshal(h.boot.pending(cfg))
	v.RestartJSON = string(restart)
	return v
}

func writeConfigJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// configRefusal is a save or test that did not go through: a top-level
// `error` always (L2, L5), and the field errors when there are any.
func configRefusal(w http.ResponseWriter, status int, msg string, errs fieldErrs) {
	if errs == nil {
		errs = fieldErrs{}
	}
	writeConfigJSON(w, status, map[string]any{"ok": false, "error": msg, "errors": errs})
}

// readConfigBody decodes a JSON object body. An empty body is an empty object.
func readConfigBody(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte("{}")
	}
	return json.Unmarshal(raw, v)
}

// apiConfigSave is the page's one save door: PUT /api/config with
// {section: {...}} for the dirty sections only.
//
// ONE SAVE PATH. Under h.configMu, held for the whole sequence: copy the live
// config, apply the posted sections to the copy, validate, (database: ping
// the draft, C3; it applies after a restart, R27), write the file, swap the copy into the live config field
// by field, then reconfigure what changed. Any error before the write and
// nothing is written and the live config is untouched. The copy is taken
// inside the mutex and only the posted sections are applied to it, so two
// people saving different sections both land.
//
// Answers: 400 {ok:false, error, errors{field: msg}} for a refused save; 500
// {ok:false, error: "Failed to save: …"} when the file cannot be written; 200
// {ok:true, applied, restart, failed}, where `failed` names a subsystem that
// did not take the saved config (the file stays).
func (h *Handlers) apiConfigSave(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if err := readConfigBody(r, &body); err != nil {
		configRefusal(w, http.StatusBadRequest, "The body is not a JSON object of sections: "+err.Error(), nil)
		return
	}
	posted, unknown := orderedConfigSections(body)
	if unknown != "" {
		configRefusal(w, http.StatusBadRequest, "unknown section: "+unknown, nil)
		return
	}

	h.configMu.Lock()
	defer h.configMu.Unlock()

	live := h.engine.AppConfig()
	draft := live.Clone()
	errs := fieldErrs{}
	for _, name := range posted {
		applyConfigSection(draft, name, body[name], errs)
	}
	if len(errs) > 0 {
		configRefusal(w, http.StatusBadRequest, errs.first(), errs)
		return
	}

	if err := h.pingConfigDatabase(live, draft, posted); err != nil {
		configRefusal(w, http.StatusBadRequest,
			"The database did not answer, so nothing was saved: "+err.Error(), nil)
		return
	}
	if err := draft.Save(h.engine.ConfigPath()); err != nil {
		log.Printf("config: save error: %v", err)
		configRefusal(w, http.StatusInternalServerError, "Failed to save: "+err.Error(), nil)
		return
	}

	changed := make(map[string]bool, len(posted))
	for _, name := range posted {
		changed[name] = configSectionChanged(live, draft, name)
	}
	live.ReplaceFrom(draft)
	failed := h.reconfigureConfigSections(posted, changed)

	log.Printf("config: saved %s", strings.Join(posted, ", "))
	writeConfigJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"applied": posted,
		"restart": h.boot.pending(live),
		"failed":  failed,
	})
}

// orderedConfigSections returns the posted section names in page order, or
// the first name it does not know. Nothing posted is itself unknown.
func orderedConfigSections(body map[string]json.RawMessage) (posted []string, unknown string) {
	for name := range body {
		if !knownConfigSection(name) && (unknown == "" || name < unknown) {
			unknown = name
		}
	}
	if unknown != "" {
		return nil, unknown
	}
	if len(body) == 0 {
		return nil, "none posted"
	}
	for _, name := range configSectionOrder {
		if _, ok := body[name]; ok {
			posted = append(posted, name)
		}
	}
	return posted, ""
}

// pingConfigDatabase: when the posted database section changes the
// connection, the draft is opened and pinged (bounded, no migrate) before
// anything is written, and a database that does not answer refuses the save
// (C3, R2). It is only a check: the new settings apply after a restart (R27),
// because the lane lock and the ETA cache hold the *sql.DB they were built
// with, so swapping the pool under a running Core left them on a closed one.
func (h *Handlers) pingConfigDatabase(live, draft *config.Config, posted []string) error {
	for _, name := range posted {
		if name == "database" && configSectionChanged(live, draft, name) {
			if h.pingDB != nil {
				return h.pingDB(draft.Database)
			}
			return h.engine.HealthService().TestDatabase(draft.Database)
		}
	}
	return nil
}

// reconfigureConfigSections applies the live config to each subsystem whose
// section changed. Plant and fire alarm have none (read at boot and at use).
func (h *Handlers) reconfigureConfigSections(posted []string, changed map[string]bool) []string {
	failed := []string{}
	for _, name := range posted {
		if !changed[name] {
			continue
		}
		var err error
		switch name {
		case "fleet":
			err = h.orchestration.ReconfigureFleet()
		case "messaging":
			err = h.orchestration.ReconfigureMessaging()
		case "notifications":
			err = h.orchestration.ReconfigureNotifications()
		}
		if err != nil {
			failed = append(failed, name+" ("+err.Error()+")")
		}
	}
	return failed
}

// apiConfigTestDatabase is the Database section's Test connection: it opens
// and pings the posted draft (blank password = the saved one) and never
// migrates or saves.
func (h *Handlers) apiConfigTestDatabase(w http.ResponseWriter, r *http.Request) {
	var wire databaseWire
	if err := readConfigBody(r, &wire); err != nil {
		configRefusal(w, http.StatusBadRequest, "The body is not the database section: "+err.Error(), nil)
		return
	}
	draft := h.engine.AppConfig().Clone().Database
	errs := fieldErrs{}
	wire.apply(&draft.Postgres, errs)
	if len(errs) > 0 {
		configRefusal(w, http.StatusBadRequest, errs.first(), errs)
		return
	}
	pg := draft.Postgres
	if err := h.engine.HealthService().TestDatabase(draft); err != nil {
		writeConfigJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "message": err.Error()})
		return
	}
	msg := fmt.Sprintf("Connected to %s:%d/%s.", pg.Host, pg.Port, pg.Database)
	writeConfigJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
}

// notificationsDraft decodes a posted notifications section over the saved
// one (so a blank password is the saved password) for the test doors (C5).
func (h *Handlers) notificationsDraft(r *http.Request) (config.NotificationsConfig, fieldErrs, error) {
	n := h.engine.AppConfig().Clone().Notifications
	var wire notificationsWire
	if err := readConfigBody(r, &wire); err != nil {
		return n, nil, err
	}
	errs := fieldErrs{}
	wire.apply(&n, errs)
	return n, errs, nil
}

const smtpFieldsRequired = "SMTP host, from address, and at least one recipient are required"

// testSendDraft reads the posted draft and answers for it when it cannot be
// sent: a malformed body, a field error, or the SMTP fields missing. ok is
// false when it has already answered.
func (h *Handlers) testSendDraft(w http.ResponseWriter, r *http.Request) (config.NotificationsConfig, bool) {
	n, errs, err := h.notificationsDraft(r)
	if err != nil {
		writeConfigJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error(), "error": err.Error()})
		return n, false
	}
	if len(errs) > 0 {
		writeConfigJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": errs.first(), "error": errs.first(), "errors": errs})
		return n, false
	}
	if n.SMTPHost == "" || n.FromAddress == "" || len(n.Recipients) == 0 {
		writeConfigJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": smtpFieldsRequired})
		return n, false
	}
	return n, true
}

// handleConfigTestEmail sends a plain test email with the posted draft (C5:
// the draft, not the saved config; Enabled is not required).
func (h *Handlers) handleConfigTestEmail(w http.ResponseWriter, r *http.Request) {
	n, ok := h.testSendDraft(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")

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

// handleConfigTestAlert sends one of the alert emails with the posted draft
// (C5: the draft; Enabled is no longer required).
func (h *Handlers) handleConfigTestAlert(w http.ResponseWriter, r *http.Request) {
	alertType := r.URL.Query().Get("type")
	if alertType != "fault" && alertType != "fail" && alertType != "cleared" && alertType != "chain" {
		http.Error(w, "type must be fault, fail, cleared, or chain", http.StatusBadRequest)
		return
	}
	n, ok := h.testSendDraft(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")

	addr := fmt.Sprintf("%s:%d", n.SMTPHost, n.SMTPPort)
	sendMail := notify.PlainSend
	if n.SMTPTLS {
		sendMail = notify.TLSSend
	}
	testRobotID := "ROBOT-42"

	if alertType == "chain" {
		sendTestChain(w, n, addr, sendMail, testRobotID)
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

	if sendErr := sendMail(addr, n.SMTPUser, n.SMTPPassword, n.FromAddress, n.Recipients, subject, body); sendErr != nil {
		log.Printf("config: test %s alert failed: %v", alertType, sendErr)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": sendErr.Error()})
		return
	}

	log.Printf("config: test %s alert sent to %d recipient(s)", alertType, len(n.Recipients))
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": fmt.Sprintf("Test %s alert sent to %d recipient(s)", alertType, len(n.Recipients))})
}

type mailSender func(addr, user, pass, from string, to []string, subject, body string, opts ...notify.SendOption) error

// sendTestChain sends a fault and, 2 s later, its cleared reply on the same
// thread, so the threading can be checked in a mail client.
func sendTestChain(w http.ResponseWriter, n config.NotificationsConfig, addr string, sendMail mailSender, robot string) {
	msgID := notify.GenerateMessageID("fault-chain-test")
	subject := notify.FaultSubject(robot)
	body := notify.FaultAlert(99999, "test-edge-uuid", "STATION-01", "Simulated fault for chain testing", robot)
	if err := sendMail(addr, n.SMTPUser, n.SMTPPassword, n.FromAddress, n.Recipients, subject, body, notify.WithMessageID(msgID)); err != nil {
		log.Printf("config: test chain fault failed: %v", err)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "Fault email failed: " + err.Error()})
		return
	}

	time.Sleep(2 * time.Second)

	clearSubject := notify.FaultClearedSubject(robot)
	clearBody := notify.FaultClearedAlert(99999, "test-edge-uuid", "STATION-01", robot, "3 m 0 s")
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
}

// handleConfigPassword rotates the logged-in admin's password.
//
// Core had no password-change path of any kind until this landed: the only
// rotation available was an UPDATE against admin_users in Postgres by hand.
// Edge has carried the store/service/handler chain since its setup page was
// built, and this mirrors it — same old-password check, same hash-then-update
// order, same JSON shape as the rest of core's /config POSTs.
//
// The current password is verified here rather than at the service layer
// because that is where edge verifies it and where the session lives. The
// config page's Account section calls it from its Change password modal (C8).
func (h *Handlers) handleConfigPassword(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	username := h.getUsername(r)
	if username == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "not logged in"})
		return
	}

	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if req.NewPassword == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "new password is required"})
		return
	}

	user, err := h.engine.AdminService().GetUser(username)
	if err != nil {
		log.Printf("config: password change: lookup %q: %v", username, err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "user not found"})
		return
	}

	if !auth.CheckPassword(user.PasswordHash, req.OldPassword) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "current password is incorrect"})
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "failed to hash password"})
		return
	}

	if err := h.engine.AdminService().UpdatePassword(username, hash); err != nil {
		log.Printf("config: password change: update %q: %v", username, err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "failed to update password"})
		return
	}

	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
