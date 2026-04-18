package domain

import "time"

// BinType is the lookup entity that classifies physical bins by their
// outer dimensions. Bins point at a BinType via Bin.BinTypeID; the
// BinTypeCode copy on Bin is the most common joined field, so most
// rendering paths don't need to follow the pointer.
type BinType struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	Description string    `json:"description"`
	WidthIn     float64   `json:"width_in"`
	HeightIn    float64   `json:"height_in"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
