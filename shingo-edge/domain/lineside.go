package domain

import "time"

// LinesideBucket is one row in the lineside_buckets table — a tracked
// quantity of a particular part number staged at a process Node for
// the active or a previous Style. ActiveLineside vs InactiveLineside
// is determined by State + matching Style ID against the Node's
// active claim.
//
// Renamed from `lineside.Bucket` during Stage 2A.2 lift to make the
// type self-describing once outside the lineside sub-package.
type LinesideBucket struct {
	ID         int64     `json:"id"`
	NodeID     int64     `json:"node_id"`
	PairKey    string    `json:"pair_key"`
	StyleID    int64     `json:"style_id"`
	PartNumber string    `json:"part_number"`
	Qty        int       `json:"qty"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}
