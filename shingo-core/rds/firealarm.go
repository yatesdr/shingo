package rds

import (
	"shingocore/fleet"
)

// fireAlarmResponse is the raw RDS response from GET /isFire.
type fireAlarmResponse struct {
	Response         // code, msg, create_on
	IsFire bool `json:"is_fire"`
}

// fireAlarmRequest is the body for POST /fireOperations.
type fireAlarmRequest struct {
	On         bool `json:"on"`
	AutoResume bool `json:"autoResume,omitempty"`
}

// GetFireAlarmStatus queries the current fire alarm state.
// Satisfies fleet.FireAlarmController.
func (c *Client) GetFireAlarmStatus() (*fleet.FireAlarmStatus, error) {
	var resp fireAlarmResponse
	if err := c.get("/isFire", &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}

	// Suppress epoch-zero timestamps — RDS may return these when
	// the fire alarm has never been triggered.
	changedAt := resp.CreateOn
	if changedAt == "" || changedAt == "1970-01-01T00:00:00Z" {
		changedAt = ""
	}

	return &fleet.FireAlarmStatus{
		IsFire:    resp.IsFire,
		ChangedAt: changedAt,
	}, nil
}

// SetFireAlarm triggers or clears the fire alarm.
// Satisfies fleet.FireAlarmController.
func (c *Client) SetFireAlarm(on bool, autoResume bool) error {
	var resp Response
	if err := c.post("/fireOperations", &fireAlarmRequest{
		On:         on,
		AutoResume: autoResume,
	}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}
