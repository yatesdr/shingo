package rds

import "fmt"

// CreateJoinOrder creates a pickup-to-delivery join order.
func (c *Client) CreateJoinOrder(req *SetJoinOrderRequest) error {
	var resp Response
	if err := c.post("/setOrder", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// CreateOrder creates a multi-block order.
func (c *Client) CreateOrder(req *SetOrderRequest) error {
	var resp Response
	if err := c.post("/setOrder", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// TerminateOrder terminates one or more RDS orders.
func (c *Client) TerminateOrder(req *TerminateRequest) error {
	var resp Response
	if err := c.post("/terminate", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// GetOrderDetails retrieves details for a single order by ID.
func (c *Client) GetOrderDetails(id string) (*OrderDetail, error) {
	var resp OrderDetailsResponse
	if err := c.get(fmt.Sprintf("/orderDetails/%s", id), &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	if resp.Detail.ID == "" {
		c.dbg("!! orderDetails/%s returned code=0 but no order ID in response", id)
		return nil, fmt.Errorf("order %s: empty response data", id)
	}
	return &resp.Detail, nil
}

// ListOrders retrieves a paged list of orders.
func (c *Client) ListOrders(page, size int) ([]OrderDetail, error) {
	path := fmt.Sprintf("/orders?page=%d&size=%d", page, size)
	var resp OrderListResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// SetPriority changes the priority of a pending order.
func (c *Client) SetPriority(id string, priority int) error {
	var resp Response
	if err := c.post("/setPriority", &SetPriorityRequest{ID: id, Priority: priority}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// MarkComplete marks an incremental order as complete (no more blocks will be added).
func (c *Client) MarkComplete(req *MarkCompleteRequest) error {
	var resp Response
	if err := c.post("/markComplete", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// GetOrderByExternalID retrieves order details by external ID.
func (c *Client) GetOrderByExternalID(externalID string) (*OrderDetail, error) {
	var resp OrderDetailsResponse
	if err := c.get(fmt.Sprintf("/orderDetailsByExternalId/%s", externalID), &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	if resp.Detail.ID == "" {
		c.dbg("!! orderDetailsByExternalId/%s returned code=0 but no order ID in response", externalID)
		return nil, fmt.Errorf("order by externalId %s: empty response data", externalID)
	}
	return &resp.Detail, nil
}

// GetOrderByBlockID retrieves the parent order containing a specific block.
func (c *Client) GetOrderByBlockID(blockID string) (*OrderDetail, error) {
	var resp OrderDetailsResponse
	if err := c.get(fmt.Sprintf("/orderDetailsByBlockId/%s", blockID), &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	if resp.Detail.ID == "" {
		c.dbg("!! orderDetailsByBlockId/%s returned code=0 but no order ID in response", blockID)
		return nil, fmt.Errorf("order by blockId %s: empty response data", blockID)
	}
	return &resp.Detail, nil
}

// SetLabel sets a dispatch-filtering label on an order.
func (c *Client) SetLabel(id, label string) error {
	var resp Response
	if err := c.post("/setLabel", &SetLabelRequest{ID: id, Label: label}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// AddBlocks appends blocks to an existing incremental order.
func (c *Client) AddBlocks(id string, blocks []Block, complete bool) error {
	var resp Response
	if err := c.post("/addBlocks", &AddBlocksRequest{ID: id, Blocks: blocks, Complete: complete}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// GetBlockDetails retrieves details for a specific block by its ID.
func (c *Client) GetBlockDetails(blockID string) (*BlockDetail, error) {
	var resp BlockDetailsResponse
	if err := c.get(fmt.Sprintf("/blockDetailsById/%s", blockID), &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	if resp.Detail.BlockID == "" {
		c.dbg("!! blockDetailsById/%s returned code=0 but no blockId in response", blockID)
		return nil, fmt.Errorf("block %s: empty response data", blockID)
	}
	return &resp.Detail, nil
}
