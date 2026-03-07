package nodestate

type NodeState struct {
	NodeID    int64
	NodeName  string
	Zone      string
	Enabled   bool
	Items     []PayloadItem
	ItemCount int
}

type PayloadItem struct {
	ID                int64  `json:"id"`
	BlueprintID       int64  `json:"blueprint_id"`
	BlueprintCode     string `json:"blueprint_code"`
	BinID             *int64 `json:"bin_id,omitempty"`
	BinLabel          string `json:"bin_label"`
	ManifestConfirmed bool   `json:"manifest_confirmed"`
	Notes             string `json:"notes,omitempty"`
	ClaimedBy         *int64 `json:"claimed_by,omitempty"`
}
