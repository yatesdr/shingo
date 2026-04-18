package domain

import "time"

// NodeType classifies nodes — e.g. STOR (storage slot), LANE (lane),
// NGRP (node group), EDGE (edge station). The Code is the three- or
// four-letter identifier the node-graph logic branches on in dispatch
// and binresolver; IsSynthetic marks types that don't correspond to a
// physical location and exist only for grouping / routing.
type NodeType struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IsSynthetic bool      `json:"is_synthetic"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
