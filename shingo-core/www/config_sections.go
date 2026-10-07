package www

import (
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"shingocore/config"
)

// The config page's wire: one JSON object per section, sent by the page for
// the sections that are dirty (PUT /api/config, {section: {...}}). Each
// section decodes into its own struct and is applied to a DRAFT copy of the
// live config; a value that does not parse is a field error, keyed by the
// page's data-field name, and nothing is saved (C2).
//
// A field left out of a posted section is its zero value: a blank string is
// stored blank, as the form door did (ruling L1). The page always sends the
// whole section, so this only matters to a hand-built post.

// configSectionOrder is the canonical order: the page's, top to bottom. A
// save applies, reconfigures and reports its sections in this order.
var configSectionOrder = []string{"plant", "fleet", "messaging", "notifications", "fire_alarm", "database"}

func knownConfigSection(name string) bool {
	for _, s := range configSectionOrder {
		if s == name {
			return true
		}
	}
	return false
}

// fieldErrs maps a page field (its data-field) to what is wrong with it.
type fieldErrs map[string]string

// first returns one message, from the first field in sorted order, for the
// answer's top-level `error` (L2/L5: a failure always carries one).
func (e fieldErrs) first() string {
	best := ""
	for k := range e {
		if best == "" || k < best {
			best = k
		}
	}
	return e[best]
}

// flexString takes a JSON string or a bare number as text, so a port or a
// count can be typed into a box (a string) or sent by hand (a number) and is
// parsed, and refused, in one place.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	*f = flexString(string(b))
	return nil
}

func (f flexString) text() string { return strings.TrimSpace(string(f)) }

// positiveInt parses a count or a port. Anything but a whole number above zero
// is a field error.
func positiveInt(v flexString, key, what string, errs fieldErrs) int {
	n, err := strconv.Atoi(v.text())
	if err != nil || n <= 0 {
		errs[key] = what + " must be a whole number above zero."
		return 0
	}
	return n
}

func portNumber(v flexString, key string, errs fieldErrs) int {
	n, err := strconv.Atoi(v.text())
	if err != nil || n < 1 || n > 65535 {
		errs[key] = "A port is a number from 1 to 65535."
		return 0
	}
	return n
}

// duration parses a Go duration ("45m0s", "10s"); the page converts what is
// typed ("45 min") before it sends. min is the smallest value accepted.
func duration(v flexString, key string, min time.Duration, errs fieldErrs) time.Duration {
	d, err := time.ParseDuration(v.text())
	if err != nil {
		errs[key] = "Not a duration. Type it with its unit, like 45 min or 10 s."
		return 0
	}
	if d < min {
		if min > 0 {
			errs[key] = "Must be longer than zero."
		} else {
			errs[key] = "Cannot be negative."
		}
		return 0
	}
	return d
}

// --- plant ------------------------------------------------------------------

type plantWire struct {
	Timezone string `json:"timezone"`
}

// apply stores the zone as typed (blank stays blank: unset, Core shows UTC and
// tells its Edges nothing). time.LoadLocation checks it before the save (C9).
func (w plantWire) apply(c *config.Config, errs fieldErrs) {
	tz := strings.TrimSpace(w.Timezone)
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			errs["timezone"] = "Not a time zone this server knows. Use a name like America/Chicago."
			return
		}
	}
	c.Timezone = tz
}

// --- fleet ------------------------------------------------------------------

type fleetWire struct {
	BaseURL      string     `json:"base_url"`
	PollInterval flexString `json:"poll_interval"`
	Timeout      flexString `json:"timeout"`
	FaultGrace   flexString `json:"fault_grace"`
}

// apply: a zero poll interval or timeout is stored as 0s, as the form door did
// (L1); the grace period must be above zero, as it always had to be.
func (w fleetWire) apply(c *config.Config, errs fieldErrs) {
	poll := duration(w.PollInterval, "fleet_poll_interval", 0, errs)
	timeout := duration(w.Timeout, "fleet_timeout", 0, errs)
	grace := duration(w.FaultGrace, "fleet_fault_grace", time.Nanosecond, errs)
	c.RDS.BaseURL = w.BaseURL
	c.RDS.PollInterval = poll
	c.RDS.Timeout = timeout
	c.RDS.FaultGrace = grace
}

// --- messaging --------------------------------------------------------------

type messagingWire struct {
	Brokers       []string `json:"brokers"`
	GroupID       string   `json:"group_id"`
	OrdersTopic   string   `json:"orders_topic"`
	DispatchTopic string   `json:"dispatch_topic"`
}

// apply: brokers are one host:port string each, in the order sent (C1: a list
// on the wire, so removing the first row keeps the rest). A broker with no
// port, or a port that is not a number, is a field error on its row (C2).
func (w messagingWire) apply(c *config.Config, errs fieldErrs) {
	var brokers []string
	for i, b := range w.Brokers {
		b = strings.TrimSpace(b)
		host, port, err := net.SplitHostPort(b)
		key := fmt.Sprintf("kafka_broker_%d", i)
		if err != nil || strings.TrimSpace(host) == "" {
			errs[key] = "Type the broker as host:port, like kafka:9092."
			continue
		}
		if portNumber(flexString(port), key, errs) == 0 {
			continue
		}
		brokers = append(brokers, b)
	}
	c.Messaging.Kafka.Brokers = brokers
	c.Messaging.Kafka.GroupID = w.GroupID
	c.Messaging.OrdersTopic = w.OrdersTopic
	c.Messaging.DispatchTopic = w.DispatchTopic
}

// --- notifications ----------------------------------------------------------

type notificationsWire struct {
	Enabled           bool       `json:"enabled"`
	SMTPHost          string     `json:"smtp_host"`
	SMTPPort          flexString `json:"smtp_port"`
	SMTPTLS           bool       `json:"smtp_tls"`
	SMTPUser          string     `json:"smtp_user"`
	SMTPPassword      string     `json:"smtp_password"`
	ClearSMTPPassword bool       `json:"clear_smtp_password"`
	FromAddress       string     `json:"from_address"`
	Recipients        []string   `json:"recipients"`
	ThrottleMinutes   flexString `json:"throttle_minutes"`
}

// apply: a blank password keeps the saved one and clear_smtp_password removes
// it (C4; the form door erased it on blank). Blank recipient rows are dropped.
func (w notificationsWire) apply(n *config.NotificationsConfig, errs fieldErrs) {
	port := portNumber(w.SMTPPort, "notif_smtp_port", errs)
	throttle := positiveInt(w.ThrottleMinutes, "notif_throttle_minutes", "The wait between alerts (in minutes)", errs)
	n.Enabled = w.Enabled
	n.SMTPHost = w.SMTPHost
	n.SMTPPort = port
	n.SMTPTLS = w.SMTPTLS
	n.SMTPUser = w.SMTPUser
	switch {
	case w.ClearSMTPPassword:
		n.SMTPPassword = ""
	case w.SMTPPassword != "":
		n.SMTPPassword = w.SMTPPassword
	}
	n.FromAddress = w.FromAddress
	var recipients []string
	for _, r := range w.Recipients {
		if r = strings.TrimSpace(r); r != "" {
			recipients = append(recipients, r)
		}
	}
	n.Recipients = recipients
	n.ThrottleMinutes = throttle
}

// --- fire alarm -------------------------------------------------------------

type fireAlarmWire struct {
	Enabled           bool `json:"enabled"`
	AutoResumeDefault bool `json:"auto_resume_default"`
}

func (w fireAlarmWire) apply(c *config.Config, _ fieldErrs) {
	c.FireAlarm.Enabled = w.Enabled
	c.FireAlarm.AutoResumeDefault = w.AutoResumeDefault
}

// --- database ---------------------------------------------------------------

type databaseWire struct {
	Host            string     `json:"host"`
	Port            flexString `json:"port"`
	Database        string     `json:"database"`
	User            string     `json:"user"`
	Password        string     `json:"password"`
	ClearPassword   bool       `json:"clear_password"`
	SSLMode         string     `json:"sslmode"`
	MaxOpenConns    flexString `json:"max_open_conns"`
	MaxIdleConns    flexString `json:"max_idle_conns"`
	ConnMaxLifetime flexString `json:"conn_max_lifetime"`
}

var sslModes = []string{"disable", "require", "verify-ca", "verify-full"}

// apply: SSL mode is one of the four libpq modes (C2); a blank password keeps
// the saved one (C4), clear_password removes it.
func (w databaseWire) apply(pg *config.PostgresConfig, errs fieldErrs) {
	port := portNumber(w.Port, "pg_port", errs)
	open := positiveInt(w.MaxOpenConns, "pg_max_open_conns", "Open connections", errs)
	idle := positiveInt(w.MaxIdleConns, "pg_max_idle_conns", "Idle connections", errs)
	life := duration(w.ConnMaxLifetime, "pg_conn_max_lifetime", time.Nanosecond, errs)
	ok := false
	for _, m := range sslModes {
		ok = ok || w.SSLMode == m
	}
	if !ok {
		errs["pg_sslmode"] = "SSL mode is one of disable, require, verify-ca or verify-full."
	}
	pg.Host = w.Host
	pg.Port = port
	pg.Database = w.Database
	pg.User = w.User
	switch {
	case w.ClearPassword:
		pg.Password = ""
	case w.Password != "":
		pg.Password = w.Password
	}
	pg.SSLMode = w.SSLMode
	pg.MaxOpenConns = open
	pg.MaxIdleConns = idle
	pg.ConnMaxLifetime = life
}

// --- dispatch ---------------------------------------------------------------

// applyConfigSection decodes one posted section and applies it to the draft.
// A body that is not the section's shape is an error on the section itself.
func applyConfigSection(draft *config.Config, name string, raw json.RawMessage, errs fieldErrs) {
	var err error
	switch name {
	case "plant":
		var w plantWire
		if err = json.Unmarshal(raw, &w); err == nil {
			w.apply(draft, errs)
		}
	case "fleet":
		var w fleetWire
		if err = json.Unmarshal(raw, &w); err == nil {
			w.apply(draft, errs)
		}
	case "messaging":
		var w messagingWire
		if err = json.Unmarshal(raw, &w); err == nil {
			w.apply(draft, errs)
		}
	case "notifications":
		var w notificationsWire
		if err = json.Unmarshal(raw, &w); err == nil {
			w.apply(&draft.Notifications, errs)
		}
	case "fire_alarm":
		var w fireAlarmWire
		if err = json.Unmarshal(raw, &w); err == nil {
			w.apply(draft, errs)
		}
	case "database":
		var w databaseWire
		if err = json.Unmarshal(raw, &w); err == nil {
			w.apply(&draft.Database.Postgres, errs)
		}
	}
	if err != nil {
		errs[name] = "The " + name + " section is not in the shape this page sends: " + err.Error()
	}
}

// configSectionValue is the part of the config a section owns, for telling
// whether a save changed it (only a changed section is reconfigured).
func configSectionValue(c *config.Config, name string) any {
	switch name {
	case "plant":
		return c.Timezone
	case "fleet":
		return c.RDS
	case "messaging":
		return c.Messaging
	case "notifications":
		return c.Notifications
	case "fire_alarm":
		return c.FireAlarm
	case "database":
		return c.Database
	}
	return nil
}

func configSectionChanged(before, after *config.Config, name string) bool {
	return !reflect.DeepEqual(configSectionValue(before, name), configSectionValue(after, name))
}

// --- restart-only fields ------------------------------------------------------

// restartField is a setting Core reads once at boot (all or in part), so a
// saved change waits for a restart. The page's notice names each one whose
// live value differs from what the process started with.
type restartField struct {
	label string
	value func(*config.Config) string
}

// coreRestartFields: the plant timezone (www applies it to plantLocation at
// router construction; Edges are offered the boot value, main.go
// SetPlantTimezone); the robot status poll (the poller is not rewired, C7);
// the orders topic (subscribed once at boot); the dispatch topic (read at boot
// by the handler and dispatcher, live by the outbox, so split until the
// restart); the fault grace (live for the fleet, but Edges are offered the boot
// value, main.go SetFaultWindow); every database setting (the pool is opened
// once at boot and never swapped, R27: the lane lock and the ETA cache hold
// the *sql.DB they were built with).
var coreRestartFields = []restartField{
	{"Plant timezone", func(c *config.Config) string { return c.Timezone }},
	{"Ask for robot status every", func(c *config.Config) string { return c.RDS.PollInterval.String() }},
	{"Fail a faulted order after (the value Edges are offered)", func(c *config.Config) string { return c.RDS.FaultGrace.String() }},
	{"Orders topic", func(c *config.Config) string { return c.Messaging.OrdersTopic }},
	{"Dispatch topic", func(c *config.Config) string { return c.Messaging.DispatchTopic }},
	{"Database server", func(c *config.Config) string {
		return fmt.Sprintf("%s:%d", c.Database.Postgres.Host, c.Database.Postgres.Port)
	}},
	{"Database name", func(c *config.Config) string { return c.Database.Postgres.Database }},
	{"Database sign-in", func(c *config.Config) string {
		return fmt.Sprintf("%q %q", c.Database.Postgres.User, c.Database.Postgres.Password)
	}},
	{"Database encryption (SSL mode)", func(c *config.Config) string { return c.Database.Postgres.SSLMode }},
	{"Database connection pool", func(c *config.Config) string {
		pg := c.Database.Postgres
		return fmt.Sprintf("%d/%d/%s", pg.MaxOpenConns, pg.MaxIdleConns, pg.ConnMaxLifetime)
	}},
}

// bootSnapshot is the restart-only fields as read at process start, taken in
// NewRouter. A Handlers built without one (most tests) shows no notice.
type bootSnapshot []string

func takeBootSnapshot(c *config.Config) bootSnapshot {
	out := make(bootSnapshot, len(coreRestartFields))
	for i, f := range coreRestartFields {
		out[i] = f.value(c)
	}
	return out
}

// pending lists the restart-only fields whose live value (which equals the
// file after a save) differs from the boot value. Never nil, so the answer's
// `restart` is always a list.
func (b bootSnapshot) pending(c *config.Config) []string {
	out := []string{}
	if b == nil {
		return out
	}
	for i, f := range coreRestartFields {
		if f.value(c) != b[i] {
			out = append(out, f.label)
		}
	}
	return out
}

// durationText writes a duration the way the page shows one: one box with its
// unit in it ("45 min", "10 s", "1 h 30 min"), never "45m0s". The page's
// durationToText (shared/utils.js) writes the same.
func durationText(d time.Duration) string {
	if d == 0 {
		return "0 s"
	}
	neg := ""
	if d < 0 {
		neg, d = "-", -d
	}
	ms := d.Milliseconds()
	h, m, s, rest := ms/3600000, ms%3600000/60000, ms%60000/1000, ms%1000
	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%d h", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%d min", m))
	}
	if s > 0 {
		parts = append(parts, fmt.Sprintf("%d s", s))
	}
	if rest > 0 {
		parts = append(parts, fmt.Sprintf("%d ms", rest))
	}
	if len(parts) == 0 {
		return d.String()
	}
	return neg + strings.Join(parts, " ")
}
