//go:build sim

package simwarlink

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
	"shingoedge/config"
)

// MachineDowntime tracks the state of one machine's downtime cycle.
// It draws from a seeded PRNG to produce clustered random outages:
// exponential time-between-failures + bounded-random repair duration,
// constrained so MTBF / (MTBF + MTTR) = target availability.
type MachineDowntime struct {
	mu           sync.Mutex
	plcName      string
	availability float64 // target availability (0-1)
	mttrMin      time.Duration
	mttrMax      time.Duration
	rng          *rand.Rand
	clk          clock.Clock // used for After() (sim-scaled waits)
	station      string

	// state
	isDown      bool
	downSince   time.Time
	nextFailure time.Time // when the next failure is scheduled
	eventSeq    int64     // monotonic event counter for dedup

	// callback to force readiness gate off during downtime
	setDown func(plcName string, down bool)

	// callback to emit a downtime event envelope (via outbox)
	emitEvent func(env []byte, subject string)
}

// DowntimeModel manages per-machine downtime cycles for all configured machines.
type DowntimeModel struct {
	mu       sync.Mutex
	machines map[string]*MachineDowntime // plcName → state
}

// NewDowntimeModel creates the downtime model from config. Each machine gets
// its own PRNG derived from the master seed + plcName for reproducibility.
func NewDowntimeModel(cfg config.SimConfig, clk clock.Clock, station string,
	setDown func(plcName string, down bool),
	emitEvent func(env []byte, subject string)) *DowntimeModel {

	if !cfg.Downtime.Enabled || len(cfg.Downtime.Machines) == 0 {
		return nil
	}

	dm := &DowntimeModel{
		machines: make(map[string]*MachineDowntime),
	}

	masterSeed := cfg.Seed
	if masterSeed == 0 {
		masterSeed = int64(clk.Now().UnixNano())
		log.Printf("sim downtime: deriving seed from clock: %d", masterSeed)
	}

	for i, mc := range cfg.Downtime.Machines {
		avail := mc.Availability
		if avail <= 0 || avail >= 1 {
			avail = 0.85
		}
		mttrMin, mttrMax := parseMTTR(mc.MinMTTR, mc.MaxMTTR)

		// Derive per-machine seed: master + index so each machine is
		// independent but deterministic.
		machineRNG := rand.New(rand.NewSource(masterSeed + int64(i)*7919))

		md := &MachineDowntime{
			plcName:      mc.PLCName,
			availability: avail,
			mttrMin:      mttrMin,
			mttrMax:      mttrMax,
			rng:          machineRNG,
			clk:          clk,
			station:      station,
			setDown:      setDown,
			emitEvent:    emitEvent,
		}
		dm.machines[mc.PLCName] = md
	}

	return dm
}

// Start begins the downtime cycles for all machines. Each machine schedules
// its first failure and runs a self-scheduling loop (sleep → fail → repair → repeat).
// Call after the clock is wired so Now() returns the right time.
func (dm *DowntimeModel) Start(ctx context.Context) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	for _, md := range dm.machines {
		md.scheduleNextFailure()
		go md.run(ctx)
	}
}

// IsDown reports whether a machine is currently in a downtime state.
func (dm *DowntimeModel) IsDown(plcName string) bool {
	dm.mu.Lock()
	md, ok := dm.machines[plcName]
	dm.mu.Unlock()
	if !ok {
		return false
	}
	md.mu.Lock()
	defer md.mu.Unlock()
	return md.isDown
}

func (md *MachineDowntime) scheduleNextFailure() {
	// Mean MTTR = (min + max) / 2
	meanMTTR := (md.mttrMin + md.mttrMax) / 2
	// availability = MTBF / (MTBF + MTTR) → MTBF = availability * MTTR / (1 - availability)
	mtbf := time.Duration(float64(meanMTTR) * md.availability / (1.0 - md.availability))

	// Exponential draw for TBF (produces clustering)
	tbf := time.Duration(math.Log(1-md.rng.Float64()) * float64(-mtbf))
	if tbf < time.Minute {
		tbf = time.Minute // floor at 1 minute
	}
	md.nextFailure = clock.Now().Add(tbf)
}

func (md *MachineDowntime) run(ctx context.Context) {
	for {
		// Wait until next failure is due
		now := clock.Now()
		waitDur := md.nextFailure.Sub(now)
		if waitDur > 0 {
			select {
			case <-ctx.Done():
				return
			case <-md.clk.After(waitDur):
			}
		}

		// Go down
		md.mu.Lock()
		md.isDown = true
		md.downSince = clock.Now()
		md.eventSeq++
		seq := md.eventSeq
		station := md.station
		plcName := md.plcName
		downSince := md.downSince
		md.mu.Unlock()

		// Force readiness gate off
		if md.setDown != nil {
			md.setDown(plcName, true)
		}

		// Emit "down" event
		md.emitDownEvent(station, plcName, downSince, seq)

		// Draw repair duration
		mttr := md.drawMTTR()
		repairEnd := clock.Now().Add(mttr)

		// Wait for repair
		select {
		case <-ctx.Done():
			return
		case <-md.clk.After(mttr):
		}

		// Come back up
		md.mu.Lock()
		md.isDown = false
		md.eventSeq++
		endSeq := md.eventSeq
		endedAt := clock.Now()
		durationMS := endedAt.Sub(downSince).Milliseconds()
		md.mu.Unlock()

		// Clear readiness gate
		if md.setDown != nil {
			md.setDown(plcName, false)
		}

		// Emit "up" event
		md.emitUpEvent(station, plcName, downSince, endedAt, durationMS, endSeq)

		// Schedule next failure
		md.mu.Lock()
		md.scheduleNextFailure()
		md.mu.Unlock()

		_ = repairEnd // suppress unused warning
	}
}

func (md *MachineDowntime) drawMTTR() time.Duration {
	// Uniform random between min and max
	delta := md.mttrMax - md.mttrMin
	draw := md.rng.Float64()
	return md.mttrMin + time.Duration(draw*float64(delta))
}

func (md *MachineDowntime) emitDownEvent(station, plcName string, startedAt time.Time, seq int64) {
	snap := &protocol.DowntimeEvent{
		Station:     station,
		PLCName:     plcName,
		Reason:      "breakdown",
		IsDown:      true,
		StartedAt:   startedAt,
		EndedAt:     time.Time{},
		DurationMS:  0,
		EdgeEventID: seq,
	}
	md.sendEnvelope(snap)
}

func (md *MachineDowntime) emitUpEvent(station, plcName string, startedAt, endedAt time.Time, durationMS int64, seq int64) {
	snap := &protocol.DowntimeEvent{
		Station:     station,
		PLCName:     plcName,
		Reason:      "breakdown",
		IsDown:      false,
		StartedAt:   startedAt,
		EndedAt:     endedAt,
		DurationMS:  durationMS,
		EdgeEventID: seq,
	}
	md.sendEnvelope(snap)
}

func (md *MachineDowntime) sendEnvelope(d *protocol.DowntimeEvent) {
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectDowntimeEvent,
		protocol.Address{Role: protocol.RoleEdge, Station: md.station},
		protocol.Address{Role: protocol.RoleCore},
		d,
	)
	if err != nil {
		log.Printf("downtime: build envelope plc=%s: %v", md.plcName, err)
		return
	}
	data, err := env.Encode()
	if err != nil {
		log.Printf("downtime: encode envelope plc=%s: %v", md.plcName, err)
		return
	}
	if md.emitEvent != nil {
		md.emitEvent(data, protocol.SubjectDowntimeEvent)
	}
}

func parseMTTR(minStr, maxStr string) (time.Duration, time.Duration) {
	minMTTR := 5 * time.Minute
	maxMTTR := 30 * time.Minute
	if minStr != "" {
		if d, err := time.ParseDuration(minStr); err == nil {
			minMTTR = d
		}
	}
	if maxStr != "" {
		if d, err := time.ParseDuration(maxStr); err == nil {
			maxMTTR = d
		}
	}
	// Ensure max >= min
	if maxMTTR < minMTTR {
		maxMTTR = minMTTR
	}
	return minMTTR, maxMTTR
}

// FormatDowntimeParams returns a human-readable summary for logging.
func FormatDowntimeParams(cfg config.SimConfig) string {
	if !cfg.Downtime.Enabled {
		return "downtime: disabled"
	}
	mttrMin, mttrMax := 5*time.Minute, 30*time.Minute
	avail := 0.85
	if len(cfg.Downtime.Machines) > 0 {
		avail = cfg.Downtime.Machines[0].Availability
		if avail <= 0 || avail >= 1 {
			avail = 0.85
		}
		mttrMin, mttrMax = parseMTTR(cfg.Downtime.Machines[0].MinMTTR, cfg.Downtime.Machines[0].MaxMTTR)
	}
	meanMTTR := (mttrMin + mttrMax) / 2
	mtbf := time.Duration(float64(meanMTTR) * avail / (1.0 - avail))
	return fmt.Sprintf("downtime: %d machines, availability=%.0f%%, MTBF≈%s, MTTR=%s–%s",
		len(cfg.Downtime.Machines), avail*100, mtbf.Round(time.Minute), mttrMin, mttrMax)
}
