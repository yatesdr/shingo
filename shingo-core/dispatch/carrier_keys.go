package dispatch

import (
	"fmt"
	"log"
	"maps"
	"strings"
)

// ── CARRIER KEYS: THE BINTASK KEY A CARRIER FAMILY'S LIFT AND WAIT SEND ──────
//
// A carrier family can name its own RDS lift key and wait key
// (carrier_families.lift_bintask, wait_bintask, v180), so a plant's RDS can run
// a different action per cart size. The dispatcher holds the families that name
// one in memory, the table-turn cache's precedent: loaded at start
// (ReloadCarrierKeys, from the engine's newDispatcher), by the family save
// (SaveCarrierFamilyKeys) and on the lane floor's 60 s tick, which logs a set
// that moved without the save (a write that went around it). A plant with no
// key on any family holds an empty set, and the block builder then reads
// nothing and sends today's bytes.

// familyKeys is one family's two keys; a blank one sends the default.
type familyKeys struct {
	lift string
	wait string
}

// carrierKeys is the families with a key, by family id. Never nil once
// loaded, and replaced whole, never written in place: a reader holds a set no
// writer changes.
func (d *Dispatcher) carrierKeys() map[int64]familyKeys {
	if m := d.familyKeys.Load(); m != nil {
		return *m
	}
	return nil
}

// ReloadCarrierKeys re-reads the families that name a key. It reports whether
// the set it read differs from the one it held.
func (d *Dispatcher) ReloadCarrierKeys() (bool, error) {
	families, err := d.db.ListCarrierFamilies()
	if err != nil {
		return false, fmt.Errorf("read the carrier families' keys: %w", err)
	}
	next := make(map[int64]familyKeys)
	for _, f := range families {
		if f.LiftBinTask != "" || f.WaitBinTask != "" {
			next[f.ID] = familyKeys{lift: f.LiftBinTask, wait: f.WaitBinTask}
		}
	}
	prev := d.familyKeys.Swap(&next)
	return prev == nil || !maps.Equal(*prev, next), nil
}

// reloadCarrierKeysOnFloor is the floor's reload: a set that moved since the
// last load or save was written around the save, and says so.
func (d *Dispatcher) reloadCarrierKeysOnFloor() {
	held := d.familyKeys.Load() != nil
	changed, err := d.ReloadCarrierKeys()
	if err != nil {
		log.Printf("dispatch: carrier keys floor: %v (the keys held stay in force; the next pass retries)", err)
		return
	}
	if held && changed {
		log.Printf("dispatch: carrier keys: the families' keys changed without the Bins page's save — now %d keyed famil(ies)",
			len(d.carrierKeys()))
	}
}

// SaveCarrierFamilyKeys is the one door that sets a family's keys: it writes
// them and reloads the set, so the next block built sends them. A blank key is
// the default. Every type of the family sends what is saved here.
func (d *Dispatcher) SaveCarrierFamilyKeys(familyID int64, lift, wait string) error {
	lift, wait = strings.TrimSpace(lift), strings.TrimSpace(wait)
	if err := d.db.SetCarrierFamilyKeys(familyID, lift, wait); err != nil {
		return err
	}
	if _, err := d.ReloadCarrierKeys(); err != nil {
		log.Printf("dispatch: carrier keys: family %d saved, but the keys could not be re-read: %v (the floor reloads them within a minute)",
			familyID, err)
	}
	return nil
}
