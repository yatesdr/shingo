package rds

import "encoding/json"

// --- Order requests ---

type SetJoinOrderRequest struct {
	ID               string      `json:"id"`
	ExternalID       string      `json:"externalId,omitempty"`
	FromLoc          string      `json:"fromLoc"`
	ToLoc            string      `json:"toLoc"`
	Vehicle          string      `json:"vehicle,omitempty"`
	Group            string      `json:"group,omitempty"`
	GoodsID          string      `json:"goodsId,omitempty"`
	Priority         int         `json:"priority,omitempty"`
	LoadPostAction   *PostAction `json:"loadPostAction,omitempty"`
	UnloadPostAction *PostAction `json:"unloadPostAction,omitempty"`
}

type SetOrderRequest struct {
	ID         string   `json:"id"`
	ExternalID string   `json:"externalId,omitempty"`
	Vehicle    string   `json:"vehicle,omitempty"`
	Group      string   `json:"group,omitempty"`
	Label      string   `json:"label,omitempty"`
	KeyRoute   []string `json:"keyRoute,omitempty"`
	KeyTask    string   `json:"keyTask,omitempty"`
	Priority   int      `json:"priority,omitempty"`
	Blocks     []Block  `json:"blocks"`
	Complete   bool     `json:"complete"`
}

type Block struct {
	BlockID       string         `json:"blockId"`
	Location      string         `json:"location"`
	Operation     string         `json:"operation,omitempty"`
	OperationArgs map[string]any `json:"operation_args,omitempty"`
	BinTask       string         `json:"binTask,omitempty"`
	GoodsID       string         `json:"goodsId,omitempty"`
	ScriptName    string         `json:"scriptName,omitempty"`
	ScriptArgs    map[string]any `json:"scriptArgs,omitempty"`
	PostAction    *PostAction    `json:"postAction,omitempty"`
}

type PostAction struct {
	ConfigID string `json:"configId,omitempty"`
}

type TerminateRequest struct {
	ID             string   `json:"id,omitempty"`
	IDList         []string `json:"idList,omitempty"`
	Vehicles       []string `json:"vehicles,omitempty"`
	DisableVehicle bool     `json:"disableVehicle"`
	ClearAll       bool     `json:"clearAll,omitempty"`
}

type SetPriorityRequest struct {
	ID       string `json:"id"`
	Priority int    `json:"priority"`
}

type MarkCompleteRequest struct {
	ID string `json:"id"`
}

type SetLabelRequest struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type AddBlocksRequest struct {
	ID       string  `json:"id"`
	Blocks   []Block `json:"blocks"`
	Complete bool    `json:"complete"`
}

// --- Order responses ---

// OrderDetailsResponse handles the Seer RDS response for order detail endpoints.
// RDS returns order fields at the top level (not nested under "data"), so we
// unmarshal the entire JSON into both the Response base and the OrderDetail.
type OrderDetailsResponse struct {
	Response
	Detail OrderDetail
}

func (r *OrderDetailsResponse) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &r.Response); err != nil {
		return err
	}
	return json.Unmarshal(data, &r.Detail)
}

type OrderDetail struct {
	ID            string        `json:"id"`
	ExternalID    string        `json:"externalId"`
	Vehicle       string        `json:"vehicle"`
	Group         string        `json:"group"`
	State         OrderState    `json:"state"`
	Complete      bool          `json:"complete"`
	Priority      int           `json:"priority"`
	CreateTime    int64         `json:"createTime"`
	TerminalTime  int64         `json:"terminalTime"`
	Blocks        []BlockDetail `json:"blocks"`
	Errors        []OrderMessage `json:"errors"`
	Warnings      []OrderMessage `json:"warnings"`
	Notices       []OrderMessage `json:"notices"`
	// Join order fields
	FromLoc       string     `json:"fromLoc,omitempty"`
	ToLoc         string     `json:"toLoc,omitempty"`
	GoodsID       string     `json:"goodsId,omitempty"`
	ContainerName string     `json:"containerName,omitempty"`
	LoadOrderID   string     `json:"loadOrderId,omitempty"`
	LoadState     OrderState `json:"loadState,omitempty"`
	UnloadOrderID string     `json:"unloadOrderId,omitempty"`
	UnloadState   OrderState `json:"unloadState,omitempty"`
}

// OrderMessage represents a structured notice, warning, or error from SEER RDS.
// RDS returns these as objects with code/desc/times/timestamp rather than plain strings.
type OrderMessage struct {
	Code      int    `json:"code"`
	Desc      string `json:"desc"`
	Times     int    `json:"times"`
	Timestamp int64  `json:"timestamp"`
}

// BlockDetailsResponse handles the Seer RDS response for block detail endpoints.
type BlockDetailsResponse struct {
	Response
	Detail BlockDetail
}

func (r *BlockDetailsResponse) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &r.Response); err != nil {
		return err
	}
	return json.Unmarshal(data, &r.Detail)
}

type BlockDetail struct {
	BlockID       string         `json:"blockId"`
	Location      string         `json:"location"`
	State         OrderState     `json:"state"`
	ContainerName string         `json:"containerName"`
	GoodsID       string         `json:"goodsId"`
	Operation     string         `json:"operation"`
	BinTask       string         `json:"binTask"`
	OperationArgs map[string]any `json:"operation_args"`
	ScriptArgs    map[string]any `json:"script_args"`
	ScriptName    string         `json:"script_name"`
}

type OrderListResponse struct {
	Response
	Data []OrderDetail `json:"data,omitempty"`
}
