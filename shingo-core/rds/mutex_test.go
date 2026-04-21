package rds

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestOccupyMutexGroup(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/getBlockGroup" {
			t.Errorf("path = %q, want /getBlockGroup", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req MutexGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.ID != "wp-99" {
			t.Errorf("ID = %q, want %q", req.ID, "wp-99")
		}
		want := []string{"groupA", "groupB"}
		if !reflect.DeepEqual(req.BlockGroup, want) {
			t.Errorf("BlockGroup = %v, want %v", req.BlockGroup, want)
		}
		// Bare JSON array response.
		_, _ = w.Write([]byte(`[{"name":"groupA","isOccupied":true,"occupier":"wp-99"},{"name":"groupB","isOccupied":false,"occupier":""}]`))
	})
	defer srv.Close()

	got, err := client.OccupyMutexGroup("wp-99", []string{"groupA", "groupB"})
	if err != nil {
		t.Fatalf("OccupyMutexGroup: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "groupA" || !got[0].IsOccupied || got[0].Occupier != "wp-99" {
		t.Errorf("got[0] = %+v, want groupA/true/wp-99", got[0])
	}
	if got[1].Name != "groupB" || got[1].IsOccupied {
		t.Errorf("got[1] = %+v, want groupB/false", got[1])
	}
}

func TestOccupyMutexGroup_HTTPError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	defer srv.Close()

	_, err := client.OccupyMutexGroup("wp-1", []string{"g"})
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestReleaseMutexGroup(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releaseBlockGroup" {
			t.Errorf("path = %q, want /releaseBlockGroup", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req MutexGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.ID != "wp-7" {
			t.Errorf("ID = %q, want %q", req.ID, "wp-7")
		}
		if !reflect.DeepEqual(req.BlockGroup, []string{"only-group"}) {
			t.Errorf("BlockGroup = %v, want [only-group]", req.BlockGroup)
		}
		_, _ = w.Write([]byte(`[{"name":"only-group","isOccupied":false,"occupier":""}]`))
	})
	defer srv.Close()

	got, err := client.ReleaseMutexGroup("wp-7", []string{"only-group"})
	if err != nil {
		t.Fatalf("ReleaseMutexGroup: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Name != "only-group" || got[0].IsOccupied {
		t.Errorf("got[0] = %+v, want only-group/false", got[0])
	}
}

func TestReleaseMutexGroup_DecodeFailure(t *testing.T) {
	// Server returns a non-array payload that the post() decoder cannot
	// fit into []MutexGroupResult. The wrapper should propagate the error.
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"not":"an-array"}`))
	})
	defer srv.Close()

	_, err := client.ReleaseMutexGroup("wp-1", []string{"g"})
	if err == nil {
		t.Fatal("expected decode error for non-array body")
	}
}

func TestGetMutexGroupStatus(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blockGroupStatus" {
			t.Errorf("path = %q, want /blockGroupStatus", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		_, _ = w.Write([]byte(`[{"name":"g1","isOccupied":true,"occupier":"wp-3"},{"name":"g2","isOccupied":false,"occupier":""}]`))
	})
	defer srv.Close()

	got, err := client.GetMutexGroupStatus(nil)
	if err != nil {
		t.Fatalf("GetMutexGroupStatus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "g1" || !got[0].IsOccupied || got[0].Occupier != "wp-3" {
		t.Errorf("got[0] = %+v, want g1/true/wp-3", got[0])
	}
	if got[1].Name != "g2" || got[1].IsOccupied {
		t.Errorf("got[1] = %+v, want g2/false", got[1])
	}
}

func TestGetMutexGroupStatus_HTTPError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	})
	defer srv.Close()

	_, err := client.GetMutexGroupStatus(nil)
	if err == nil {
		t.Fatal("expected error for HTTP 502")
	}
}

func TestGetMutexGroupStatus_BadJSON(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	})
	defer srv.Close()

	_, err := client.GetMutexGroupStatus(nil)
	if err == nil {
		t.Fatal("expected decode error for non-JSON body")
	}
}
