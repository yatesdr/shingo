package rds

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDownloadScene(t *testing.T) {
	payload := []byte("BINARY-SCENE-BLOB-\x00\x01\x02")
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/downloadScene" {
			t.Errorf("path = %q, want /downloadScene", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		_, _ = w.Write(payload)
	})
	defer srv.Close()

	got, err := client.DownloadScene()
	if err != nil {
		t.Fatalf("DownloadScene: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("body = %q, want %q", got, payload)
	}
}

func TestDownloadScene_HTTPError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("missing"))
	})
	defer srv.Close()

	_, err := client.DownloadScene()
	if err == nil {
		t.Fatal("expected error for HTTP 404")
	}
}

func TestUploadScene(t *testing.T) {
	body := []byte("scene-bytes-here")
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/uploadScene" {
			t.Errorf("path = %q, want /uploadScene", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("Content-Type = %q, want application/octet-stream", ct)
		}
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, body) {
			t.Errorf("body = %q, want %q", got, body)
		}
		_ = json.NewEncoder(w).Encode(Response{Code: 0, Msg: "ok"})
	})
	defer srv.Close()

	if err := client.UploadScene(bytes.NewReader(body)); err != nil {
		t.Fatalf("UploadScene: %v", err)
	}
}

func TestUploadScene_RDSErrorCode(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Response{Code: 42, Msg: "scene rejected"})
	})
	defer srv.Close()

	err := client.UploadScene(strings.NewReader("x"))
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("error should contain code 42, got %v", err)
	}
}

func TestUploadScene_HTTPError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	defer srv.Close()

	if err := client.UploadScene(strings.NewReader("x")); err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestSyncScene(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/syncScene" {
			t.Errorf("path = %q, want /syncScene", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		// SyncScene posts a nil body.
		raw, _ := io.ReadAll(r.Body)
		if len(raw) != 0 {
			t.Errorf("body = %q, want empty", raw)
		}
		_ = json.NewEncoder(w).Encode(Response{Code: 0, Msg: "ok"})
	})
	defer srv.Close()

	if err := client.SyncScene(); err != nil {
		t.Fatalf("SyncScene: %v", err)
	}
}

func TestSyncScene_RDSErrorCode(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Response{Code: 7, Msg: "no robots connected"})
	})
	defer srv.Close()

	err := client.SyncScene()
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
	if !strings.Contains(err.Error(), "no robots connected") {
		t.Errorf("error should contain server msg, got %v", err)
	}
}
