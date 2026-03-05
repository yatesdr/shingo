package rds

// --- System types ---

type GetProfilesRequest struct {
	File string `json:"file"`
}

type LicenseResponse struct {
	Response
	Data *LicenseInfo `json:"data,omitempty"`
}

type LicenseInfo struct {
	MaxRobots int              `json:"maxRobots"`
	Expiry    string           `json:"expiry"`
	Features  []LicenseFeature `json:"features,omitempty"`
}

type LicenseFeature struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}
