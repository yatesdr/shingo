package rds

import "encoding/json"

// --- Simulation types ---

type SimStateTemplateResponse struct {
	Response
	Data json.RawMessage `json:"data,omitempty"`
}
