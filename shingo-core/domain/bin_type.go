package domain

import "time"

// BinType is the lookup entity that classifies physical bins by their
// outer dimensions. Bins point at a BinType via Bin.BinTypeID; the
// BinTypeCode copy on Bin is the most common joined field, so most
// rendering paths don't need to follow the pointer.
//
// The three dimensions are DESCRIPTIVE. Nothing in the system decides whether a
// carrier fits a slot by comparing them — fit is type identity everywhere, and
// LengthIn arriving (v90) does not change that. They exist so a plant's carrier
// catalogue can be read off the schema instead of parsed out of codes like
// "45x58x32", which is where the third number lived until now.
type BinType struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	Description string    `json:"description"`
	WidthIn     float64   `json:"width_in"`
	HeightIn    float64   `json:"height_in"`
	LengthIn    float64   `json:"length_in"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
