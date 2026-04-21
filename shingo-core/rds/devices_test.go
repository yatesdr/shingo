package rds

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCallTerminal(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callTerminal" {
			t.Errorf("path = %q, want /callTerminal", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req CallTerminalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.ID != "TERM-1" {
			t.Errorf("ID = %q, want TERM-1", req.ID)
		}
		if req.Type != "open" {
			t.Errorf("Type = %q, want open", req.Type)
		}
		// Reply with code=0 + an arbitrary `data` payload.
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"echo":"hello"}}`))
	})
	defer srv.Close()

	resp, err := client.CallTerminal(&CallTerminalRequest{ID: "TERM-1", Type: "open"})
	if err != nil {
		t.Fatalf("CallTerminal: %v", err)
	}
	if resp == nil {
		t.Fatal("resp = nil")
	}
	if resp.Code != 0 {
		t.Errorf("resp.Code = %d, want 0", resp.Code)
	}
	if string(resp.Data) != `{"echo":"hello"}` {
		t.Errorf("resp.Data = %s, want %s", string(resp.Data), `{"echo":"hello"}`)
	}
}

func TestCallTerminal_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(CallTerminalResponse{
			Response: Response{Code: 5, Msg: "terminal not found"},
		})
	})
	defer srv.Close()

	_, err := client.CallTerminal(&CallTerminalRequest{ID: "missing"})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestGetDevicesStatus_NoFilter(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devicesDetails" {
			t.Errorf("path = %q, want /devicesDetails", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("rawQuery = %q, want empty", r.URL.RawQuery)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		json.NewEncoder(w).Encode(DevicesResponse{
			Response: Response{Code: 0},
			Doors:    []DoorStatus{{Name: "D1", State: 1, Disabled: false}},
			Lifts:    []LiftStatus{{Name: "L1", State: 0, Disabled: true}},
			Terminals: []TerminalStatus{
				{ID: "T1", State: 2},
			},
		})
	})
	defer srv.Close()

	resp, err := client.GetDevicesStatus()
	if err != nil {
		t.Fatalf("GetDevicesStatus: %v", err)
	}
	if len(resp.Doors) != 1 || resp.Doors[0].Name != "D1" || resp.Doors[0].State != 1 {
		t.Errorf("Doors = %+v, want one door D1/state=1", resp.Doors)
	}
	if len(resp.Lifts) != 1 || !resp.Lifts[0].Disabled {
		t.Errorf("Lifts = %+v, want one disabled lift", resp.Lifts)
	}
	if len(resp.Terminals) != 1 || resp.Terminals[0].ID != "T1" {
		t.Errorf("Terminals = %+v, want one T1", resp.Terminals)
	}
}

func TestGetDevicesStatus_WithFilter(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("devices")
		if got != "D1,D2" {
			t.Errorf("devices = %q, want D1,D2", got)
		}
		json.NewEncoder(w).Encode(DevicesResponse{
			Response: Response{Code: 0},
			Doors:    []DoorStatus{{Name: "D1"}, {Name: "D2"}},
		})
	})
	defer srv.Close()

	resp, err := client.GetDevicesStatus("D1", "D2")
	if err != nil {
		t.Fatalf("GetDevicesStatus: %v", err)
	}
	if len(resp.Doors) != 2 {
		t.Fatalf("len(Doors) = %d, want 2", len(resp.Doors))
	}
}

func TestGetDevicesStatus_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(DevicesResponse{
			Response: Response{Code: 3, Msg: "fail"},
		})
	})
	defer srv.Close()

	_, err := client.GetDevicesStatus()
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestCallDoor(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callDoor" {
			t.Errorf("path = %q, want /callDoor", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		// CallDoor sends the slice directly as the request body.
		var req []CallDoorRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req) != 2 {
			t.Fatalf("len(req) = %d, want 2", len(req))
		}
		if req[0].Name != "Door-A" || req[0].State != 1 {
			t.Errorf("req[0] = %+v, want {Name:Door-A State:1}", req[0])
		}
		if req[1].Name != "Door-B" || req[1].State != 0 {
			t.Errorf("req[1] = %+v, want {Name:Door-B State:0}", req[1])
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.CallDoor([]CallDoorRequest{
		{Name: "Door-A", State: 1},
		{Name: "Door-B", State: 0},
	})
	if err != nil {
		t.Fatalf("CallDoor: %v", err)
	}
}

func TestCallDoor_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "door fault"})
	})
	defer srv.Close()

	err := client.CallDoor([]CallDoorRequest{{Name: "Door-A", State: 1}})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestDisableDoor(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/disableDoor" {
			t.Errorf("path = %q, want /disableDoor", r.URL.Path)
		}
		var req DisableDeviceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Names) != 1 || req.Names[0] != "Door-X" {
			t.Errorf("Names = %v, want [Door-X]", req.Names)
		}
		if !req.Disabled {
			t.Errorf("Disabled = false, want true")
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.DisableDoor(&DisableDeviceRequest{Names: []string{"Door-X"}, Disabled: true})
	if err != nil {
		t.Fatalf("DisableDoor: %v", err)
	}
}

func TestDisableDoor_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 4, Msg: "fail"})
	})
	defer srv.Close()

	err := client.DisableDoor(&DisableDeviceRequest{Names: []string{"Door-X"}, Disabled: true})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestCallLift(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callLift" {
			t.Errorf("path = %q, want /callLift", r.URL.Path)
		}
		var req []CallLiftRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req) != 1 {
			t.Fatalf("len(req) = %d, want 1", len(req))
		}
		if req[0].Name != "Lift-1" || req[0].TargetArea != "Floor-2" {
			t.Errorf("req[0] = %+v, want {Lift-1 Floor-2}", req[0])
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.CallLift([]CallLiftRequest{{Name: "Lift-1", TargetArea: "Floor-2"}})
	if err != nil {
		t.Fatalf("CallLift: %v", err)
	}
}

func TestCallLift_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "lift busy"})
	})
	defer srv.Close()

	err := client.CallLift([]CallLiftRequest{{Name: "Lift-1", TargetArea: "F1"}})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestDisableLift(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/disableLift" {
			t.Errorf("path = %q, want /disableLift", r.URL.Path)
		}
		var req DisableDeviceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Names) != 2 || req.Names[0] != "Lift-1" || req.Names[1] != "Lift-2" {
			t.Errorf("Names = %v, want [Lift-1 Lift-2]", req.Names)
		}
		if req.Disabled {
			t.Errorf("Disabled = true, want false")
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.DisableLift(&DisableDeviceRequest{Names: []string{"Lift-1", "Lift-2"}, Disabled: false})
	if err != nil {
		t.Fatalf("DisableLift: %v", err)
	}
}

func TestDisableLift_ServerError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	defer srv.Close()

	err := client.DisableLift(&DisableDeviceRequest{Names: []string{"L"}, Disabled: true})
	if err == nil {
		t.Fatal("expected error for HTTP 502")
	}
}
