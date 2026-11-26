package backup

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"shingoedge/config"
	"shingoedge/store"
)

// A backup's database is asked what `migrate check` asks before it can replace
// the live one: when it is staged (the person who picked it reads the
// sentence), in `migrate check` while it waits (the installer reads it), and
// when it is applied, before the config or the database is written.

// unpacked is a backup's archive, unpacked and verified.
type unpacked struct {
	config     []byte         // the archived config, as stored
	restored   *config.Config // the same, parsed: it names where the database goes
	dbFilePath string         // the archived database, in the scratch directory
}

// readBackup unpacks archivePath into dir and verifies it: its manifest,
// sizes and checksums, and, where one is expected, its station.
func readBackup(archivePath, expectedStationID, dir string) (*unpacked, error) {
	manifest, files, err := extractArchive(archivePath, dir)
	if err != nil {
		return nil, err
	}
	if err := verifyManifest(manifest, files); err != nil {
		return nil, err
	}
	if expectedStationID != "" && manifest.StationID != expectedStationID {
		return nil, fmt.Errorf("backup station ID %q does not match expected station %q", manifest.StationID, expectedStationID)
	}
	cfgData, err := os.ReadFile(files[ConfigEntryName])
	if err != nil {
		return nil, fmt.Errorf("read restored config: %w", err)
	}
	restored := config.Defaults()
	if err := yaml.Unmarshal(cfgData, restored); err != nil {
		return nil, fmt.Errorf("parse restored config: %w", err)
	}
	if files[DBEntryName] == "" {
		return nil, fmt.Errorf("archive missing %s", DBEntryName)
	}
	return &unpacked{config: cfgData, restored: restored, dbFilePath: files[DBEntryName]}, nil
}

// checkSchema refuses a backup this build would not start on.
func (u *unpacked) checkSchema() error {
	if _, _, err := store.CheckDatabase(u.dbFilePath); err != nil {
		return fmt.Errorf("backup refused: %w", err)
	}
	return nil
}

// checkArchive reads and checks an archive in a private temp directory, and
// writes nothing else.
func checkArchive(archivePath, expectedStationID string) error {
	dir, err := os.MkdirTemp("", "shingoedge-restore-check-*")
	if err != nil {
		return fmt.Errorf("create restore check dir: %w", err)
	}
	defer os.RemoveAll(dir)
	u, err := readBackup(archivePath, expectedStationID, dir)
	if err != nil {
		return err
	}
	return u.checkSchema()
}

// checkArchiveData is checkArchive for an archive not yet on disk.
func checkArchiveData(data []byte, expectedStationID string) error {
	dir, err := os.MkdirTemp("", "shingoedge-restore-stage-*")
	if err != nil {
		return fmt.Errorf("create restore check dir: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "restore.tar.gz")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write restore check archive: %w", err)
	}
	return checkArchive(path, expectedStationID)
}

// CheckPendingRestore checks the staged restore the next start would apply. It
// returns false and nil when none is staged.
func CheckPendingRestore(configPath string) (staged bool, err error) {
	marker, err := loadRestoreMarker(configPath)
	if err != nil || marker == nil {
		return false, err
	}
	return true, checkArchive(marker.Archive, marker.StationID)
}
