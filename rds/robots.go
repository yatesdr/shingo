package rds

// GetRobotsStatus retrieves status for all robots.
func (c *Client) GetRobotsStatus() ([]RobotStatus, error) {
	var resp RobotsStatusResponse
	if err := c.get("/robotsStatus", &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	return resp.Report, nil
}

// SetDispatchable sets dispatchability for robots.
func (c *Client) SetDispatchable(req *DispatchableRequest) error {
	var resp Response
	if err := c.post("/dispatchable", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// RedoFailed retries the current failed block for given robots.
func (c *Client) RedoFailed(req *RedoFailedRequest) error {
	var resp Response
	if err := c.post("/redoFailedOrder", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// ManualFinish marks the current block as manually finished for given robots.
func (c *Client) ManualFinish(req *ManualFinishRequest) error {
	var resp Response
	if err := c.post("/manualFinished", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}
