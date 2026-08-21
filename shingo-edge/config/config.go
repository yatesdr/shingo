package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level application configuration.
type Config struct {
	mu sync.RWMutex `yaml:"-"`

	// StationUID is this edge's identity, minted by Core at enrollment. It is
	// opaque, it never changes, and it is the value that travels as
	// protocol.Address.Station.
	//
	// AN OPERATOR PUTS IT HERE BY HAND, ONCE, AND THAT IS THE DESIGN RATHER
	// THAN A MISSING FEATURE. Handing the uid over is exactly the step at
	// which a human distinguishes the two hardware events Core cannot tell
	// apart on its own: a NEW station (enroll, take the fresh uid) from
	// REPLACEMENT HARDWARE for an existing one (do not enroll — copy the
	// existing uid off Core onto the new Pi, and the station's whole history
	// stays attached to it). A Pi that mints its own id can express the first
	// and never the second.
	//
	// It rides the backup archive (backup/snapshot.go includes the config), so
	// a restore is the other way a replacement box gets it back.
	//
	// Empty means unenrolled, and since the enrollment deploy that is a startup
	// REFUSAL rather than a derivation — there is no default left to compose
	// one from. See main.mustHaveIdentity.
	StationUID string `yaml:"station_uid"`

	// Namespace / LineID are LABELS NOW, not identity. They no longer compose
	// a station id and they no longer have defaults; they survive because
	// existing plant configs carry them and silently dropping fields a plant
	// has written is its own kind of surprise.
	Namespace    string        `yaml:"namespace"`
	LineID       string        `yaml:"line_id"`
	DatabasePath string        `yaml:"database_path"`
	PollRate     time.Duration `yaml:"poll_rate"`

	Timezone string `yaml:"timezone"` // IANA timezone for shift/hourly bucketing (e.g. "America/Chicago")

	CoreAPI     string            `yaml:"core_api"` // Core HTTP base URL (e.g. "http://192.168.1.10:8080")
	WarLink     WarLinkConfig     `yaml:"warlink"`
	Web         WebConfig         `yaml:"web"`
	Messaging   MessagingConfig   `yaml:"messaging"`
	Counter     CounterConfig     `yaml:"counter"`
	Backup      BackupConfig      `yaml:"backup"`
	Sim         SimConfig         `yaml:"sim"`

	// LoadersMultiWindow — DEPRECATED. The setting moved onto the loader itself:
	// Core's bin_loaders.funnel_windows, synced down and read by
	// engine.multiWindowFor. A plant-wide key could only answer for every loader
	// at once, so a plant needing the funnel for one loader imposed it on all.
	//
	// What survives here is an OFF switch and nothing more. An explicit
	// `loaders_multi_window: false` still funnels every loader in the plant, so a
	// site that set it does not silently start spreading the day this ships. It
	// cannot force spreading ON against a loader configured to funnel — the
	// loader is the authority; this is a brake.
	//
	// Leave it unset. Nil, and `true`, both mean "let each loader decide", which
	// is what the old default did anyway. Deploy 9 deletes the key once no
	// deployed config sets it.
	LoadersMultiWindow *bool `yaml:"loaders_multi_window"`

	// UOPAccumulatingCTAAfter is how long a node may sit unbound with counts
	// accumulating before the operator chip starts telling the operator to do
	// something about it.
	//
	// Below this, an unbound node with a rising count is a NORMAL swap in
	// progress and the chip says only that. Above it, the wait is longer than a
	// swap should take and the chip adds the fix ("Record Count on the bin tab").
	//
	// Config, not a constant, because the longest legitimate gap is a property of
	// the plant, not of the software. One observed data point at Springfield is
	// 15 minutes; the default sits above it with room, so the call to action means
	// something when it appears.
	//
	// Zero or unset uses DefaultUOPAccumulatingCTAAfter. Negative means always
	// show the call to action.
	UOPAccumulatingCTAAfter time.Duration `yaml:"uop_accumulating_cta_after"`

	Demand DemandConfig `yaml:"demand"`
}

// DefaultUOPAccumulatingCTAAfter is the fallback for
// Config.UOPAccumulatingCTAAfter — see that field for why it is configurable.
const DefaultUOPAccumulatingCTAAfter = 30 * time.Minute

// UOPAccumulatingCTADelay resolves the configured value, applying the default
// for the unset case. Callers use this rather than the field so the "0 means
// default" rule lives in one place.
func (c *Config) UOPAccumulatingCTADelay() time.Duration {
	if c == nil || c.UOPAccumulatingCTAAfter == 0 {
		return DefaultUOPAccumulatingCTAAfter
	}
	return c.UOPAccumulatingCTAAfter
}

// DemandConfig tunes demand-episode detection.
type DemandConfig struct {
	// HysteresisPercent is the recovery margin, as a percentage of the claim's
	// reorder_point. A cell episode opens when remaining UOP falls to or below
	// reorder_point and closes only once it climbs back ABOVE
	// reorder_point + margin.
	//
	// WITHOUT A MARGIN a cell sitting exactly at its reorder point mints an
	// episode every time a tick nudges it across — thousands of 20-second noise
	// episodes, and the demand surface becomes unreadable at precisely the
	// plants where each episode matters most. Bites hardest at a low-mix plant,
	// which is why it goes in from the start rather than after someone sees it.
	//
	// THE NUMBER IS NOT DERIVED FROM ANYTHING. The design says "close above
	// threshold + margin" and deliberately names no value, so this is a
	// conservative default and a knob, not a constant someone reverse-engineers
	// later. 10% of the reorder point, floored at 1 UOP so a small reorder point
	// still gets a real margin.
	//
	// It is small on purpose: in normal operation a swap refills the node to
	// roughly bin capacity, far above any margin, so this only governs the
	// oscillation case. Raise it if a plant reports flapping episodes.
	//
	// TUNING IT ON THE SIM IS ONLY MEANINGFUL AFTER THE CLOCK INJECTION that
	// landed with this work: the threshold monitor's windows previously ran on
	// wall time while the activity they gate ran at 15x sim speed, so anything
	// calibrated against them was calibrated against the wrong ratio.
	//
	// A specific number for a specific plant is a question for a human, not a
	// default to quietly change here.
	HysteresisPercent *float64 `yaml:"hysteresis_percent"`
}

// DefaultHysteresisPercent is the recovery margin as a fraction of
// reorder_point when nothing is configured. See DemandConfig.
const DefaultHysteresisPercent = 10.0

// MinHysteresisUOP is the floor on the computed margin. A reorder point of 5
// would otherwise get a margin of 0 and no hysteresis at all — which is the
// case that needs it most, since a small reorder point is crossed by ordinary
// tick noise.
const MinHysteresisUOP = 1

// HysteresisMargin returns the UOP a claim must recover ABOVE its reorder point
// before its episode closes.
func (c *Config) HysteresisMargin(reorderPoint int) int {
	c.mu.RLock()
	pct := c.Demand.HysteresisPercent
	c.mu.RUnlock()

	p := DefaultHysteresisPercent
	if pct != nil {
		p = *pct
	}
	if p < 0 || reorderPoint <= 0 {
		return MinHysteresisUOP
	}
	margin := int(float64(reorderPoint) * p / 100.0)
	if margin < MinHysteresisUOP {
		return MinHysteresisUOP
	}
	return margin
}

// WarLinkConfig defines the WarLink connection.
type WarLinkConfig struct {
	Host     string        `yaml:"host"        json:"host"`
	Port     int           `yaml:"port"        json:"port"`
	PollRate time.Duration `yaml:"poll_rate"   json:"poll_rate"`
	Enabled  bool          `yaml:"enabled"     json:"enabled"`
	Mode     string        `yaml:"mode"        json:"mode"` // "sse" (default) or "poll"
}

// WebConfig defines the web server settings.
type WebConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// snapshot:"secret" keeps this out of the defaults rendering. It is
	// GENERATED per Defaults() call, so rendering it would both leak a
	// credential into git and make the snapshot differ from itself every run.
	SessionSecret string `yaml:"session_secret" snapshot:"secret"`
	AutoConfirm   bool   `yaml:"auto_confirm"`
}

// MessagingConfig defines the messaging backend.
type MessagingConfig struct {
	Kafka               KafkaConfig   `yaml:"kafka"`
	DispatchTopic       string        `yaml:"dispatch_topic"`
	OrdersTopic         string        `yaml:"orders_topic"`
	OutboxDrainInterval time.Duration `yaml:"outbox_drain_interval"`
	StationID           string        `yaml:"station_id"`
	SigningKey          string        `yaml:"signing_key"` // optional HMAC-SHA256 shared secret for envelope signing
}

// KafkaConfig defines Kafka broker settings.
type KafkaConfig struct {
	Brokers []string `yaml:"brokers"`

	// GroupID is the consumer group this edge joins. RUNTIME-ONLY — the
	// `yaml:"-"` is the fix, not an omission.
	//
	// IT USED TO PERSIST, AND THAT IS A MEASURED LIVE DEFECT AT BOTH PLANTS.
	// main.go writes the DERIVED value into this field at boot; the field
	// carried a yaml tag; and Config.Save() marshals the whole struct. So any
	// of the nine runtime Save() call sites — saving a PLC setting, a traffic
	// setting, anything — wrote `group_id: shingo-edge-plant-a.line-1` into
	// /etc/shingo/shingoedge.yaml. Measured on line 22 of the Edge yaml at
	// Springfield (mtime Jul 16) and Hopkinsville (mtime May 27).
	// install-edge.sh never wrote it; a Save did.
	//
	// KafkaGroupID() then short-circuited on the non-empty value FOREVER, so
	// the group stayed pinned to the OLD station id across any rename. The
	// consequence for this change is exact: renaming a station would have been
	// COSMETIC ON THE WIRE — the edge would keep consuming under the old group
	// and keep sharing it with any second edge, which is the "one edge is deaf"
	// defect the rename was supposed to end.
	//
	// A rollout step that deletes the key from both files fixes the two files
	// that exist. `yaml:"-"` fixes the mechanism: the value cannot be written
	// back, cannot be read in, and cannot override the derivation. The rollout
	// step stays anyway, because a stale key in a config file is a lie the next
	// reader has to disprove, and because a rollback to a pre-v66 binary would
	// re-arm it.
	GroupID string `yaml:"-"`
}

// CounterConfig defines counter anomaly thresholds.
type CounterConfig struct {
	JumpThreshold int64 `yaml:"jump_threshold"`
}

// BackupConfig defines edge backup behavior and storage.
type BackupConfig struct {
	Enabled          bool           `yaml:"enabled" json:"enabled"`
	ScheduleInterval time.Duration  `yaml:"schedule_interval" json:"schedule_interval"`
	KeepHourly       int            `yaml:"keep_hourly" json:"keep_hourly"`
	KeepDaily        int            `yaml:"keep_daily" json:"keep_daily"`
	KeepWeekly       int            `yaml:"keep_weekly" json:"keep_weekly"`
	KeepMonthly      int            `yaml:"keep_monthly" json:"keep_monthly"`
	S3               BackupS3Config `yaml:"s3" json:"s3"`
}

// BackupS3Config defines an S3-compatible storage target.
type BackupS3Config struct {
	Endpoint              string `yaml:"endpoint" json:"endpoint"`
	Bucket                string `yaml:"bucket" json:"bucket"`
	Region                string `yaml:"region" json:"region"`
	AccessKey             string `yaml:"access_key" json:"access_key"`
	SecretKey             string `yaml:"secret_key" json:"secret_key"`
	UsePathStyle          bool   `yaml:"use_path_style" json:"use_path_style"`
	InsecureSkipTLSVerify bool   `yaml:"insecure_skip_tls_verify" json:"insecure_skip_tls_verify"`
}

// SimConfig configures the local-dev production/operator simulation (edge side).
// Sim code is behind //go:build sim AND requires SHINGO_ALLOW_SIM=1 at runtime;
// this struct only carries the knobs. See implementation-brief.md.
type SimConfig struct {
	Enabled    bool               `yaml:"enabled"`
	Seed       int64              `yaml:"seed"`        // PRNG seed; 0 = derive from time and log it
	Speed      float64            `yaml:"speed"`       // time multiplier: 2.0 = twice as fast. Default 1.0
	MaxSpeed   float64            `yaml:"max_speed"`   // effective-speed cap; <=0 → default (15×). Past this the clock outruns the real choreography and the loop wedges, so requests are clamped. Must match core. Set very high to effectively uncap.
	Epoch      time.Time          `yaml:"epoch"`       // sim clock start (fast-forward origin). Zero = wall-now
	AnchorWall time.Time          `yaml:"anchor_wall"` // SHARED wall anchor for fast-forward sync: sim-now = epoch + speed×(wallNow−anchor). Set IDENTICALLY in core+edge to the run-start wall time so the two clocks stay in lockstep (no cross-process drift). Zero = per-process boot anchor (drifts).
	Calendar   SimCalendarConfig  `yaml:"calendar"`
	Downtime   SimDowntimeConfig  `yaml:"downtime"`
	Processes  []SimProcessConfig `yaml:"processes"`
	Operators  SimOperatorsConfig `yaml:"operators"`
}

// SimCalendarConfig defines the production calendar for the sim (G14).
// When enabled, the readiness gate also checks shift boundaries — cells
// don't cycle during breaks, between shifts, or on weekends.
type SimCalendarConfig struct {
	Enabled bool             `yaml:"enabled"` // default false (backward-compatible)
	Shifts  []SimShiftConfig `yaml:"shifts"`  // ordered by start time; default 3×8h
	Weekend []time.Weekday   `yaml:"weekend"` // days with no production; default [Saturday, Sunday]
}

// SimShiftConfig defines one shift in the production calendar.
type SimShiftConfig struct {
	Start string           `yaml:"start"`  // "HH:MM" 24h, e.g. "06:00"
	End   string           `yaml:"end"`    // "HH:MM" 24h, e.g. "14:00"
	Break []SimBreakConfig `yaml:"breaks"` // breaks within this shift
}

// SimBreakConfig defines a break within a shift.
type SimBreakConfig struct {
	Start string `yaml:"start"` // "HH:MM" 24h
	End   string `yaml:"end"`   // "HH:MM" 24h
}

// SimDowntimeConfig configures the clustered-random downtime model (G9, §3.1).
// Per-machine 85% uptime via exponential TBF + bounded-random MTTR draws.
// Disabled by default (backward-compatible).
type SimDowntimeConfig struct {
	Enabled  bool                       `yaml:"enabled"`  // default false
	Machines []SimDowntimeMachineConfig `yaml:"machines"` // per-machine knobs; empty = disabled
}

// SimDowntimeMachineConfig defines downtime parameters for one machine (PLC).
// Availability = MTBF / (MTBF + MTTR). With MTTR = random[min, max], the
// TBF is derived to hit the target availability.
type SimDowntimeMachineConfig struct {
	PLCName      string  `yaml:"plc_name"`
	Availability float64 `yaml:"availability"` // target availability (0-1), default 0.85
	MinMTTR      string  `yaml:"min_mttr"`     // minimum repair time, e.g. "5m"
	MaxMTTR      string  `yaml:"max_mttr"`     // maximum repair time, e.g. "30m"
}

// Scaled divides a duration by the speed multiplier. Zero or negative speed is
// treated as 1.0 (no scaling). Used to scale tick intervals, operator delays,
// transit times, etc. consistently across the sim.
func (s SimConfig) Scaled(d time.Duration) time.Duration {
	if s.Speed <= 0 {
		return d
	}
	return time.Duration(float64(d) / s.Speed)
}

// SimProcessConfig describes one fake PLC counter the fake WarLink advances.
// PLCName/TagName must exactly match a reporting_points row the seed tool creates.
type SimProcessConfig struct {
	PLCName      string        `yaml:"plc_name"`
	TagName      string        `yaml:"tag_name"`
	TickInterval time.Duration `yaml:"tick_interval"` // default 3s (applied at consumption)
	UOPPerTick   int64         `yaml:"uop_per_tick"`  // default 1 (applied at consumption)
}

// SimOperatorsConfig configures the auto-operator (loader auto-LOAD, unloader
// auto-CLEAR, changeover auto-cutover). Global enable only; per-node override is v2 (Q6).
type SimOperatorsConfig struct {
	Enabled               bool          `yaml:"enabled"`
	LoaderAutoLoad        time.Duration `yaml:"loader_auto_load"`        // default 5s
	UnloaderAutoClear     time.Duration `yaml:"unloader_auto_clear"`     // default 8s
	ChangeoverAutoCutover bool          `yaml:"changeover_auto_cutover"` // default true (T3.2)
	CutoverDelay          time.Duration `yaml:"cutover_delay"`           // default 10s
}

// Defaults returns a Config with sane defaults.
func Defaults() *Config {
	return &Config{
		// NO Namespace / LineID DEFAULTS. They were `plant-a` and `line-1`,
		// and Config.stationID() composed them into the station identity every
		// unenrolled edge in the fleet ran under — a value nobody typed and
		// nobody chose. install-edge.sh writes a placeholder config containing
		// only database_path, so a fresh install COULD NOT come up as anything
		// else. The duplicate-edge failure was never somebody making a
		// mistake; it was the system having a default answer to a question
		// that has no default.
		DatabasePath: "shingoedge.db",
		PollRate:     time.Second,
		WarLink: WarLinkConfig{
			Host:     "localhost",
			Port:     8080,
			PollRate: 2 * time.Second,
			Enabled:  true,
			Mode:     "sse",
		},
		Web: WebConfig{
			Host:          "0.0.0.0",
			Port:          8081,
			SessionSecret: generateSecret(),
		},
		Messaging: MessagingConfig{
			DispatchTopic:       "shingo.dispatch",
			OrdersTopic:         "shingo.orders",
			OutboxDrainInterval: 5 * time.Second,
			Kafka: KafkaConfig{
				Brokers: []string{},
			},
		},
		Counter: CounterConfig{
			JumpThreshold: 1000,
		},
		Backup: BackupConfig{
			Enabled:          false,
			ScheduleInterval: time.Hour,
			KeepHourly:       48,
			KeepDaily:        14,
			KeepWeekly:       8,
			KeepMonthly:      12,
			S3: BackupS3Config{
				Region:       "us-east-1",
				UsePathStyle: true,
			},
		},
		Sim: SimConfig{
			// Enabled false by default. Sim operator timings default here so a dev
			// YAML can enable sim without spelling out every knob; per-process
			// TickInterval/UOPPerTick default at consumption (fake WarLink).
			Operators: SimOperatorsConfig{
				LoaderAutoLoad:        5 * time.Second,
				UnloaderAutoClear:     8 * time.Second,
				ChangeoverAutoCutover: true,
				CutoverDelay:          10 * time.Second,
			},
		},
	}
}

// Load reads a YAML config file. If the file doesn't exist, defaults are used.
func Load(path string) (*Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	// Ensure session secret is never empty (YAML may have omitted it)
	if cfg.Web.SessionSecret == "" {
		cfg.Web.SessionSecret = generateSecret()
	}
	return cfg, nil
}

// Save writes the config to a YAML file.
func (c *Config) Save(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// stationID returns the station identity without locking (for internal use).
//
// THE UID, OR THE EXPLICIT OVERRIDE, AND NOTHING COMPOSED. The migration
// window's third branch — Namespace + "." + LineID — is gone with the defaults
// that fed it. An edge with neither value returns "" and mustHaveIdentity
// refuses to start; there is no longer a string this function can invent.
//
// Messaging.StationID survives as a deliberate operator override for a
// development or bench edge that is not enrolled against a Core. It is not a
// default: somebody has to write it in the file.
func (c *Config) stationID() string {
	if c.StationUID != "" {
		return c.StationUID
	}
	return c.Messaging.StationID
}

// StationID returns this edge's identity as it appears on the wire —
// protocol.Address.Station. Its value IS the station uid once enrolled.
func (c *Config) StationID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stationID()
}

// KafkaGroupID returns the Kafka consumer group ID for this edge.
//
// ALWAYS DERIVED. There is no configured override any more, and removing it is
// guard 4 — the deafness defect closed structurally rather than by a rule
// somebody has to follow.
//
// The defect: topics are created with NumPartitions=1, a consumer group
// assigns one partition to exactly one member, and this group id was the only
// thing distinguishing two edges. Two edges with the same station id joined one
// group on one partition, so ONE OF THEM RECEIVED NO DISPATCH TRAFFIC AT ALL
// while continuing to heartbeat, register and publish — no error anywhere.
//
// Deriving from the station uid means the group cannot collide unless the
// identity does, and the identity cannot collide because Core mints it. What
// made the old code unable to make that promise was not the derivation, which
// was already right; it was the `if GroupID != "" { return GroupID }` ahead of
// it, which let a persisted copy of a previous derivation outlive the value it
// was derived from. See KafkaConfig.GroupID for the measurement.
func (c *Config) KafkaGroupID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return "shingo-edge-" + c.stationID()
}

// StationUIDOrEmpty reports the enrolled identity, "" when this edge has not
// been enrolled. Distinct from StationID(), which falls back to the legacy
// derivation — the guard needs to know whether an identity EXISTS, not what
// string the edge would use in its absence.
func (c *Config) StationUIDOrEmpty() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.StationUID
}

// Lock acquires the config mutex for multi-step mutations.
func (c *Config) Lock() { c.mu.Lock() }

// Unlock releases the config mutex.
func (c *Config) Unlock() { c.mu.Unlock() }

// RLock acquires the config read lock.
func (c *Config) RLock() { c.mu.RLock() }

// RUnlock releases the config read lock.
func (c *Config) RUnlock() { c.mu.RUnlock() }

// NewInstanceID returns a random id identifying ONE RUN of this process.
//
// Not persisted, not derived from anything about the box, and deliberately
// not a UUID from a library — what it has to be is UNGUESSABLE-BY-A-CLONE and
// FRESH-PER-BOOT, and 8 random bytes are both. It is the only thing that
// separates two Pis flashed from the same SD image, which share a hostname,
// share a station_uid and are otherwise byte-identical to Core.
//
// GENERATE IT ONCE, IN THE COMPOSITION ROOT. setupKafkaSubscribers runs again
// on the Kafka-unreachable-at-startup retry path (main.go), so generating it
// inside the Heartbeater would draw a new value on a retry and Core would read
// a lease MOVE where nothing moved.
//
// A failed rand.Read yields "" rather than a constant. Core reads an empty
// instance as "cannot judge" and neither alarms nor binds on it — the same
// treatment an empty hostname gets. A fallback constant would be far worse:
// every edge that hit the error would share one instance and look like the
// clone case this exists to detect.
func NewInstanceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// generateSecret returns a random 32-byte hex-encoded string for session signing.
func generateSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "shingo-edge-fallback-secret"
	}
	return hex.EncodeToString(b)
}

// NewStationUID mints this edge's own identity when nobody has given it one.
//
// SAME SHAPE AS CORE'S MINTER, deliberately: 'stn-' + 16 hex. An operator
// reading a log line, a Kafka consumer group or a yaml file cannot tell whether
// Core minted the value or the edge did, and should not need to — the two are
// the same kind of thing. What differs is only whether a human has said what
// the station IS, and that lives on Core as claimed_at, not in the string.
//
// 64 bits is not birthday-collision-resistant and does not need to be. The
// property that matters is that two unconfigured edges cannot draw the SAME
// value, which is exactly what the old derived id did — 'plant-a.line-1' on
// every one of them, composed from two struct defaults. Randomness is what
// makes self-introduction safe; Core's unique index is what makes it certain.
func NewStationUID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return "stn-" + hex.EncodeToString(b)
}
