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
	"strings"
	"time"

	"shingoedge/config"
	"shingoedge/store"
)

func createSnapshotArchive(db *store.DB, cfg *config.Config, configPath, appVersion string) (string, *Manifest, int64, func(), error) {
	tmpDir, err := os.MkdirTemp("", "shingoedge-backup-*")
	if err != nil {
		return "", nil, 0, nil, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	configCopy := filepath.Join(tmpDir, ConfigEntryName)
	dbCopy := filepath.Join(tmpDir, DBEntryName)
	archivePath := filepath.Join(tmpDir, archiveFileName(time.Now().UTC()))

	if err := copyFile(configPath, configCopy); err != nil {
		cleanup()
		return "", nil, 0, nil, err
	}
	if err := vacuumInto(db, dbCopy); err != nil {
		cleanup()
		return "", nil, 0, nil, err
	}

	cfg.RLock()
	stationID := cfg.StationID()
	cfg.RUnlock()

	createdAt := time.Now().UTC()
	manifest := &Manifest{
		FormatVersion: FormatVersion,
		StationID:     stationID,
		CreatedAt:     createdAt,
		AppVersion:    appVersion,
	}
	for _, name := range []string{ConfigEntryName, DBEntryName} {
		path := filepath.Join(tmpDir, name)
		info, err := os.Stat(path)
		if err != nil {
			cleanup()
			return "", nil, 0, nil, fmt.Errorf("stat %s: %w", name, err)
		}
		sum, err := fileSHA256(path)
		if err != nil {
			cleanup()
			return "", nil, 0, nil, fmt.Errorf("hash %s: %w", name, err)
		}
		manifest.Files = append(manifest.Files, ManifestFile{Name: name, Size: info.Size(), SHA256: sum})
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		cleanup()
		return "", nil, 0, nil, fmt.Errorf("marshal manifest: %w", err)
	}

	f, err := os.Create(archivePath)
	if err != nil {
		cleanup()
		return "", nil, 0, nil, fmt.Errorf("create archive: %w", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	if err := writeTarEntry(tw, ManifestName, manifestBytes, createdAt); err != nil {
		cleanup()
		return "", nil, 0, nil, err
	}
	for _, name := range []string{ConfigEntryName, DBEntryName} {
		if err := writeTarFile(tw, filepath.Join(tmpDir, name), name, createdAt); err != nil {
			cleanup()
			return "", nil, 0, nil, err
		}
	}
	if err := tw.Close(); err != nil {
		cleanup()
		return "", nil, 0, nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		cleanup()
		return "", nil, 0, nil, fmt.Errorf("close gzip: %w", err)
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		cleanup()
		return "", nil, 0, nil, fmt.Errorf("stat archive: %w", err)
	}
	return archivePath, manifest, info.Size(), cleanup, nil
}

func archiveFileName(ts time.Time) string {
	return ts.UTC().Format("2006-01-02T15-04-05Z") + ".tar.gz"
}

func archiveKey(stationID string, ts time.Time) string {
	prefix := objectPrefix(stationID)
	return fmt.Sprintf("%s%s/%s/%s", prefix, ts.UTC().Format("2006"), ts.UTC().Format("01"), ts.UTC().Format("02/")+archiveFileName(ts))
}

func objectPrefix(stationID string) string {
	return sanitizeKeyPart(stationID) + "/"
}

func sanitizeKeyPart(v string) string {
	v = strings.TrimSpace(v)
	v = strings.ReplaceAll(v, "\\", "_")
	v = strings.ReplaceAll(v, "/", "_")
	if v == "" {
		return "unknown-station"
	}
	return v
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

func vacuumInto(db *store.DB, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
	}
	escaped := strings.ReplaceAll(dest, "'", "''")
	if _, err := db.Exec("VACUUM INTO '" + escaped + "'"); err != nil {
		return fmt.Errorf("sqlite vacuum into: %w", err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeTarEntry(tw *tar.Writer, name string, data []byte, modTime time.Time) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    int64(len(data)),
		ModTime: modTime,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write tar data %s: %w", name, err)
	}
	return nil
}

func writeTarFile(tw *tar.Writer, srcPath, entryName string, modTime time.Time) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", srcPath, err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    entryName,
		Mode:    0o644,
		Size:    info.Size(),
		ModTime: modTime,
	}); err != nil {
		return fmt.Errorf("write tar header %s: %w", entryName, err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("write tar file %s: %w", entryName, err)
	}
	return nil
}
