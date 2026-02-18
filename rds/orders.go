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
	return resp.Data, nil
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
