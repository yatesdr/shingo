package rds

// --- Bin/location types ---

type BinDetailsResponse struct {
	Response
	Data []BinDetail `json:"data,omitempty"`
}

type BinDetail struct {
	ID     string `json:"id"`
	Filled bool   `json:"filled"`
	Holder int    `json:"holder"`
	Status int    `json:"status"`
}

type BinCheckRequest struct {
	Bins []string `json:"bins"`
}

type BinCheckResponse struct {
	Response
	Bins []BinCheckResult `json:"bins,omitempty"`
}

type BinCheckResult struct {
	ID     string          `json:"id"`
	Exist  bool            `json:"exist"`
	Valid  bool            `json:"valid"`
	Status *BinPointStatus `json:"status,omitempty"`
}

type BinPointStatus struct {
	PointName string `json:"point_name"`
}
