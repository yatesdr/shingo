// handlers_config_save.go — the Edge Configuration page's one save door,
// PUT /api/config (U3, ruling R4), and the restart notice it shares with the
// page render.
//
// ONE SAVE PATH. The seven per-item doors this replaces each mutated the live
// config and then wrote the file, and six of them requested a backup: a page
// save touching three sections was three writes and three backups, and a bad
// value refused half-way left the live config already changed. Here, under
// the config's save mutex (also taken by adoptPlantTimezone in
// cmd/shingoedge): copy the live config → apply the posted sections to the
// copy → validate → (backups: test new storage) → write the file once → swap
// the copy into the live config field by field → apply what applies live →
// request ONE backup. Any error before the write: nothing is written and the
// live config is untouched. Only the posted sections are applied to the copy,
// and the copy is taken inside the mutex, so two people saving different
// sections both land.
//
// THE ANSWER keeps the legacy keys beside the new ones (lead ruling L5):
//
//	200 {"ok":true, "status":"ok", "applied":[…], "restart":[…], "failed":[…]}
//	400 {"ok":false, "error":"<first message>", "errors":{"<field>":"<msg>"}}
//
// `restart` is the whole restart notice (every restart-only field whose live
// value differs from what the process booted with), not just this save's.
// `failed` names a live apply that did not take after the file was written
// (the file stays). `simulated` names an apply the sim skips on purpose (E4).
//
// Live vs restart-only (U3): brokers (ReconnectKafka), the PLC link
// (ApplyWarLinkConfig), backups (the service reads the live config) and
// auto-confirm (read at use) apply live. Station UID, Plant timezone and the
// Core address apply after a restart. The Station UID is still swapped into
// the live config: some readers take it live (cfg.StationID()), the rest hold
// the boot value, so it runs split until the restart, and leaving it out of
// the swap would make the next save write the old one back.

package www

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"shingoedge/config"
)

// configSections are the wire sections, in the order they are applied and
// their errors reported.
var configSections = []string{"station", "core", "plc", "messaging", "backups"}

// configBoot is what the process booted with, for the restart-only fields.
// Taken once in NewRouter. Nil (handler tests build &Handlers{} directly)
// means no notice: no snapshot, no notice.
type configBoot struct {
	stationID string
	timezone  string
	coreAPI   string
}

func newConfigBoot(cfg *config.Config) *configBoot {
	if cfg == nil {
		return nil
	}
	c := cfg.Clone()
	return &configBoot{stationID: c.StationID(), timezone: c.Timezone, coreAPI: c.CoreAPI}
}

// restartPending lists the restart-only fields whose live value (which equals
// the file after a save) differs from the boot value. After adoptPlantTimezone
// fills a blank zone this lists Plant timezone though nobody saved: that is
// correct, the display stays on the boot zone until a restart.
func (h *Handlers) restartPending() []string {
	out := []string{}
	if h.boot == nil || h.engine == nil {
		return out
	}
	cfg := h.engine.AppConfig()
	if cfg == nil {
		return out
	}
	c := cfg.Clone()
	if c.StationID() != h.boot.stationID {
		out = append(out, "Station UID")
	}
	if c.Timezone != h.boot.timezone {
		out = append(out, "Plant timezone")
	}
	if c.CoreAPI != h.boot.coreAPI {
		out = append(out, "Core address")
	}
	return out
}

// ── Wire sections ───────────────────────────────────────────────────────

// stationSection: the Station UID, the plant timezone and auto-confirm.
//
// station_id is the legacy Messaging.StationID, accepted for the migration
// window only (the page has no field for it): a post with either one is
// saved, a post with both blank is refused (E1). An absent timezone keeps the
// saved zone; a blank one is stored blank, which means "take the zone Core
// offers" (adoptPlantTimezone fills it at the next heartbeat ack). An absent
// auto_confirm is off, as it was on its own door; the page always sends the
// section whole.
type stationSection struct {
	StationUID  string  `json:"station_uid"`
	StationID   string  `json:"station_id"`
	Timezone    *string `json:"timezone"`
	AutoConfirm bool    `json:"auto_confirm"`
}

// coreSection: the Core address. Restart-only (E2, R20). A blank is stored
// blank, as its own door did (lead ruling L8).
type coreSection struct {
	CoreAPI string `json:"core_api"`
}

// plcSection: the WarLink link. As its own door: a blank host, a zero port, a
// blank poll rate or a blank mode keeps the saved value; an absent enabled is
// off (L8; the page always sends the section whole).
type plcSection struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	PollRate string `json:"poll_rate"`
	Enabled  bool   `json:"enabled"`
	Mode     string `json:"mode"`
}

// messagingSection: the Kafka brokers, stored as typed (L8).
type messagingSection struct {
	Brokers []string `json:"brokers"`
}

// backupsSection: automatic backups and their storage. The secret is never
// rendered (E8, R3): a blank secret_key keeps the saved one, and
// remove_secret clears it.
type backupsSection struct {
	Enabled               bool   `json:"enabled"`
	ScheduleInterval      string `json:"schedule_interval"`
	KeepHourly            int    `json:"keep_hourly"`
	KeepDaily             int    `json:"keep_daily"`
	KeepWeekly            int    `json:"keep_weekly"`
	KeepMonthly           int    `json:"keep_monthly"`
	Endpoint              string `json:"endpoint"`
	Bucket                string `json:"bucket"`
	Region                string `json:"region"`
	AccessKey             string `json:"access_key"`
	SecretKey             string `json:"secret_key"`
	RemoveSecret          bool   `json:"remove_secret"`
	UsePathStyle          bool   `json:"use_path_style"`
	InsecureSkipTLSVerify bool   `json:"insecure_skip_tls_verify"`
}

// configErrors keeps field errors in the order they were found, so the
// legacy top-level `error` is the first one.
type configErrors struct {
	byField map[string]string
	first   string
}

func (e *configErrors) add(field, msg string) {
	if e.byField == nil {
		e.byField = map[string]string{}
	}
	if _, dup := e.byField[field]; dup {
		return
	}
	e.byField[field] = msg
	if e.first == "" {
		e.first = msg
	}
}

func (e *configErrors) any() bool { return len(e.byField) > 0 }

func writeConfigRefusal(w http.ResponseWriter, status int, errs *configErrors) {
	byField := errs.byField
	if byField == nil {
		byField = map[string]string{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": errs.first, "errors": byField})
}

func decodeSection(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	return dec.Decode(into)
}

// ── The door ────────────────────────────────────────────────────────────

// apiSaveConfig is PUT /api/config. The steps, in order, each refusing with
// nothing written: read the body → apply the posted sections to a copy taken
// under the save mutex → test changed backup storage → write the file → swap
// the copy in → apply what applies live → request one backup → answer.
func (h *Handlers) apiSaveConfig(w http.ResponseWriter, r *http.Request) {
	raw, errs := readConfigBody(r)
	if errs.any() {
		writeConfigRefusal(w, http.StatusBadRequest, errs)
		return
	}

	cfg := h.engine.AppConfig()
	cfg.LockSave()
	defer cfg.UnlockSave()

	draft := cfg.Clone()
	posted, storageTest, errs := applyConfigSections(draft, raw)
	if errs.any() {
		writeConfigRefusal(w, http.StatusBadRequest, errs)
		return
	}
	if status := h.testBackupStorage(r.Context(), storageTest, errs); status != 0 {
		writeConfigRefusal(w, status, errs)
		return
	}

	if err := draft.Save(h.engine.ConfigPath()); err != nil {
		errs.add("body", "Failed to save: "+err.Error())
		writeConfigRefusal(w, http.StatusInternalServerError, errs)
		return
	}
	cfg.Adopt(draft)

	applied, failed, simulated := h.applyConfigLive(cfg, posted)
	h.requestBackup("config")
	resp := map[string]any{
		"ok":      true,
		"status":  "ok",
		"applied": applied,
		"restart": h.restartPending(),
		"failed":  failed,
	}
	if len(simulated) > 0 {
		resp["simulated"] = simulated
	}
	writeJSON(w, resp)
}

// readConfigBody decodes the body into its sections and refuses an unreadable
// body, an unknown section or an empty one. The errors are empty when the body
// is usable.
func readConfigBody(r *http.Request) (map[string]json.RawMessage, *configErrors) {
	errs := &configErrors{}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		errs.add("body", err.Error())
		return nil, errs
	}
	known := map[string]bool{}
	for _, s := range configSections {
		known[s] = true
	}
	for k := range raw {
		if !known[k] {
			errs.add(k, fmt.Sprintf("unknown section %q", k))
		}
	}
	if len(raw) == 0 {
		errs.add("body", "nothing to save")
	}
	return raw, errs
}

// applyConfigSections applies each posted section to the draft, in
// configSections order so the errors are reported in that order. It returns
// which sections were posted and the backup storage to test (nil for none).
func applyConfigSections(draft *config.Config, raw map[string]json.RawMessage) (map[string]bool, *config.BackupS3Config, *configErrors) {
	errs := &configErrors{}
	posted := map[string]bool{}
	var storageTest *config.BackupS3Config
	for _, name := range configSections {
		body, ok := raw[name]
		if !ok {
			continue
		}
		posted[name] = true
		if st := applyConfigSection(draft, name, body, errs); st != nil {
			storageTest = st
		}
	}
	return posted, storageTest, errs
}

// applyConfigSection decodes one section and applies it to the draft. A body
// that does not decode is an error on the section's own name. It returns the
// backup storage to test, for the backups section only.
func applyConfigSection(draft *config.Config, name string, body json.RawMessage, errs *configErrors) *config.BackupS3Config {
	switch name {
	case "station":
		var s stationSection
		if err := decodeSection(body, &s); err != nil {
			errs.add("station", err.Error())
			return nil
		}
		applyStation(draft, s, errs)
	case "core":
		var s coreSection
		if err := decodeSection(body, &s); err != nil {
			errs.add("core", err.Error())
			return nil
		}
		draft.CoreAPI = s.CoreAPI
	case "plc":
		var s plcSection
		if err := decodeSection(body, &s); err != nil {
			errs.add("plc", err.Error())
			return nil
		}
		applyPLC(draft, s, errs)
	case "messaging":
		var s messagingSection
		if err := decodeSection(body, &s); err != nil {
			errs.add("messaging", err.Error())
			return nil
		}
		if s.Brokers == nil {
			s.Brokers = []string{}
		}
		draft.Messaging.Kafka.Brokers = s.Brokers
	case "backups":
		var s backupsSection
		if err := decodeSection(body, &s); err != nil {
			errs.add("backups", err.Error())
			return nil
		}
		return applyBackups(draft, s, errs)
	}
	return nil
}

// testBackupStorage is E8: enabling with changed storage settings tests the
// storage first. It returns 0 when there is nothing to test or the test
// passed, else the refusal's status with the error added to errs. The
// validation before it has already answered a bad body (L6), so a 501 here
// means a valid save that needs a service this process does not have.
func (h *Handlers) testBackupStorage(ctx context.Context, storageTest *config.BackupS3Config, errs *configErrors) int {
	if storageTest == nil {
		return 0
	}
	if h.backup == nil {
		errs.add("backups", "backup service unavailable")
		return http.StatusNotImplemented
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := h.backup.TestConfig(ctx, *storageTest)
	cancel()
	if err != nil {
		errs.add("endpoint", "Storage test failed: "+err.Error())
		return http.StatusBadRequest
	}
	return 0
}

// applyConfigLive applies what applies live for the posted sections, after
// the file is written and the copy swapped in. It names each section as
// applied, failed (a live apply that did not take; the file stays) or
// simulated (an apply the sim skips on purpose, E4).
func (h *Handlers) applyConfigLive(cfg *config.Config, posted map[string]bool) (applied, failed, simulated []string) {
	applied, failed, simulated = []string{}, []string{}, []string{}
	if posted["station"] {
		applied = append(applied, "Station")
	}
	if posted["plc"] {
		// In sim ApplyWarLinkConfig is a deliberate no-op (engine.go): the
		// injected sim client stays. Say so instead of claiming it applied.
		cfg.RLock()
		sim := cfg.Sim.Enabled
		cfg.RUnlock()
		h.orchestration.ApplyWarLinkConfig()
		if sim {
			simulated = append(simulated, "PLC link")
		} else {
			applied = append(applied, "PLC link")
		}
	}
	if posted["messaging"] {
		if err := h.orchestration.ReconnectKafka(); err != nil {
			log.Printf("kafka reconnect after config save: %v", err)
			failed = append(failed, "Messaging")
		} else {
			applied = append(applied, "Messaging")
		}
	}
	if posted["backups"] {
		applied = append(applied, "Backups")
	}
	return applied, failed, simulated
}

// applyStation applies the Station section to the draft (E1, the timezone).
func applyStation(draft *config.Config, s stationSection, errs *configErrors) {
	uid := strings.TrimSpace(s.StationUID)
	legacy := strings.TrimSpace(s.StationID)
	if uid == "" && legacy == "" {
		errs.add("station_uid", "Station UID is required.")
	}
	if uid != "" {
		draft.StationUID = s.StationUID
	}
	if legacy != "" {
		draft.Messaging.StationID = s.StationID
	}
	if s.Timezone != nil {
		tz := *s.Timezone
		if tz == "" {
			draft.Timezone = ""
		} else if loc, err := time.LoadLocation(tz); err != nil {
			errs.add("timezone", fmt.Sprintf("not an IANA timezone: %q", *s.Timezone))
		} else {
			draft.Timezone = loc.String()
		}
	}
	draft.Web.AutoConfirm = s.AutoConfirm
}

// applyPLC applies the PLC link section, validating before anything is kept.
func applyPLC(draft *config.Config, s plcSection, errs *configErrors) {
	if s.Mode != "" && s.Mode != "poll" && s.Mode != "sse" {
		errs.add("mode", `mode must be "poll" or "sse"`)
	}
	var poll time.Duration
	if s.PollRate != "" {
		d, err := time.ParseDuration(s.PollRate)
		if err != nil {
			errs.add("poll_rate", "invalid poll_rate: "+err.Error())
		}
		poll = d
	}
	if s.Host != "" {
		draft.WarLink.Host = s.Host
	}
	if s.Port > 0 {
		draft.WarLink.Port = s.Port
	}
	if s.PollRate != "" && !errsHas(errs, "poll_rate") {
		draft.WarLink.PollRate = poll
	}
	draft.WarLink.Enabled = s.Enabled
	if s.Mode != "" {
		draft.WarLink.Mode = s.Mode
	}
}

// applyBackups applies the Backups section (E8). It returns the storage to
// test when enabling with changed storage settings, else nil.
//
// A blank posted secret is replaced by the saved one BEFORE the enable check
// and before any test; remove_secret clears it. "Changed storage" compares the
// whole S3 target, the secret included (lead ruling L7).
func applyBackups(draft *config.Config, s backupsSection, errs *configErrors) *config.BackupS3Config {
	saved := draft.Backup.S3
	interval := config.DefaultBackupScheduleInterval
	if strings.TrimSpace(s.ScheduleInterval) != "" {
		d, err := time.ParseDuration(s.ScheduleInterval)
		if err != nil {
			errs.add("schedule_interval", "invalid schedule_interval: "+err.Error())
		}
		interval = d
	}
	next := config.BackupS3Config{
		Endpoint:              strings.TrimSpace(s.Endpoint),
		Bucket:                strings.TrimSpace(s.Bucket),
		Region:                strings.TrimSpace(s.Region),
		AccessKey:             strings.TrimSpace(s.AccessKey),
		SecretKey:             strings.TrimSpace(s.SecretKey),
		UsePathStyle:          s.UsePathStyle,
		InsecureSkipTLSVerify: s.InsecureSkipTLSVerify,
	}
	switch {
	case s.RemoveSecret:
		next.SecretKey = ""
	case next.SecretKey == "":
		next.SecretKey = saved.SecretKey
	}
	if s.Enabled {
		switch {
		case next.Endpoint == "":
			errs.add("endpoint", "endpoint, bucket, access key, and secret key are required to enable automatic backups")
		case next.Bucket == "":
			errs.add("bucket", "endpoint, bucket, access key, and secret key are required to enable automatic backups")
		case next.AccessKey == "":
			errs.add("access_key", "endpoint, bucket, access key, and secret key are required to enable automatic backups")
		case next.SecretKey == "":
			errs.add("secret_key", "endpoint, bucket, access key, and secret key are required to enable automatic backups")
		}
		if interval <= 0 && !errsHas(errs, "schedule_interval") {
			errs.add("schedule_interval", "schedule_interval must be greater than zero when automatic backups are enabled")
		}
	}
	draft.Backup.Enabled = s.Enabled
	draft.Backup.ScheduleInterval = interval
	draft.Backup.KeepHourly = s.KeepHourly
	draft.Backup.KeepDaily = s.KeepDaily
	draft.Backup.KeepWeekly = s.KeepWeekly
	draft.Backup.KeepMonthly = s.KeepMonthly
	draft.Backup.S3 = next
	if s.Enabled && next != saved {
		return &next
	}
	return nil
}

func errsHas(errs *configErrors, field string) bool {
	_, ok := errs.byField[field]
	return ok
}
