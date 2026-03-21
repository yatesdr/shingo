package plc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"shingoedge/config"
	"shingoedge/store"
)

// TagInfo describes a tag available on a PLC.
type TagInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// TagValue is a cached tag value from WarLink.
type TagValue struct {
	Name    string
	TypeStr string
	Value   interface{}
	Error   string
}

// PLCHealth tracks the health state of a PLC from WarLink SSE health events.
type PLCHealth struct {
	Online    bool   `json:"online"`
	Driver    string `json:"driver"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Timestamp string `json:"timestamp"`
}

// ManagedPLC tracks per-PLC state from WarLink discovery.
type ManagedPLC struct {
	Name        string
	Status      string
	Error       string
	ProductName string              // from status-change
	Vendor      string              // from status-change
	Values      map[string]TagValue
	Health      *PLCHealth          // nil until first health event
	mu          sync.RWMutex
}

// DebugLogFunc is a nil-safe debug logging function.
type DebugLogFunc func(format string, args ...any)

func (fn DebugLogFunc) log(format string, args ...any) {
	if fn != nil {
		fn(format, args...)
	}
}

// Manager manages PLC data via WarLink HTTP polling or SSE and counter polling.
type Manager struct {
	mu      sync.RWMutex
	db      *store.DB
	cfg     *config.Config
	emitter EventEmitter
	wl      WarlinkClient
	plcs    map[string]*ManagedPLC

	DebugLog DebugLogFunc

	warlinkConnected bool
	warlinkError     error

	stopChan        chan struct{}
	warlinkStopChan chan struct{}
	warlinkRunning  bool
	warlinkWg       sync.WaitGroup
	wg              sync.WaitGroup

	sseCancel context.CancelFunc
}

// NewManager creates a PLC manager.
func NewManager(db *store.DB, cfg *config.Config, emitter EventEmitter) *Manager {
	return &Manager{
		db:      db,
		cfg:     cfg,
		emitter: emitter,
		wl:      NewWarlinkClient(fmt.Sprintf("http://%s:%d/api", cfg.WarLink.Host, cfg.WarLink.Port)),
		plcs:    make(map[string]*ManagedPLC),

		stopChan: make(chan struct{}),
	}
}

// baseURL returns the WarLink base URL built from host+port config.
func (m *Manager) baseURL() string {
	return fmt.Sprintf("http://%s:%d/api", m.cfg.WarLink.Host, m.cfg.WarLink.Port)
}

// ReplaceClient swaps the WarLink client (used when config changes at runtime).
func (m *Manager) ReplaceClient(wl WarlinkClient) {
	m.mu.Lock()
	m.wl = wl
	m.mu.Unlock()
}

// StartWarLinkPoller starts the goroutine that fetches PLC and tag data from WarLink.
// The mode is determined by Config.WarLink.Mode: "sse" for server-sent events,
// anything else (including the default "") for HTTP polling.
func (m *Manager) StartWarLinkPoller() {
	m.mu.Lock()
	if m.warlinkRunning {
		m.mu.Unlock()
		return
	}
	m.warlinkStopChan = make(chan struct{})
	m.warlinkRunning = true
	m.mu.Unlock()

	m.warlinkWg.Add(1)
	if m.cfg.WarLink.Mode == "sse" {
		go m.sseLoop()
	} else {
		go m.warlinkPollLoop()
	}
}

// StopWarLinkPoller stops the WarLink polling/SSE goroutine and resets connection state.
func (m *Manager) StopWarLinkPoller() {
	m.mu.Lock()
	if !m.warlinkRunning {
		m.mu.Unlock()
		return
	}
	close(m.warlinkStopChan)
	m.warlinkRunning = false
	if m.sseCancel != nil {
		m.sseCancel()
		m.sseCancel = nil
	}
	m.mu.Unlock()

	m.warlinkWg.Wait()

	// Reset connection state
	m.mu.Lock()
	wasConnected := m.warlinkConnected
	if wasConnected {
		m.warlinkConnected = false
		m.warlinkError = nil
	}
	var disconnectedPLCs []string
	for _, mp := range m.plcs {
		mp.mu.Lock()
		if mp.Status == "Connected" {
			mp.Status = "Disconnected"
			disconnectedPLCs = append(disconnectedPLCs, mp.Name)
		}
		mp.mu.Unlock()
	}
	m.mu.Unlock()

	if wasConnected {
		m.emitter.EmitWarLinkDisconnected(nil)
	}
	for _, name := range disconnectedPLCs {
		m.emitter.EmitPLCDisconnected(name, nil)
	}
}

func (m *Manager) warlinkPollLoop() {
	defer m.warlinkWg.Done()

	getPollRate := func() time.Duration {
		d := m.cfg.WarLink.PollRate
		if d <= 0 {
			return 2 * time.Second
		}
		return d
	}

	ticker := time.NewTicker(getPollRate())
	defer ticker.Stop()

	// Do an immediate first poll
	m.DebugLog.log("warlink poll loop started (mode=poll)")
	m.warlinkPollTick()

	for {
		select {
		case <-m.stopChan:
			return
		case <-m.warlinkStopChan:
			return
		case <-ticker.C:
			m.warlinkPollTick()
			ticker.Reset(getPollRate())
		}
	}
}

func (m *Manager) warlinkPollTick() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	plcs, err := m.fetchPLCs(ctx)
	if err != nil {
		m.mu.Lock()
		wasConnected := m.warlinkConnected
		if wasConnected {
			m.warlinkConnected = false
			m.warlinkError = err
		}
		m.mu.Unlock()
		if wasConnected {
			log.Printf("WarLink connection lost: %v", err)
			m.DebugLog.log("warlink disconnected: %v", err)
			m.emitter.EmitWarLinkDisconnected(err)
		}
		return
	}

	m.mu.Lock()
	wasDisconnected := !m.warlinkConnected
	m.warlinkConnected = true
	m.warlinkError = nil
	m.mu.Unlock()
	if wasDisconnected {
		log.Printf("WarLink connected: %s", m.baseURL())
		m.DebugLog.log("warlink connected: %s", m.baseURL())
	}

	// Track which PLCs we've seen this tick for status transitions
	seen := make(map[string]bool)

	for _, p := range plcs {
		seen[p.Name] = true

		m.mu.Lock()
		existing, exists := m.plcs[p.Name]
		if !exists {
			existing = &ManagedPLC{
				Name:   p.Name,
				Values: make(map[string]TagValue),
			}
			m.plcs[p.Name] = existing
		}
		m.mu.Unlock()

		existing.mu.Lock()
		oldStatus := existing.Status
		existing.Status = p.Status
		existing.Error = p.Error
		existing.mu.Unlock()

		// Emit connection transitions
		if p.Status == "Connected" && oldStatus != "Connected" {
			m.DebugLog.log("plc connected: %s", p.Name)
			m.emitter.EmitPLCConnected(p.Name)
		} else if p.Status != "Connected" && oldStatus == "Connected" {
			var emitErr error
			if p.Error != "" {
				emitErr = fmt.Errorf("%s", p.Error)
			}
			m.DebugLog.log("plc disconnected: %s err=%v", p.Name, emitErr)
			m.emitter.EmitPLCDisconnected(p.Name, emitErr)
		}

		// Fetch tags for connected PLCs
		if p.Status == "Connected" {
			tags, err := m.fetchTags(ctx, p.Name)
			if err != nil {
				log.Printf("WarLink fetch tags %s: %v", p.Name, err)
				continue
			}
			m.applyTags(p.Name, tags)
		}
	}

	// Detect PLCs that disappeared from WarLink
	m.mu.RLock()
	var removed []string
	for name, mp := range m.plcs {
		mp.mu.RLock()
		isConnected := mp.Status == "Connected"
		mp.mu.RUnlock()
		if !seen[name] && isConnected {
			removed = append(removed, name)
		}
	}
	m.mu.RUnlock()

	for _, name := range removed {
		m.mu.RLock()
		mp := m.plcs[name]
		m.mu.RUnlock()
		mp.mu.Lock()
		mp.Status = "Disconnected"
		mp.mu.Unlock()
		m.emitter.EmitPLCDisconnected(name, fmt.Errorf("removed from WarLink"))
	}

	// Emit after all PLCs are in the map so the API returns the full list
	if wasDisconnected {
		m.emitter.EmitWarLinkConnected()
	}
}

func (m *Manager) fetchPLCs(ctx context.Context) ([]WarlinkPLC, error) {
	return m.wl.ListPLCs(ctx)
}

func (m *Manager) fetchTags(ctx context.Context, plcName string) (map[string]WarlinkTag, error) {
	return m.wl.ListTags(ctx, plcName)
}

func (m *Manager) applyTags(plcName string, tags map[string]WarlinkTag) {
	m.mu.RLock()
	mp, ok := m.plcs[plcName]
	m.mu.RUnlock()
	if !ok {
		return
	}

	mp.mu.Lock()
	defer mp.mu.Unlock()

	// Build fresh values map, stripping PLCName. prefix from keys
	prefix := plcName + "."
	newValues := make(map[string]TagValue, len(tags))
	for key, tag := range tags {
		name := strings.TrimPrefix(key, prefix)
		newValues[name] = TagValue{
			Name:    name,
			TypeStr: tag.Type,
			Value:   tag.Value,
			Error:   tag.Error,
		}
	}
	mp.Values = newValues
}

// IsWarLinkConnected returns whether we can reach WarLink.
func (m *Manager) IsWarLinkConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.warlinkConnected
}

// WarLinkError returns the last WarLink connection error, if any.
func (m *Manager) WarLinkError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.warlinkError
}

// PLCNames returns the names of all discovered PLCs, sorted.
func (m *Manager) PLCNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.plcs))
	for name := range m.plcs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GetPLC returns the managed PLC state, or nil if not found.
func (m *Manager) GetPLC(name string) *ManagedPLC {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.plcs[name]
}

// GetPLCHealth returns the health state for a PLC, or nil if unknown.
func (m *Manager) GetPLCHealth(name string) *PLCHealth {
	m.mu.RLock()
	mp, ok := m.plcs[name]
	m.mu.RUnlock()
	if !ok || mp.Health == nil {
		return nil
	}
	mp.mu.RLock()
	defer mp.mu.RUnlock()
	h := *mp.Health
	return &h
}

// PLCStatuses returns a map of PLC name to connection status for all discovered PLCs.
func (m *Manager) PLCStatuses() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	statuses := make(map[string]string, len(m.plcs))
	for name, mp := range m.plcs {
		mp.mu.RLock()
		statuses[name] = mp.Status
		mp.mu.RUnlock()
	}
	return statuses
}

// IsConnected returns whether a PLC is currently connected via WarLink.
func (m *Manager) IsConnected(name string) bool {
	m.mu.RLock()
	mp, ok := m.plcs[name]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	mp.mu.RLock()
	defer mp.mu.RUnlock()
	return mp.Status == "Connected"
}

// ReadTag reads a single tag from the WarLink cache.
func (m *Manager) ReadTag(plcName, tagName string) (interface{}, error) {
	m.mu.RLock()
	mp, ok := m.plcs[plcName]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("PLC %s not found", plcName)
	}

	mp.mu.RLock()
	defer mp.mu.RUnlock()

	tv, ok := mp.Values[tagName]
	if !ok {
		return nil, fmt.Errorf("tag %s not found on %s", tagName, plcName)
	}
	if tv.Error != "" {
		return nil, fmt.Errorf("tag %s error: %s", tagName, tv.Error)
	}
	return tv.Value, nil
}

// DiscoverTags returns all tags from the WarLink cache for a PLC.
func (m *Manager) DiscoverTags(plcName string) ([]TagInfo, error) {
	m.mu.RLock()
	mp, ok := m.plcs[plcName]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("PLC %s not found", plcName)
	}

	mp.mu.RLock()
	defer mp.mu.RUnlock()

	tags := make([]TagInfo, 0, len(mp.Values))
	for _, tv := range mp.Values {
		tags = append(tags, TagInfo{Name: tv.Name, Type: tv.TypeStr})
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Name < tags[j].Name })
	return tags, nil
}

// StartPolling starts the counter polling loop for all enabled reporting points.
func (m *Manager) StartPolling() {
	m.wg.Add(1)
	go m.pollLoop()
}

// Stop stops the polling loops and cleans up.
func (m *Manager) Stop() {
	select {
	case <-m.stopChan:
	default:
		close(m.stopChan)
	}
	m.wg.Wait()
}

// --- Polling ---

func (m *Manager) pollLoop() {
	defer m.wg.Done()
	pollRate := m.cfg.PollRate
	if pollRate <= 0 {
		pollRate = time.Second
	}
	ticker := time.NewTicker(pollRate)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.pollAllReportingPoints()
		}
	}
}

func (m *Manager) pollAllReportingPoints() {
	rps, err := m.db.ListEnabledReportingPoints()
	if err != nil {
		log.Printf("list reporting points: %v", err)
		return
	}

	for _, rp := range rps {
		m.pollReportingPoint(rp)
	}
}

func (m *Manager) pollReportingPoint(rp store.ReportingPoint) {
	val, err := m.ReadTag(rp.PLCName, rp.TagName)
	if err != nil {
		m.DebugLog.log("tag read error: %s/%s rp=%d: %v", rp.PLCName, rp.TagName, rp.ID, err)
		log.Printf("read tag %s/%s (rp %d): %v", rp.PLCName, rp.TagName, rp.ID, err)
		m.emitter.EmitCounterReadError(rp.ID, rp.PLCName, rp.TagName, err.Error())
		return
	}

	newCount, ok := toInt64(val)
	if !ok {
		return
	}

	m.emitter.EmitCounterRead(rp.ID, rp.PLCName, rp.TagName, newCount)

	delta, anomaly := CalculateDelta(rp.LastCount, newCount, m.cfg.Counter.JumpThreshold)
	if delta == 0 && anomaly == "" {
		return
	}

	m.DebugLog.log("counter delta: rp=%d %s/%s last=%d new=%d delta=%d anomaly=%s",
		rp.ID, rp.PLCName, rp.TagName, rp.LastCount, newCount, delta, anomaly)

	// Record snapshot
	confirmed := anomaly != "jump"
	snapID, err := m.db.InsertCounterSnapshot(rp.ID, newCount, delta, anomaly, confirmed)
	if err != nil {
		log.Printf("insert counter snapshot: %v", err)
		return
	}

	// Update the reporting point's last known count
	if err := m.db.UpdateReportingPointCounter(rp.ID, newCount); err != nil {
		log.Printf("update reporting point counter: %v", err)
	}

	if anomaly != "" {
		m.emitter.EmitCounterAnomaly(snapID, rp.ID, rp.PLCName, rp.TagName, rp.LastCount, newCount, anomaly)
	}

	// Only emit delta for normal counts and resets (not jumps, which need operator confirmation)
	if rp.JobStyleID == 0 {
		return // no job style linked
	}
	if anomaly != "jump" && delta > 0 {
		m.emitter.EmitCounterDelta(rp.ID, rp.LineID, rp.JobStyleID, delta, newCount, anomaly)
	}
}

func toInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int16:
		return int64(n), true
	case int:
		return int64(n), true
	case uint64:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint16:
		return int64(n), true
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	default:
		return 0, false
	}
}
