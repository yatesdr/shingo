package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"shingoedge/config"
)

func ApplyPendingRestore(configPath string, logf func(string, ...any)) error {
	marker, err := loadRestoreMarker(configPath)
	if err != nil {
		return err
	}
	if marker == nil {
		return nil
	}
	if logf != nil {
		logf("backup: applying staged restore from %s", marker.Key)
	}

	tmpDir, err := os.MkdirTemp("", "shingoedge-restore-*")
	if err != nil {
		return fmt.Errorf("create restore temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := restoreArchivePath(configPath, marker.Archive, marker.StationID); err != nil {
		return err
	}

	if err := clearRestoreMarker(configPath); err != nil {
		return err
	}
	if logf != nil {
		logf("backup: staged restore applied successfully")
	}
	return nil
}

func StageRestoreArchive(configPath, key string, archive io.Reader, stationID string) error {
	stateDir := stateDir(configPath)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	archivePath := filepath.Join(stateDir, "pending-restore.tar.gz")
	tmpArchive := archivePath + ".tmp"
	f, err := os.Create(tmpArchive)
	if err != nil {
		return fmt.Errorf("create staged restore archive: %w", err)
	}
	if _, err := io.Copy(f, archive); err != nil {
		f.Close()
		_ = os.Remove(tmpArchive)
		return fmt.Errorf("write staged restore archive: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpArchive)
		return fmt.Errorf("close staged restore archive: %w", err)
	}
	if err := os.Rename(tmpArchive, archivePath); err != nil {
		_ = os.Remove(tmpArchive)
		return fmt.Errorf("activate staged restore archive: %w", err)
	}
	marker := RestoreMarker{
		Key:       key,
		StagedAt:  time.Now().UTC(),
		Archive:   archivePath,
		StationID: stationID,
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal restore marker: %w", err)
	}
	if err := atomicWrite(restoreMarkerPath(configPath), data, 0o644); err != nil {
		return err
	}
	return nil
}

func PendingRestore(configPath string) (*RestoreMarker, error) {
	return loadRestoreMarker(configPath)
}

func RestoreArchiveNow(configPath string, archive io.Reader, expectedStationID string) error {
	tmpDir, err := os.MkdirTemp("", "shingoedge-restore-direct-*")
	if err != nil {
		return fmt.Errorf("create direct restore temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, "restore.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("create direct restore archive: %w", err)
	}
	if _, err := io.Copy(f, archive); err != nil {
		f.Close()
		return fmt.Errorf("write direct restore archive: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close direct restore archive: %w", err)
	}
	return restoreArchivePath(configPath, archivePath, expectedStationID)
}

func clearRestoreMarker(configPath string) error {
	markerPath := restoreMarkerPath(configPath)
	marker, _ := loadRestoreMarker(configPath)
	if marker != nil && marker.Archive != "" {
		_ = os.Remove(marker.Archive)
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove restore marker: %w", err)
	}
	return nil
}

func loadRestoreMarker(configPath string) (*RestoreMarker, error) {
	data, err := os.ReadFile(restoreMarkerPath(configPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read restore marker: %w", err)
	}
	var marker RestoreMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, fmt.Errorf("parse restore marker: %w", err)
	}
	return &marker, nil
}

func restoreArchivePath(configPath, archivePath, expectedStationID string) error {
	tmpDir, err := os.MkdirTemp("", "shingoedge-restore-*")
	if err != nil {
		return fmt.Errorf("create restore temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	manifest, files, err := extractArchive(archivePath, tmpDir)
	if err != nil {
		return err
	}
	if err := verifyManifest(manifest, files); err != nil {
		return err
	}
	if expectedStationID != "" && manifest.StationID != expectedStationID {
		return fmt.Errorf("backup station ID %q does not match expected station %q", manifest.StationID, expectedStationID)
	}

	cfgData, err := os.ReadFile(files[ConfigEntryName])
	if err != nil {
		return fmt.Errorf("read restored config: %w", err)
	}
	restoredCfg := config.Defaults()
	if err := yaml.Unmarshal(cfgData, restoredCfg); err != nil {
		return fmt.Errorf("parse restored config: %w", err)
	}
	dbData, err := os.ReadFile(files[DBEntryName])
	if err != nil {
		return fmt.Errorf("read restored db: %w", err)
	}
	if err := atomicWrite(configPath, cfgData, 0o644); err != nil {
		return fmt.Errorf("write restored config: %w", err)
	}
	if err := atomicWrite(restoredCfg.DatabasePath, dbData, 0o644); err != nil {
		return fmt.Errorf("write restored db: %w", err)
	}
	return nil
}

func extractArchive(path, destDir string) (*Manifest, map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, fmt.Errorf("open gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := make(map[string]string)
	var manifest *Manifest
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		target := filepath.Join(destDir, filepath.Base(hdr.Name))
		out, err := os.Create(target)
		if err != nil {
			return nil, nil, fmt.Errorf("create extracted file: %w", err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return nil, nil, fmt.Errorf("extract %s: %w", hdr.Name, err)
		}
		if err := out.Close(); err != nil {
			return nil, nil, fmt.Errorf("close extracted %s: %w", hdr.Name, err)
		}
		files[filepath.Base(hdr.Name)] = target
		if filepath.Base(hdr.Name) == ManifestName {
			data, err := os.ReadFile(target)
			if err != nil {
				return nil, nil, fmt.Errorf("read manifest: %w", err)
			}
			var m Manifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, nil, fmt.Errorf("parse manifest: %w", err)
			}
			manifest = &m
		}
	}
	if manifest == nil {
		return nil, nil, fmt.Errorf("manifest missing from archive")
	}
	return manifest, files, nil
}

func verifyManifest(manifest *Manifest, files map[string]string) error {
	if manifest.FormatVersion != FormatVersion {
		return fmt.Errorf("unsupported backup format version: %d", manifest.FormatVersion)
	}
	for _, file := range manifest.Files {
		path := files[file.Name]
		if path == "" {
			return fmt.Errorf("archive missing %s", file.Name)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat extracted %s: %w", file.Name, err)
		}
		if info.Size() != file.Size {
			return fmt.Errorf("size mismatch for %s", file.Name)
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return fmt.Errorf("hash %s: %w", file.Name, err)
		}
		if sum != file.SHA256 {
			return fmt.Errorf("checksum mismatch for %s", file.Name)
		}
	}
	return nil
}

func ReadManifestFromArchive(r io.Reader) (*Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("open gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("read manifest header: %w", err)
	}
	if filepath.Base(hdr.Name) != ManifestName {
		return nil, fmt.Errorf("manifest not first entry")
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &manifest, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("write temp %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

func restoreMarkerPath(configPath string) string {
	return filepath.Join(stateDir(configPath), "restore.json")
}

func stateDir(configPath string) string {
	dir := filepath.Dir(configPath)
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, ".shingoedge-backup")
}

// TODO(dead-code): no callers as of 2026-04-17; verify before the next refactor.
func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
