package rds

// --- Container types ---

type BindGoodsRequest struct {
	Vehicle       string `json:"vehicle"`
	ContainerName string `json:"containerName"`
	GoodsID       string `json:"goodsId"`
}

type UnbindGoodsRequest struct {
	Vehicle string `json:"vehicle"`
	GoodsID string `json:"goodsId"`
}

type UnbindContainerRequest struct {
	Vehicle       string `json:"vehicle"`
	ContainerName string `json:"containerName"`
}

type ClearAllGoodsRequest struct {
	Vehicle string `json:"vehicle"`
}
