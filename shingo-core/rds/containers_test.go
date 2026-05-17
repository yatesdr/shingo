package rds

import (
	"encoding/json"
	"net/http"
	"shingo/protocol/testutil"
	"testing"
)

func TestBindContainerGoods(t *testing.T) {
	t.Parallel()
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/setContainerGoods" {
			t.Errorf("path = %q, want /setContainerGoods", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req BindGoodsRequest
		testutil.MustNoErr(t, json.NewDecoder(r.Body).Decode(&req), "decode")
		if req.Vehicle != "AMB-01" {
			t.Errorf("Vehicle = %q, want AMB-01", req.Vehicle)
		}
		if req.ContainerName != "C1" {
			t.Errorf("ContainerName = %q, want C1", req.ContainerName)
		}
		if req.GoodsID != "G-99" {
			t.Errorf("GoodsID = %q, want G-99", req.GoodsID)
		}
		json.NewEncoder(w).Encode(Response{Code: 0, Msg: "ok"})
	})
	defer srv.Close()

	err := client.BindContainerGoods(&BindGoodsRequest{
		Vehicle:       "AMB-01",
		ContainerName: "C1",
		GoodsID:       "G-99",
	})
	if err != nil {
		t.Fatalf("BindContainerGoods: %v", err)
	}
}

func TestBindContainerGoods_Error(t *testing.T) {
	t.Parallel()
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 7, Msg: "container in use"})
	})
	defer srv.Close()

	err := client.BindContainerGoods(&BindGoodsRequest{Vehicle: "AMB-01"})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestUnbindGoods(t *testing.T) {
	t.Parallel()
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clearGoods" {
			t.Errorf("path = %q, want /clearGoods", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req UnbindGoodsRequest
		testutil.MustNoErr(t, json.NewDecoder(r.Body).Decode(&req), "decode")
		if req.Vehicle != "AMB-02" {
			t.Errorf("Vehicle = %q, want AMB-02", req.Vehicle)
		}
		if req.GoodsID != "G-1" {
			t.Errorf("GoodsID = %q, want G-1", req.GoodsID)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	testutil.MustNoErr(t, client.UnbindGoods("AMB-02", "G-1"), "UnbindGoods")
}

func TestUnbindGoods_ServerError(t *testing.T) {
	t.Parallel()
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()

	if err := client.UnbindGoods("AMB-02", "G-1"); err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestUnbindContainerGoods(t *testing.T) {
	t.Parallel()
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clearContainer" {
			t.Errorf("path = %q, want /clearContainer", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req UnbindContainerRequest
		testutil.MustNoErr(t, json.NewDecoder(r.Body).Decode(&req), "decode")
		if req.Vehicle != "AMB-03" {
			t.Errorf("Vehicle = %q, want AMB-03", req.Vehicle)
		}
		if req.ContainerName != "shelf-2" {
			t.Errorf("ContainerName = %q, want shelf-2", req.ContainerName)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	testutil.MustNoErr(t, client.UnbindContainerGoods("AMB-03", "shelf-2"), "UnbindContainerGoods")
}

func TestUnbindContainerGoods_Error(t *testing.T) {
	t.Parallel()
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 9, Msg: "no such container"})
	})
	defer srv.Close()

	err := client.UnbindContainerGoods("AMB-03", "missing")
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestClearAllContainerGoods(t *testing.T) {
	t.Parallel()
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clearAllContainersGoods" {
			t.Errorf("path = %q, want /clearAllContainersGoods", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req ClearAllGoodsRequest
		testutil.MustNoErr(t, json.NewDecoder(r.Body).Decode(&req), "decode")
		if req.Vehicle != "AMB-04" {
			t.Errorf("Vehicle = %q, want AMB-04", req.Vehicle)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	testutil.MustNoErr(t, client.ClearAllContainerGoods("AMB-04"), "ClearAllContainerGoods")
}

func TestClearAllContainerGoods_Disconnect(t *testing.T) {
	t.Parallel()
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {})
	srv.Close() // Close before invoking — POST should fail with connection error.

	if err := client.ClearAllContainerGoods("AMB-04"); err == nil {
		t.Fatal("expected error when server is unreachable")
	}
}
