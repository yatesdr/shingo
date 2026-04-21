package rds

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestPing is defined in client_test.go — not duplicated here.

func TestGetProfiles(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/getProfiles" {
			t.Errorf("path = %q, want /getProfiles", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var req GetProfilesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.File != "dispatch.cfg" {
			t.Errorf("File = %q, want dispatch.cfg", req.File)
		}
		// Return arbitrary JSON — the wrapper returns it as a raw message.
		_, _ = w.Write([]byte(`{"foo":"bar","nested":{"n":7}}`))
	})
	defer srv.Close()

	got, err := client.GetProfiles("dispatch.cfg")
	if err != nil {
		t.Fatalf("GetProfiles: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("returned payload is not valid JSON: %v (%s)", err, string(got))
	}
	if parsed["foo"] != "bar" {
		t.Errorf("foo = %v, want bar", parsed["foo"])
	}
}

func TestGetProfiles_HTTPError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such file"))
	})
	defer srv.Close()

	_, err := client.GetProfiles("missing.cfg")
	if err == nil {
		t.Fatal("expected error for HTTP 404")
	}
}

func TestGetLicenseInfo(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/licInfo" {
			t.Errorf("path = %q, want /licInfo", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		_ = json.NewEncoder(w).Encode(LicenseResponse{
			Response: Response{Code: 0, Msg: "ok"},
			Data: &LicenseInfo{
				MaxRobots: 42,
				Expiry:    "2099-12-31",
				Features: []LicenseFeature{
					{Name: "multi-floor", Enabled: true},
					{Name: "simulation", Enabled: false},
				},
			},
		})
	})
	defer srv.Close()

	info, err := client.GetLicenseInfo()
	if err != nil {
		t.Fatalf("GetLicenseInfo: %v", err)
	}
	if info.MaxRobots != 42 {
		t.Errorf("MaxRobots = %d, want 42", info.MaxRobots)
	}
	if info.Expiry != "2099-12-31" {
		t.Errorf("Expiry = %q, want 2099-12-31", info.Expiry)
	}
	if len(info.Features) != 2 {
		t.Fatalf("len(Features) = %d, want 2", len(info.Features))
	}
	if info.Features[0].Name != "multi-floor" || !info.Features[0].Enabled {
		t.Errorf("Features[0] = %+v, want multi-floor/true", info.Features[0])
	}
	if info.Features[1].Enabled {
		t.Errorf("Features[1].Enabled = true, want false")
	}
}

func TestGetLicenseInfo_RDSErrorCode(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(LicenseResponse{
			Response: Response{Code: 5, Msg: "license expired"},
		})
	})
	defer srv.Close()

	_, err := client.GetLicenseInfo()
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
	if !strings.Contains(err.Error(), "license expired") {
		t.Errorf("error should contain server msg, got %v", err)
	}
}

func TestGetLicenseInfo_EmptyData(t *testing.T) {
	// Documented edge case: code=0 but data is null/absent. The client must
	// surface this rather than returning a nil LicenseInfo.
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	})
	defer srv.Close()

	_, err := client.GetLicenseInfo()
	if err == nil {
		t.Fatal("expected error for code=0 with null data")
	}
	if !strings.Contains(err.Error(), "empty response data") {
		t.Errorf("error should mention empty data, got %v", err)
	}
}

func TestGetLicenseInfo_HTTPError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauth"))
	})
	defer srv.Close()

	_, err := client.GetLicenseInfo()
	if err == nil {
		t.Fatal("expected error for HTTP 401")
	}
}
