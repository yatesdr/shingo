package backup

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"shingoedge/config"
	"shingoedge/store"
)

type Service struct {
	db         *store.DB
	cfg        *config.Config
	configPath string
	appVersion string
	logf       func(string, ...any)

	mu             sync.RWMutex
	status         Status
	triggerCh      chan string
	stopCh         chan struct{}
	wg             sync.WaitGroup
	storageFactory func(config.BackupS3Config) (Storage, error)
}

func NewService(db *store.DB, cfg *config.Config, configPath, appVersion string, logf func(string, ...any)) *Service {
	if logf == nil {
		logf = log.Printf
	}
	svc := &Service{
		db:         db,
		cfg:        cfg,
		configPath: configPath,
		appVersion: appVersion,
		logf:       logf,
		triggerCh:  make(chan string, 64),
		stopCh:     make(chan struct{}),
		storageFactory: func(cfg config.BackupS3Config) (Storage, error) {
			return NewS3Storage(cfg)
		},
	}
	svc.refreshStaticStatus()
	if marker, err := PendingRestore(configPath); err == nil && marker != nil {
		svc.mu.Lock()
		svc.status.RestorePending = true
		svc.status.PendingRestoreKey = marker.Key
		svc.status.PendingRestoreTime = &marker.StagedAt
		svc.mu.Unlock()
	}
	return svc
}

func (s *Service) Start() {
	s.wg.Add(1)
	go s.loop()
}

func (s *Service) Stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	s.wg.Wait()
}

func (s *Service) RequestBackup(reason string) {
	select {
	case s.triggerCh <- reason:
	default:
		s.logf("backup: dropped trigger %q because queue is full", reason)
	}
}

func (s *Service) Status() Status {
	s.refreshStaticStatus()
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.status
	cp.PendingReasons = append([]string(nil), s.status.PendingReasons...)
	return cp
}

func (s *Service) ListBackups(ctx context.Context) ([]SnapshotInfo, error) {
	storage, stationID, _, err := s.storageFromConfig()
	if err != nil {
		return nil, err
	}
	out, err := listBackupsForStation(ctx, storage, stationID)
	if err != nil {
		return nil, err
	}
	pending, _ := PendingRestore(s.configPath)
	if pending != nil {
		for i := range out {
			if out[i].Key == pending.Key {
				out[i].RestorePending = true
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return snapshotTime(out[i]).After(snapshotTime(out[j]))
	})
	return out, nil
}

func (s *Service) TestConfig(ctx context.Context, s3Cfg config.BackupS3Config) error {
	storage, err := s.storageFactory(s3Cfg)
	if err != nil {
		return err
	}
	s.cfg.RLock()
	stationID := s.cfg.StationID()
	s.cfg.RUnlock()
	return storage.Test(ctx, stationID)
}

func (s *Service) RunNow(ctx context.Context, reason string) error {
	return s.runBackup(ctx, reason)
}

func (s *Service) StageRestore(ctx context.Context, key string) error {
	storage, stationID, _, err := s.storageFromConfig()
	if err != nil {
		return err
	}
	rc, err := storage.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()

	archiveData, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read backup archive: %w", err)
	}
	manifest, err := ReadManifestFromArchive(bytesReader(archiveData))
	if err != nil {
		return err
	}
	if manifest.StationID != stationID {
		return fmt.Errorf("backup station ID %q does not match current station %q", manifest.StationID, stationID)
	}
	if err := StageRestoreArchive(s.configPath, key, bytesReader(archiveData), stationID); err != nil {
		return err
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.status.RestorePending = true
	s.status.PendingRestoreKey = key
	s.status.PendingRestoreTime = &now
	s.mu.Unlock()
	return nil
}

func ListBackupsWithConfig(ctx context.Context, s3Cfg config.BackupS3Config, stationID string) ([]SnapshotInfo, error) {
	storage, err := NewS3Storage(s3Cfg)
	if err != nil {
		return nil, err
	}
	return listBackupsForStation(ctx, storage, stationID)
}

func RestoreNow(ctx context.Context, configPath string, s3Cfg config.BackupS3Config, stationID, key string) error {
	storage, err := NewS3Storage(s3Cfg)
	if err != nil {
		return err
	}
	rc, err := storage.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	return RestoreArchiveNow(configPath, rc, stationID)
}

func (s *Service) loop() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	pendingReasons := make(map[string]struct{})
	var debounce <-chan time.Time
	var debounceTimer *time.Timer

	for {
		select {
		case <-s.stopCh:
			return
		case reason := <-s.triggerCh:
			if reason == "" {
				reason = "manual"
			}
			pendingReasons[reason] = struct{}{}
			s.setPending(true, reasonSetToSlice(pendingReasons))
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(10 * time.Second)
				debounce = debounceTimer.C
			} else {
				if !debounceTimer.Stop() {
					select {
					case <-debounceTimer.C:
					default:
					}
				}
				debounceTimer.Reset(10 * time.Second)
			}
		case <-ticker.C:
			if !s.shouldRunScheduled() {
				continue
			}
			if err := s.runBackup(context.Background(), "scheduled"); err != nil {
				s.logf("backup: scheduled run failed: %v", err)
			}
		case <-debounce:
			reasons := reasonSetToSlice(pendingReasons)
			pendingReasons = make(map[string]struct{})
			s.setPending(false, nil)
			if err := s.runBackup(context.Background(), joinReasons(reasons)); err != nil {
				s.logf("backup: triggered run failed: %v", err)
			}
			debounce = nil
			debounceTimer = nil
		}
	}
}

func (s *Service) shouldRunScheduled() bool {
	s.cfg.RLock()
	enabled := s.cfg.Backup.Enabled
	interval := s.cfg.Backup.ScheduleInterval
	s.cfg.RUnlock()
	s.refreshStaticStatus()
	if !enabled {
		return false
	}
	if interval <= 0 {
		interval = time.Hour
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.status.Running || s.status.Pending {
		return false
	}
	if s.status.LastSuccessAt == nil {
		return true
	}
	return time.Since(*s.status.LastSuccessAt) >= interval
}

func (s *Service) runBackup(ctx context.Context, reason string) error {
	storage, stationID, backupCfg, err := s.storageFromConfig()
	if err != nil {
		s.markFailure(err)
		return err
	}
	s.markRunning(true, reason)
	defer s.markRunning(false, "")

	archivePath, manifest, archiveSize, cleanup, err := createSnapshotArchive(s.db, s.cfg, s.configPath, s.appVersion)
	if err != nil {
		s.markFailure(err)
		return err
	}
	defer cleanup()

	key := archiveKey(stationID, manifest.CreatedAt)
	f, err := os.Open(archivePath)
	if err != nil {
		s.markFailure(err)
		return fmt.Errorf("open archive for upload: %w", err)
	}
	defer f.Close()

	if err := storage.Put(ctx, key, f, archiveSize, map[string]string{
		"station-id":     stationID,
		"format-version": "1",
		"created-at":     manifest.CreatedAt.Format(time.RFC3339),
	}); err != nil {
		s.markFailure(err)
		return err
	}
	if err := s.prune(ctx, storage, backupCfg, stationID); err != nil {
		s.logf("backup: prune failed after upload: %v", err)
	}
	s.markSuccess(key)
	s.logf("backup: uploaded %s", key)
	return nil
}

func (s *Service) prune(ctx context.Context, storage Storage, backupCfg config.BackupConfig, stationID string) error {
	items, err := storage.List(ctx, objectPrefix(stationID))
	if err != nil {
		return err
	}
	snapshots := make([]SnapshotInfo, 0, len(items))
	for _, item := range items {
		snapshots = append(snapshots, SnapshotInfo{
			Key:          item.Key,
			Size:         item.Size,
			LastModified: item.LastModified,
		})
	}
	keep := retainedKeys(snapshots, backupCfg.KeepHourly, backupCfg.KeepDaily, backupCfg.KeepWeekly, backupCfg.KeepMonthly)
	for _, item := range snapshots {
		if _, ok := keep[item.Key]; ok {
			continue
		}
		if err := storage.Delete(ctx, item.Key); err != nil {
			s.logf("backup: prune delete failed for %s: %v", item.Key, err)
		}
	}
	return nil
}

func (s *Service) storageFromConfig() (Storage, string, config.BackupConfig, error) {
	s.cfg.RLock()
	stationID := s.cfg.StationID()
	backupCfg := s.cfg.Backup
	s.cfg.RUnlock()
	storage, err := s.storageFactory(backupCfg.S3)
	if err != nil {
		return nil, "", backupCfg, err
	}
	return storage, stationID, backupCfg, nil
}

func (s *Service) refreshStaticStatus() {
	s.cfg.RLock()
	enabled := s.cfg.Backup.Enabled
	interval := s.cfg.Backup.ScheduleInterval
	s.cfg.RUnlock()
	if interval <= 0 {
		interval = time.Hour
	}
	next := time.Now().UTC().Add(interval)
	s.mu.Lock()
	s.status.Enabled = enabled
	s.status.ScheduleInterval = interval.String()
	if s.status.LastSuccessAt != nil {
		t := s.status.LastSuccessAt.Add(interval)
		next = t
		s.status.LastSuccessAgeSec = int64(time.Since(*s.status.LastSuccessAt).Seconds())
		s.status.Stale = enabled && time.Since(*s.status.LastSuccessAt) > (interval*2)
		if s.status.Stale {
			s.status.StaleReason = "last successful backup is older than twice the configured interval"
		} else {
			s.status.StaleReason = ""
		}
	} else {
		s.status.LastSuccessAgeSec = 0
		s.status.Stale = enabled
		if enabled {
			s.status.StaleReason = "no successful backup has been recorded yet"
		} else {
			s.status.StaleReason = ""
		}
	}
	s.status.NextScheduledAt = &next
	s.mu.Unlock()
}

func (s *Service) setPending(pending bool, reasons []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Pending = pending
	s.status.PendingReasons = reasons
}

func (s *Service) markRunning(running bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Running = running
	if running {
		s.status.LastRunReason = reason
		s.status.LastError = ""
	}
}

func (s *Service) markSuccess(key string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastSuccessAt = &now
	s.status.LastSuccessKey = key
	s.status.LastError = ""
}

func (s *Service) markFailure(err error) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastFailureAt = &now
	s.status.LastError = err.Error()
}

func reasonSetToSlice(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for reason := range m {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}

func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return "manual"
	}
	return fmt.Sprintf("auto:%s", stringJoin(reasons, ", "))
}

func stringJoin(items []string, sep string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	n := len(sep) * (len(items) - 1)
	for _, item := range items {
		n += len(item)
	}
	buf := make([]byte, 0, n)
	for i, item := range items {
		if i > 0 {
			buf = append(buf, sep...)
		}
		buf = append(buf, item...)
	}
	return string(buf)
}

func bytesReader(b []byte) io.Reader {
	return &byteReader{b: b}
}

func listBackupsForStation(ctx context.Context, storage Storage, stationID string) ([]SnapshotInfo, error) {
	items, err := storage.List(ctx, objectPrefix(stationID))
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotInfo, 0, len(items))
	for _, item := range items {
		snap := SnapshotInfo{
			Key:          item.Key,
			Size:         item.Size,
			LastModified: item.LastModified,
		}
		if ts, ok := inferSnapshotTime(item.Key); ok {
			snap.CreatedAt = &ts
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool {
		return snapshotTime(out[i]).After(snapshotTime(out[j]))
	})
	return out, nil
}

func inferSnapshotTime(key string) (time.Time, bool) {
	base := key
	if idx := lastSlash(key); idx >= 0 {
		base = key[idx+1:]
	}
	base = strings.TrimSuffix(base, ".tar.gz")
	ts, err := time.Parse("2006-01-02T15-04-05Z", base)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

type byteReader struct {
	b []byte
	i int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
