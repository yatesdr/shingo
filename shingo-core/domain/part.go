package domain

import "time"

// Part is one physical part, under both of the names it is known by.
//
// TWO SYSTEMS NAME THE SAME THING. PartNumber is the business identity — what
// CMS books against and what a person calls it. CATID is what a cell's PLC
// declares it is running, and the wrong-part-on-press guard compares the live
// reading against the set of cat ids a style's payloads resolve to.
//
// THEY LIVE ON ONE ROW BECAUSE THE CAT ID FOLLOWS THE PART (owner ruling,
// 2026-09-06). It is not a property of the cell: a consume cell combining four
// parts mints a new cat id for the part it outputs, and the four inputs each
// carry their own, made at their own sub-cells. Keeping them apart is what let
// a cat id be typed into a column named part_number for two years — the two
// tables' columns each described what the other held.
//
// CATID MAY BE EMPTY and that is an honest state: a part nobody has told us the
// controls identity of. An empty one contributes nothing to a style's derived
// set, so the guard reasons over the subset it actually knows rather than
// guessing. styles.expected_catid remains the human OVERRIDE pin — when a style
// carries one, it IS the set, and nothing garbage-collects it.
type Part struct {
	ID          int64     `json:"id"`
	PartNumber  string    `json:"part_number"`
	CATID       string    `json:"catid"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
