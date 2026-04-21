package rds

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestGetSimStateTemplate(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/getSimRobotStateTemplate" {
			t.Errorf("path = %q, want /getSimRobotStateTemplate", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"x":1.5,"y":2.5,"angle":0}}`))
	})
	defer srv.Close()

	got, err := client.GetSimStateTemplate()
	if err != nil {
		t.Fatalf("GetSimStateTemplate: %v", err)
	}
	// Assert the raw JSON round-trips into a map with the expected fields.
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if parsed["x"] != 1.5 {
		t.Errorf("x = %v, want 1.5", parsed["x"])
	}
	if parsed["y"] != 2.5 {
		t.Errorf("y = %v, want 2.5", parsed["y"])
	}
}

func TestGetSimStateTemplate_RDSErrorCode(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":9,"msg":"simulation disabled"}`))
	})
	defer srv.Close()

	_, err := client.GetSimStateTemplate()
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
	if !strings.Contains(err.Error(), "simulation disabled") {
		t.Errorf("error should contain server msg, got %v", err)
	}
}

func TestGetSimStateTemplate_HTTPError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	defer srv.Close()

	_, err := client.GetSimStateTemplate()
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestUpdateSimState(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/updateSimRobotState" {
			t.Errorf("path = %q, want /updateSimRobotState", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got["vehicle"] != "AMB-01" {
			t.Errorf("vehicle = %v, want AMB-01", got["vehicle"])
		}
		if got["x"] != 3.25 {
			t.Errorf("x = %v, want 3.25", got["x"])
		}
		_ = json.NewEncoder(w).Encode(Response{Code: 0, Msg: "ok"})
	})
	defer srv.Close()

	err := client.UpdateSimState(map[string]any{
		"vehicle": "AMB-01",
		"x":       3.25,
	})
	if err != nil {
		t.Fatalf("UpdateSimState: %v", err)
	}
}

func TestUpdateSimState_RDSErrorCode(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Response{Code: 13, Msg: "robot not simulated"})
	})
	defer srv.Close()

	err := client.UpdateSimState(map[string]any{"vehicle": "real-robot"})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
	if !strings.Contains(err.Error(), "robot not simulated") {
		t.Errorf("error should contain server msg, got %v", err)
	}
}

func TestUpdateSimState_HTTPError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad"))
	})
	defer srv.Close()

	err := client.UpdateSimState(map[string]any{"vehicle": "x"})
	if err == nil {
		t.Fatal("expected error for HTTP 400")
	}
}
