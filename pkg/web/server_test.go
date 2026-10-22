package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSaveConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	s := NewServer(configPath)

	body := []byte(`{
		"apiURL": "http://localhost:2283/api",
		"apiKey": "test-key",
		"workers": 2,
		"debug": true,
		"googlePhotos": []
	}`)

	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.handleConfig(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read written config: %v", err)
	}

	if !strings.Contains(string(content), "http://localhost:2283/api") {
		t.Fatalf("saved content missing expected apiURL: %s", string(content))
	}
}

func TestSaveConfigPermissionError(t *testing.T) {
	// A directory cannot be overwritten as a config file on Windows or Unix,
	// including CI runners with elevated privileges.
	tmpDir := t.TempDir()
	roDir := filepath.Join(tmpDir, "ro")
	if err := os.Mkdir(roDir, 0755); err != nil {
		t.Fatal(err)
	}

	configPath := roDir
	s := NewServer(configPath)

	body := []byte(`{
		"apiURL": "http://localhost:2283/api",
		"apiKey": "test-key"
	}`)

	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.handleConfig(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected status 500 when file cannot be written, got %d", resp.StatusCode)
	}

	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "Failed to write config file") {
		t.Fatalf("expected error message to contain 'Failed to write config file', got %s", bodyStr)
	}
}

func TestHandleSync(t *testing.T) {
	s := NewServer("dummy.json")
	var triggered atomic.Bool
	s.OnSyncTrigger = func() {
		triggered.Store(true)
	}

	// Test POST
	req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	w := httptest.NewRecorder()
	s.handleSync(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on POST /api/sync, got %d", resp.StatusCode)
	}

	// Wait briefly for goroutine
	for i := 0; i < 50; i++ {
		if triggered.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !triggered.Load() {
		t.Fatal("expected OnSyncTrigger to be called")
	}

	// Test GET (for easy browser/Home Assistant webhook usage)
	triggered.Store(false)
	reqGet := httptest.NewRequest(http.MethodGet, "/sync_now", nil)
	wGet := httptest.NewRecorder()
	s.handleSync(wGet, reqGet)

	respGet := wGet.Result()
	if respGet.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on GET /sync_now, got %d", respGet.StatusCode)
	}

	for i := 0; i < 50; i++ {
		if triggered.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !triggered.Load() {
		t.Fatal("expected OnSyncTrigger to be called on GET")
	}

	// Test Method Not Allowed
	reqDelete := httptest.NewRequest(http.MethodDelete, "/api/sync", nil)
	wDelete := httptest.NewRecorder()
	s.handleSync(wDelete, reqDelete)

	if wDelete.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed on DELETE, got %d", wDelete.Result().StatusCode)
	}
}
func TestHandleStatus(t *testing.T) {
	s := NewServer("dummy.json")

	req := httptest.NewRequest(http.MethodGet, "/sync/status", nil)
	w := httptest.NewRecorder()

	s.handleStatus(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /sync/status, got %d", resp.StatusCode)
	}

	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "immichUser") || !strings.Contains(bodyStr, "lastRun") || !strings.Contains(bodyStr, "nextRun") {
		t.Fatalf("response missing expected fields: %s", bodyStr)
	}
}
