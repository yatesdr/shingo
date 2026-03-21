package backup

import "time"

const (
	FormatVersion   = 1
	ManifestName    = "manifest.json"
	ConfigEntryName = "shingoedge.yaml"
	DBEntryName     = "shingoedge.db"
)

type Manifest struct {
	FormatVersion int            `json:"format_version"`
	StationID     string         `json:"station_id"`
	CreatedAt     time.Time      `json:"created_at"`
	AppVersion    string         `json:"app_version"`
	Files         []ManifestFile `json:"files"`
}

type ManifestFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type ObjectInfo struct {
	Key          string     `json:"key"`
	Size         int64      `json:"size"`
	LastModified *time.Time `json:"last_modified,omitempty"`
}

type SnapshotInfo struct {
	Key            string     `json:"key"`
	Size           int64      `json:"size"`
	LastModified   *time.Time `json:"last_modified,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	StationID      string     `json:"station_id,omitempty"`
	FormatVersion  int        `json:"format_version,omitempty"`
	RestorePending bool       `json:"restore_pending"`
}

type Status struct {
	Enabled            bool       `json:"enabled"`
	ScheduleInterval   string     `json:"schedule_interval,omitempty"`
	Running            bool       `json:"running"`
	Pending            bool       `json:"pending"`
	PendingReasons     []string   `json:"pending_reasons,omitempty"`
	LastRunReason      string     `json:"last_run_reason,omitempty"`
	LastSuccessAt      *time.Time `json:"last_success_at,omitempty"`
	LastSuccessAgeSec  int64      `json:"last_success_age_sec,omitempty"`
	LastSuccessKey     string     `json:"last_success_key,omitempty"`
	LastFailureAt      *time.Time `json:"last_failure_at,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
	NextScheduledAt    *time.Time `json:"next_scheduled_at,omitempty"`
	Stale              bool       `json:"stale"`
	StaleReason        string     `json:"stale_reason,omitempty"`
	RestorePending     bool       `json:"restore_pending"`
	PendingRestoreKey  string     `json:"pending_restore_key,omitempty"`
	PendingRestoreTime *time.Time `json:"pending_restore_time,omitempty"`
}

type RestoreMarker struct {
	Key       string    `json:"key"`
	StagedAt  time.Time `json:"staged_at"`
	Archive   string    `json:"archive"`
	StationID string    `json:"station_id"`
}
