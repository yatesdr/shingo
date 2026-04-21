package rds

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCreateOrder(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/setOrder" {
			t.Errorf("path = %q, want /setOrder", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req SetOrderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.ID != "ord-1" {
			t.Errorf("ID = %q, want ord-1", req.ID)
		}
		if !req.Complete {
			t.Errorf("Complete = false, want true")
		}
		if len(req.Blocks) != 2 {
			t.Errorf("len(Blocks) = %d, want 2", len(req.Blocks))
		}
		if req.Blocks[0].BlockID != "blk-1" || req.Blocks[0].Location != "Loc-A" {
			t.Errorf("Blocks[0] = %+v, want blk-1@Loc-A", req.Blocks[0])
		}
		if req.Blocks[1].Operation != "load" {
			t.Errorf("Blocks[1].Operation = %q, want load", req.Blocks[1].Operation)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.CreateOrder(&SetOrderRequest{
		ID:       "ord-1",
		Complete: true,
		Blocks: []Block{
			{BlockID: "blk-1", Location: "Loc-A"},
			{BlockID: "blk-2", Location: "Loc-B", Operation: "load"},
		},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
}

func TestCreateOrder_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 11, Msg: "duplicate id"})
	})
	defer srv.Close()

	err := client.CreateOrder(&SetOrderRequest{ID: "dup"})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestMarkComplete(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markComplete" {
			t.Errorf("path = %q, want /markComplete", r.URL.Path)
		}
		var req MarkCompleteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.ID != "ord-99" {
			t.Errorf("ID = %q, want ord-99", req.ID)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	if err := client.MarkComplete(&MarkCompleteRequest{ID: "ord-99"}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
}

func TestMarkComplete_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "not found"})
	})
	defer srv.Close()

	if err := client.MarkComplete(&MarkCompleteRequest{ID: "x"}); err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestGetOrderByExternalID(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orderDetailsByExternalId/EXT-77" {
			t.Errorf("path = %q, want /orderDetailsByExternalId/EXT-77", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		w.Write([]byte(`{"code":0,"msg":"ok","id":"ord-77","externalId":"EXT-77","state":"RUNNING","vehicle":"AMB-09"}`))
	})
	defer srv.Close()

	detail, err := client.GetOrderByExternalID("EXT-77")
	if err != nil {
		t.Fatalf("GetOrderByExternalID: %v", err)
	}
	if detail.ID != "ord-77" {
		t.Errorf("ID = %q, want ord-77", detail.ID)
	}
	if detail.ExternalID != "EXT-77" {
		t.Errorf("ExternalID = %q, want EXT-77", detail.ExternalID)
	}
	if detail.State != StateRunning {
		t.Errorf("State = %q, want RUNNING", detail.State)
	}
	if detail.Vehicle != "AMB-09" {
		t.Errorf("Vehicle = %q, want AMB-09", detail.Vehicle)
	}
}

func TestGetOrderByExternalID_EmptyID(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		// Code=0 but no order id present — orders.go treats this as an error.
		w.Write([]byte(`{"code":0,"msg":"ok"}`))
	})
	defer srv.Close()

	_, err := client.GetOrderByExternalID("missing")
	if err == nil {
		t.Fatal("expected error when response has empty id")
	}
}

func TestGetOrderByExternalID_NotFound(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":1,"msg":"not found"}`))
	})
	defer srv.Close()

	_, err := client.GetOrderByExternalID("missing")
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestGetOrderByBlockID(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orderDetailsByBlockId/blk-42" {
			t.Errorf("path = %q, want /orderDetailsByBlockId/blk-42", r.URL.Path)
		}
		w.Write([]byte(`{"code":0,"msg":"ok","id":"ord-42","state":"FINISHED"}`))
	})
	defer srv.Close()

	detail, err := client.GetOrderByBlockID("blk-42")
	if err != nil {
		t.Fatalf("GetOrderByBlockID: %v", err)
	}
	if detail.ID != "ord-42" {
		t.Errorf("ID = %q, want ord-42", detail.ID)
	}
	if detail.State != StateFinished {
		t.Errorf("State = %q, want FINISHED", detail.State)
	}
}

func TestGetOrderByBlockID_EmptyID(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"ok"}`))
	})
	defer srv.Close()

	_, err := client.GetOrderByBlockID("missing")
	if err == nil {
		t.Fatal("expected error when response has empty id")
	}
}

func TestSetLabel(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/setLabel" {
			t.Errorf("path = %q, want /setLabel", r.URL.Path)
		}
		var req SetLabelRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.ID != "ord-1" {
			t.Errorf("ID = %q, want ord-1", req.ID)
		}
		if req.Label != "rush" {
			t.Errorf("Label = %q, want rush", req.Label)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	if err := client.SetLabel("ord-1", "rush"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
}

func TestSetLabel_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "fail"})
	})
	defer srv.Close()

	if err := client.SetLabel("ord-1", "rush"); err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestAddBlocks_NoVehicle(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/addBlocks" {
			t.Errorf("path = %q, want /addBlocks", r.URL.Path)
		}
		var req AddBlocksRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.ID != "ord-7" {
			t.Errorf("ID = %q, want ord-7", req.ID)
		}
		if req.Vehicle != "" {
			t.Errorf("Vehicle = %q, want empty", req.Vehicle)
		}
		if !req.Complete {
			t.Errorf("Complete = false, want true")
		}
		if len(req.Blocks) != 1 || req.Blocks[0].BlockID != "blk-1" {
			t.Errorf("Blocks = %+v, want [blk-1]", req.Blocks)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.AddBlocks("ord-7", []Block{{BlockID: "blk-1", Location: "L1"}}, true)
	if err != nil {
		t.Fatalf("AddBlocks: %v", err)
	}
}

func TestAddBlocks_WithVehicle(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		var req AddBlocksRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Vehicle != "AMB-05" {
			t.Errorf("Vehicle = %q, want AMB-05", req.Vehicle)
		}
		if req.Complete {
			t.Errorf("Complete = true, want false")
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.AddBlocks("ord-7", []Block{{BlockID: "blk-2"}}, false, "AMB-05")
	if err != nil {
		t.Fatalf("AddBlocks: %v", err)
	}
}

func TestAddBlocks_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 13, Msg: "order is complete"})
	})
	defer srv.Close()

	err := client.AddBlocks("ord-done", []Block{{BlockID: "x"}}, false)
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestGetBlockDetails(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blockDetailsById/blk-7" {
			t.Errorf("path = %q, want /blockDetailsById/blk-7", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		w.Write([]byte(`{"code":0,"msg":"ok","blockId":"blk-7","location":"Loc-X","state":"RUNNING","operation":"unload","goodsId":"G-12"}`))
	})
	defer srv.Close()

	detail, err := client.GetBlockDetails("blk-7")
	if err != nil {
		t.Fatalf("GetBlockDetails: %v", err)
	}
	if detail.BlockID != "blk-7" {
		t.Errorf("BlockID = %q, want blk-7", detail.BlockID)
	}
	if detail.Location != "Loc-X" {
		t.Errorf("Location = %q, want Loc-X", detail.Location)
	}
	if detail.State != StateRunning {
		t.Errorf("State = %q, want RUNNING", detail.State)
	}
	if detail.Operation != "unload" {
		t.Errorf("Operation = %q, want unload", detail.Operation)
	}
	if detail.GoodsID != "G-12" {
		t.Errorf("GoodsID = %q, want G-12", detail.GoodsID)
	}
}

func TestGetBlockDetails_EmptyID(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		// code=0 but no blockId — orders.go treats as an error.
		w.Write([]byte(`{"code":0,"msg":"ok"}`))
	})
	defer srv.Close()

	_, err := client.GetBlockDetails("missing")
	if err == nil {
		t.Fatal("expected error when response has empty blockId")
	}
}

func TestGetBlockDetails_NotFound(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":1,"msg":"not found"}`))
	})
	defer srv.Close()

	_, err := client.GetBlockDetails("missing")
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}
