package domain

// Shift is one named shift definition (1st, 2nd, 3rd) used by the
// hourly-counts reporting and the production schedule. StartTime /
// EndTime are stored as wall-clock strings ("08:00" / "16:00") since
// they're independent of date.
type Shift struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	ShiftNumber int    `json:"shift_number"`
	StartTime   string `json:"start_time"`
	EndTime     string `json:"end_time"`
}
