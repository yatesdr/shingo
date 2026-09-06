package domain

import "time"

// LinesideBucket is one row in the node_lineside_bucket table — a tracked
// quantity of a particular PAYLOAD staged at a process Node for the active or a
// previous Style. ActiveLineside vs InactiveLineside is determined by State +
// matching Style ID against the Node's active claim.
//
// THE IDENTIFIER IS A PAYLOAD CODE and the column said part_number until it
// said cat_id, neither of which it ever held. Every writer feeds a payload
// code: the release modal's chips are the claim's allowed payload codes, the
// consume tick drains by payload, and ListLinesideLevels joins this straight
// against process_node_runtime_states.lineside_payload_code. Both old names
// invited a reader to expect something else.
//
// Renamed from `lineside.Bucket` during Stage 2A.2 lift to make the
// type self-describing once outside the lineside sub-package.
type LinesideBucket struct {
	ID          int64     `json:"id"`
	NodeID      int64     `json:"node_id"`
	PairKey     string    `json:"pair_key"`
	StyleID     int64     `json:"style_id"`
	PayloadCode string    `json:"payload_code"`
	Qty         int       `json:"qty"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
