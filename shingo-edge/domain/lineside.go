package domain

import "time"

// LinesideBucket is one row in the node_lineside_bucket table: a pile of one
// PAYLOAD that an operator pulled from a bin to the bench at a process Node.
// Its identity is (node, payload, state), nothing else.
//
//   - active:   on-hand from the pull until the node's cutover. Consume ticks
//     drain it before the bin, and Core counts it.
//   - stranded: what an active pile had left at a cutover (every active-style
//     flip on the process). A permanent count-anomaly record: it never drains,
//     never counts on either side, and never revives.
//
// THE IDENTIFIER IS A PAYLOAD CODE and the column said part_number until it
// said cat_id, neither of which it ever held. Every writer feeds a payload
// code: the release modal's chips are the claim's allowed payload codes, the
// consume tick drains by payload, and ListLinesideLevels joins this straight
// against process_node_runtime_states.lineside_payload_code.
type LinesideBucket struct {
	ID          int64     `json:"id"`
	NodeID      int64     `json:"node_id"`
	PayloadCode string    `json:"payload_code"`
	Qty         int       `json:"qty"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
