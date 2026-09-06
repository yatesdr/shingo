package store

import "fmt"

// RecordPLCCATIDObservation records that a press's PLC declared this part
// identity, and when. One row per (process, cat id), with a first-seen, a
// last-seen and a count.
//
// NOBODY HAS EVER BEEN ABLE TO SAY WHAT A PLC ACTUALLY SENDS, and every
// argument about the wrong-part guard has had to work around that. The 144
// values in Core's payload_manifest were the only persisted cat ids anywhere,
// and they were typed by people, not read off a line. A live reading was
// consulted, compared, logged and dropped — the mismatch path stored one on
// process_changeovers.verify_live_catid, but only when the reading DISAGREED,
// so the record was of exceptions and never of the norm.
//
// This is that record. It stores every CONFIRMED value — post-debounce, so a
// flicker on the wire does not become a fact — including the ones that match,
// which are the ones that certify the configuration is right.
//
// IT DECIDES NOTHING. No guard, prompt or arm reads this table; it is evidence
// for a person, so a wrong row cannot cause a wrong action. That is deliberate:
// the first pass of any measurement is the one most likely to be wrong about
// what it is measuring.
func (db *DB) RecordPLCCATIDObservation(processID int64, plcName, catid string) error {
	if processID <= 0 || catid == "" {
		return nil
	}
	_, err := db.Exec(`INSERT INTO plc_catid_observations (process_id, plc_name, catid)
		VALUES (?, ?, ?)
		ON CONFLICT (process_id, catid) DO UPDATE SET
			plc_name     = excluded.plc_name,
			last_seen    = datetime('now'),
			observations = plc_catid_observations.observations + 1`,
		processID, plcName, catid)
	if err != nil {
		return fmt.Errorf("record plc catid observation (process=%d catid=%q): %w", processID, catid, err)
	}
	return nil
}

// PLCCATIDObservation is one (process, cat id) the plant has actually seen.
type PLCCATIDObservation struct {
	ProcessID    int64  `json:"process_id"`
	PLCName      string `json:"plc_name"`
	CATID        string `json:"catid"`
	FirstSeen    string `json:"first_seen"`
	LastSeen     string `json:"last_seen"`
	Observations int64  `json:"observations"`
}

// ListPLCCATIDObservations returns every observation, most recently seen first.
func (db *DB) ListPLCCATIDObservations() ([]PLCCATIDObservation, error) {
	rows, err := db.Query(`SELECT process_id, plc_name, catid, first_seen, last_seen, observations
		FROM plc_catid_observations ORDER BY last_seen DESC, process_id, catid`)
	if err != nil {
		return nil, fmt.Errorf("list plc catid observations: %w", err)
	}
	defer rows.Close()
	var out []PLCCATIDObservation
	for rows.Next() {
		var o PLCCATIDObservation
		if err := rows.Scan(&o.ProcessID, &o.PLCName, &o.CATID, &o.FirstSeen, &o.LastSeen, &o.Observations); err != nil {
			return nil, fmt.Errorf("scan plc catid observation: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
