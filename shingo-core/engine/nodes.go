package engine

import (
	"shingocore/fleet"
)

// OccupancyEntry represents a single node's fleet vs ShinGo occupancy comparison.
type OccupancyEntry struct {
	LocationID    string `json:"location_id"`
	NodeName      string `json:"node_name"`
	FleetOccupied *bool  `json:"fleet_occupied"`
	InShinGo      bool   `json:"in_shingo"`
	Discrepancy   string `json:"discrepancy"`
}

// GetNodeOccupancy compares fleet bin occupancy against ShinGo node records.
func (e *Engine) GetNodeOccupancy() ([]OccupancyEntry, error) {
	np, ok := e.fleet.(fleet.NodeOccupancyProvider)
	if !ok {
		return nil, errFleetUnsupported("occupancy status")
	}
	locations, err := np.GetNodeOccupancy()
	if err != nil {
		return nil, err
	}

	nodes, err := e.db.ListNodes()
	if err != nil {
		return nil, err
	}

	locMap := make(map[string]bool, len(locations))
	for _, loc := range locations {
		locMap[loc.ID] = loc.Occupied
	}

	nodeByName := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if n.Name != "" {
			nodeByName[n.Name] = n.Name
		}
	}

	var results []OccupancyEntry

	for _, loc := range locations {
		e := OccupancyEntry{
			LocationID:    loc.ID,
			FleetOccupied: &loc.Occupied,
			InShinGo:      nodeByName[loc.ID] != "",
			NodeName:      nodeByName[loc.ID],
		}
		if !e.InShinGo {
			e.Discrepancy = "fleet_only"
		}
		results = append(results, e)
	}

	for _, n := range nodes {
		if n.Name == "" {
			continue
		}
		if _, ok := locMap[n.Name]; !ok {
			results = append(results, OccupancyEntry{
				LocationID:  n.Name,
				NodeName:    n.Name,
				InShinGo:    true,
				Discrepancy: "shingo_only",
			})
		}
	}

	return results, nil
}

type fleetUnsupportedError struct {
	feature string
}

func (e *fleetUnsupportedError) Error() string {
	return "fleet backend does not support " + e.feature
}

func errFleetUnsupported(feature string) error {
	return &fleetUnsupportedError{feature: feature}
}

// IsFleetUnsupported returns true if the error indicates the fleet backend
// does not support the requested feature.
func IsFleetUnsupported(err error) bool {
	_, ok := err.(*fleetUnsupportedError)
	return ok
}
