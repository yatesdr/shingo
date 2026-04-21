package rds

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSetDispatchable(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dispatchable" {
			t.Errorf("path = %q, want /dispatchable", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req DispatchableRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Vehicles) != 2 || req.Vehicles[0] != "AMB-01" || req.Vehicles[1] != "AMB-02" {
			t.Errorf("Vehicles = %v, want [AMB-01 AMB-02]", req.Vehicles)
		}
		if req.Type != "dispatchable" {
			t.Errorf("Type = %q, want dispatchable", req.Type)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.SetDispatchable(&DispatchableRequest{
		Vehicles: []string{"AMB-01", "AMB-02"},
		Type:     "dispatchable",
	})
	if err != nil {
		t.Fatalf("SetDispatchable: %v", err)
	}
}

func TestSetDispatchable_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "fail"})
	})
	defer srv.Close()

	err := client.SetDispatchable(&DispatchableRequest{Vehicles: []string{"x"}, Type: "dispatchable"})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestRedoFailed(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/redoFailedOrder" {
			t.Errorf("path = %q, want /redoFailedOrder", r.URL.Path)
		}
		var req RedoFailedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Vehicles) != 1 || req.Vehicles[0] != "AMB-01" {
			t.Errorf("Vehicles = %v, want [AMB-01]", req.Vehicles)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.RedoFailed(&RedoFailedRequest{Vehicles: []string{"AMB-01"}})
	if err != nil {
		t.Fatalf("RedoFailed: %v", err)
	}
}

func TestRedoFailed_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 2, Msg: "no failed block"})
	})
	defer srv.Close()

	err := client.RedoFailed(&RedoFailedRequest{Vehicles: []string{"x"}})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestManualFinish(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manualFinished" {
			t.Errorf("path = %q, want /manualFinished", r.URL.Path)
		}
		var req ManualFinishRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Vehicles) != 1 || req.Vehicles[0] != "AMB-03" {
			t.Errorf("Vehicles = %v, want [AMB-03]", req.Vehicles)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.ManualFinish(&ManualFinishRequest{Vehicles: []string{"AMB-03"}})
	if err != nil {
		t.Fatalf("ManualFinish: %v", err)
	}
}

func TestManualFinish_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "fail"})
	})
	defer srv.Close()

	err := client.ManualFinish(&ManualFinishRequest{Vehicles: []string{"x"}})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestGetRobotMap(t *testing.T) {
	want := []byte("MAP-BINARY-DATA-\x00\x01\x02")
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robotSmap" {
			t.Errorf("path = %q, want /robotSmap", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		q := r.URL.Query()
		if q.Get("vehicle") != "AMB-01" {
			t.Errorf("vehicle = %q, want AMB-01", q.Get("vehicle"))
		}
		if q.Get("map") != "factory.smap" {
			t.Errorf("map = %q, want factory.smap", q.Get("map"))
		}
		w.Write(want)
	})
	defer srv.Close()

	got, err := client.GetRobotMap("AMB-01", "factory.smap")
	if err != nil {
		t.Fatalf("GetRobotMap: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGetRobotMap_HTTPError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("no map"))
	})
	defer srv.Close()

	_, err := client.GetRobotMap("AMB-01", "missing.smap")
	if err == nil {
		t.Fatal("expected error for HTTP 404")
	}
}

func TestPreemptControl(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lock" {
			t.Errorf("path = %q, want /lock", r.URL.Path)
		}
		var req VehiclesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Vehicles) != 2 || req.Vehicles[0] != "AMB-1" || req.Vehicles[1] != "AMB-2" {
			t.Errorf("Vehicles = %v, want [AMB-1 AMB-2]", req.Vehicles)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	if err := client.PreemptControl([]string{"AMB-1", "AMB-2"}); err != nil {
		t.Fatalf("PreemptControl: %v", err)
	}
}

func TestPreemptControl_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "already locked"})
	})
	defer srv.Close()

	if err := client.PreemptControl([]string{"AMB-1"}); err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestReleaseControl(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/unlock" {
			t.Errorf("path = %q, want /unlock", r.URL.Path)
		}
		var req VehiclesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Vehicles) != 1 || req.Vehicles[0] != "AMB-7" {
			t.Errorf("Vehicles = %v, want [AMB-7]", req.Vehicles)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	if err := client.ReleaseControl([]string{"AMB-7"}); err != nil {
		t.Fatalf("ReleaseControl: %v", err)
	}
}

func TestReleaseControl_Disconnect(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {})
	srv.Close()

	if err := client.ReleaseControl([]string{"AMB-7"}); err == nil {
		t.Fatal("expected error when server is unreachable")
	}
}

func TestSetParamsTemp(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/setParams" {
			t.Errorf("path = %q, want /setParams", r.URL.Path)
		}
		var req ModifyParamsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Vehicle != "AMB-01" {
			t.Errorf("Vehicle = %q, want AMB-01", req.Vehicle)
		}
		plugin, ok := req.Body["motion"]
		if !ok {
			t.Fatalf("Body missing motion plugin: %+v", req.Body)
		}
		if v, ok := plugin["max_speed"].(float64); !ok || v != 1.5 {
			t.Errorf("motion.max_speed = %v, want 1.5", plugin["max_speed"])
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	body := map[string]map[string]any{
		"motion": {"max_speed": 1.5},
	}
	if err := client.SetParamsTemp("AMB-01", body); err != nil {
		t.Fatalf("SetParamsTemp: %v", err)
	}
}

func TestSetParamsTemp_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "bad params"})
	})
	defer srv.Close()

	err := client.SetParamsTemp("AMB-01", map[string]map[string]any{"x": {"y": 1}})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestSetParamsPerm(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/saveParams" {
			t.Errorf("path = %q, want /saveParams", r.URL.Path)
		}
		var req ModifyParamsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Vehicle != "AMB-02" {
			t.Errorf("Vehicle = %q, want AMB-02", req.Vehicle)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	body := map[string]map[string]any{"slam": {"resolution": 0.05}}
	if err := client.SetParamsPerm("AMB-02", body); err != nil {
		t.Fatalf("SetParamsPerm: %v", err)
	}
}

func TestSetParamsPerm_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "fail"})
	})
	defer srv.Close()

	err := client.SetParamsPerm("AMB-02", map[string]map[string]any{"x": {"y": 1}})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestRestoreParamDefaults(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reloadParams" {
			t.Errorf("path = %q, want /reloadParams", r.URL.Path)
		}
		var req RestoreParamsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Vehicle != "AMB-03" {
			t.Errorf("Vehicle = %q, want AMB-03", req.Vehicle)
		}
		if len(req.Body) != 1 || req.Body[0].Plugin != "motion" {
			t.Errorf("Body = %+v, want one motion entry", req.Body)
		}
		if len(req.Body[0].Params) != 2 || req.Body[0].Params[0] != "max_speed" || req.Body[0].Params[1] != "accel" {
			t.Errorf("Params = %v, want [max_speed accel]", req.Body[0].Params)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.RestoreParamDefaults(&RestoreParamsRequest{
		Vehicle: "AMB-03",
		Body: []RestoreParamsEntry{
			{Plugin: "motion", Params: []string{"max_speed", "accel"}},
		},
	})
	if err != nil {
		t.Fatalf("RestoreParamDefaults: %v", err)
	}
}

func TestRestoreParamDefaults_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "fail"})
	})
	defer srv.Close()

	err := client.RestoreParamDefaults(&RestoreParamsRequest{Vehicle: "x"})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestSwitchMap(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/switchMap" {
			t.Errorf("path = %q, want /switchMap", r.URL.Path)
		}
		var req SwitchMapRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Vehicle != "AMB-01" {
			t.Errorf("Vehicle = %q, want AMB-01", req.Vehicle)
		}
		if req.Map != "warehouse-2" {
			t.Errorf("Map = %q, want warehouse-2", req.Map)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	if err := client.SwitchMap("AMB-01", "warehouse-2"); err != nil {
		t.Fatalf("SwitchMap: %v", err)
	}
}

func TestSwitchMap_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "no such map"})
	})
	defer srv.Close()

	if err := client.SwitchMap("AMB-01", "missing"); err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestConfirmRelocalization(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reLocConfirm" {
			t.Errorf("path = %q, want /reLocConfirm", r.URL.Path)
		}
		var req VehiclesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Vehicles) != 1 || req.Vehicles[0] != "AMB-9" {
			t.Errorf("Vehicles = %v, want [AMB-9]", req.Vehicles)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	if err := client.ConfirmRelocalization([]string{"AMB-9"}); err != nil {
		t.Fatalf("ConfirmRelocalization: %v", err)
	}
}

func TestConfirmRelocalization_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "fail"})
	})
	defer srv.Close()

	if err := client.ConfirmRelocalization([]string{"x"}); err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestPauseNavigation(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gotoSitePause" {
			t.Errorf("path = %q, want /gotoSitePause", r.URL.Path)
		}
		var req VehiclesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Vehicles) != 1 || req.Vehicles[0] != "AMB-1" {
			t.Errorf("Vehicles = %v, want [AMB-1]", req.Vehicles)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	if err := client.PauseNavigation([]string{"AMB-1"}); err != nil {
		t.Fatalf("PauseNavigation: %v", err)
	}
}

func TestPauseNavigation_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "fail"})
	})
	defer srv.Close()

	if err := client.PauseNavigation([]string{"x"}); err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestResumeNavigation(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gotoSiteResume" {
			t.Errorf("path = %q, want /gotoSiteResume", r.URL.Path)
		}
		var req VehiclesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Vehicles) != 1 || req.Vehicles[0] != "AMB-1" {
			t.Errorf("Vehicles = %v, want [AMB-1]", req.Vehicles)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	if err := client.ResumeNavigation([]string{"AMB-1"}); err != nil {
		t.Fatalf("ResumeNavigation: %v", err)
	}
}

func TestResumeNavigation_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "fail"})
	})
	defer srv.Close()

	if err := client.ResumeNavigation([]string{"x"}); err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}
