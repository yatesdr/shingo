package rds

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestGetBinDetails_NoFilter(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/binDetails" {
			t.Errorf("path = %q, want /binDetails", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if got := r.URL.RawQuery; got != "" {
			t.Errorf("rawQuery = %q, want empty", got)
		}
		json.NewEncoder(w).Encode(BinDetailsResponse{
			Response: Response{Code: 0, Msg: "ok"},
			Data: []BinDetail{
				{ID: "BIN-1", Filled: true, Holder: 2, Status: 1},
				{ID: "BIN-2", Filled: false, Holder: 0, Status: 0},
			},
		})
	})
	defer srv.Close()

	bins, err := client.GetBinDetails()
	if err != nil {
		t.Fatalf("GetBinDetails: %v", err)
	}
	if len(bins) != 2 {
		t.Fatalf("len(bins) = %d, want 2", len(bins))
	}
	if bins[0].ID != "BIN-1" {
		t.Errorf("bins[0].ID = %q, want %q", bins[0].ID, "BIN-1")
	}
	if !bins[0].Filled {
		t.Errorf("bins[0].Filled = false, want true")
	}
	if bins[0].Holder != 2 {
		t.Errorf("bins[0].Holder = %d, want 2", bins[0].Holder)
	}
	if bins[1].Filled {
		t.Errorf("bins[1].Filled = true, want false")
	}
}

func TestGetBinDetails_WithGroups(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/binDetails" {
			t.Errorf("path = %q, want /binDetails", r.URL.Path)
		}
		got := r.URL.Query().Get("binGroups")
		if got != "GroupA,GroupB" {
			t.Errorf("binGroups = %q, want %q", got, "GroupA,GroupB")
		}
		json.NewEncoder(w).Encode(BinDetailsResponse{
			Response: Response{Code: 0},
			Data:     []BinDetail{{ID: "BIN-A1"}},
		})
	})
	defer srv.Close()

	bins, err := client.GetBinDetails("GroupA", "GroupB")
	if err != nil {
		t.Fatalf("GetBinDetails: %v", err)
	}
	if len(bins) != 1 || bins[0].ID != "BIN-A1" {
		t.Fatalf("bins = %+v, want one bin BIN-A1", bins)
	}
}

func TestGetBinDetails_ErrorEnvelope(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(BinDetailsResponse{
			Response: Response{Code: 42, Msg: "no bin group"},
		})
	})
	defer srv.Close()

	_, err := client.GetBinDetails("Bogus")
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestGetBinDetails_ServerError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	})
	defer srv.Close()

	_, err := client.GetBinDetails()
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestCheckBins(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/binCheck" {
			t.Errorf("path = %q, want /binCheck", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req BinCheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(req.Bins) != 2 || req.Bins[0] != "B1" || req.Bins[1] != "B2" {
			t.Errorf("Bins = %v, want [B1 B2]", req.Bins)
		}
		json.NewEncoder(w).Encode(BinCheckResponse{
			Response: Response{Code: 0},
			Bins: []BinCheckResult{
				{ID: "B1", Exist: true, Valid: true, Status: &BinPointStatus{PointName: "P1"}},
				{ID: "B2", Exist: false, Valid: false},
			},
		})
	})
	defer srv.Close()

	results, err := client.CheckBins([]string{"B1", "B2"})
	if err != nil {
		t.Fatalf("CheckBins: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len = %d, want 2", len(results))
	}
	if results[0].ID != "B1" || !results[0].Exist || !results[0].Valid {
		t.Errorf("results[0] = %+v, want B1/exist/valid", results[0])
	}
	if results[0].Status == nil || results[0].Status.PointName != "P1" {
		t.Errorf("results[0].Status = %+v, want PointName=P1", results[0].Status)
	}
	if results[1].Exist || results[1].Valid {
		t.Errorf("results[1] = %+v, want exist=false valid=false", results[1])
	}
}

func TestCheckBins_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(BinCheckResponse{
			Response: Response{Code: 1, Msg: "bad request"},
		})
	})
	defer srv.Close()

	_, err := client.CheckBins([]string{"X"})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestGetScene(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/scene" {
			t.Errorf("path = %q, want /scene", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		json.NewEncoder(w).Encode(SceneResponse{
			Response: Response{Code: 0},
			Scene: &Scene{
				Desc: "test-scene",
				RobotGroups: []RobotGroup{
					{Name: "G1", Robot: []SceneRobotEntry{{ID: "AMR-01"}}},
				},
			},
		})
	})
	defer srv.Close()

	scene, err := client.GetScene()
	if err != nil {
		t.Fatalf("GetScene: %v", err)
	}
	if scene == nil {
		t.Fatal("scene = nil, want non-nil")
	}
	if scene.Desc != "test-scene" {
		t.Errorf("Desc = %q, want test-scene", scene.Desc)
	}
	if len(scene.RobotGroups) != 1 || scene.RobotGroups[0].Name != "G1" {
		t.Errorf("RobotGroups = %+v, want one group G1", scene.RobotGroups)
	}
}

func TestGetScene_NullScene(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		// Code=0 but scene field omitted/null — bins.go treats this as an error.
		w.Write([]byte(`{"code":0,"msg":"ok"}`))
	})
	defer srv.Close()

	_, err := client.GetScene()
	if err == nil {
		t.Fatal("expected error when scene is null with code=0")
	}
}

func TestGetScene_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(SceneResponse{
			Response: Response{Code: 1, Msg: "scene unavailable"},
		})
	})
	defer srv.Close()

	_, err := client.GetScene()
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}
