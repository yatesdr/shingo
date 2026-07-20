package rds

import "encoding/json"

// --- Bin/location types ---

type BinDetailsResponse struct {
	Response
	Data []BinDetail `json:"data,omitempty"`
}

type BinDetail struct {
	ID     string `json:"id"`
	Filled bool   `json:"filled"`
	Holder int    `json:"holder"`
	Status int    `json:"status"`
}

type BinCheckRequest struct {
	Bins []string `json:"bins"`
}

type BinCheckResponse struct {
	Response
	Bins []BinCheckResult `json:"bins,omitempty"`
}

type BinCheckResult struct {
	ID     string          `json:"id"`
	Exist  bool            `json:"exist"`
	Valid  bool            `json:"valid"`
	Status *BinPointStatus `json:"status,omitempty"`
}

type BinPointStatus struct {
	PointName string `json:"point_name"`
	// BinTask is the list of binTask actions configured at this storage location,
	// per the /binCheck response (RDSCore HTTP API manual). Each element is a
	// single-key object whose KEY is the binTask name (e.g. "ForkLoad", "load",
	// "Spin_90") and whose value is that task's RDS-side parameter map. The params
	// live in RDS; the wire (setOrder) carries only the name. We retain the raw
	// values (unparsed) and expose the key set via TaskNames — F4c advanced-load-
	// sequence validation needs only the names, matched per location.
	BinTask []map[string]json.RawMessage `json:"binTask,omitempty"`
}

// TaskNames returns the binTask action names configured at this location, in
// response order. Each BinTask entry is a single-key object keyed by the task
// name; this flattens those keys. Safe on a nil receiver (returns nil).
func (s *BinPointStatus) TaskNames() []string {
	if s == nil {
		return nil
	}
	var names []string
	for _, entry := range s.BinTask {
		for name := range entry {
			names = append(names, name)
		}
	}
	return names
}
