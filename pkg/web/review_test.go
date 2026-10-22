package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warreth.dev/gphotos2immich/pkg/googlephotos"
	"warreth.dev/gphotos2immich/pkg/review"
)

func reviewRequest(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestReviewFullHTTPFlowWithoutUploads(t *testing.T) {
	creates, adds := 0, 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "private-test-key" {
			t.Error("API key missing")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/search/metadata":
			io.WriteString(w, `{"assets":{"items":[{"id":"asset-1","originalFileName":"a.jpg","fileCreatedAt":"2020-01-02T03:04:05Z"}],"nextPage":null}}`)
		case "GET /api/albums":
			io.WriteString(w, `[]`)
		case "POST /api/albums":
			creates++
			io.WriteString(w, `{"id":"new-album"}`)
		case "PUT /api/albums/new-album/assets":
			adds++
			io.WriteString(w, `[{"id":"asset-1","success":true}]`)
		case "GET /api/assets/asset-1/thumbnail":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"Missing required permission: asset.view"}`)
		default:
			t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	}))
	defer fake.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"apiURL":"`+fake.URL+`/api","apiKey":"private-test-key","googlePhotos":[{"url":"legacy"}]}`), 0600)
	s := NewServer(path)
	s.ReviewOnly = true
	s.reviews = review.New(filepath.Join(dir, "reviews"))
	s.readAlbum = func(context.Context, string) (*googlephotos.Album, error) {
		return &googlephotos.Album{Title: "Test", Photos: []googlephotos.Photo{{ID: "google-1", OriginalFileName: "a.jpg", TakenAt: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)}}}, nil
	}
	p := reviewRequest(t, s, "POST", "/api/review/preview", `{"url":"https://photos.app.goo.gl/test"}`)
	if p.Code != 200 || creates != 0 || adds != 0 {
		t.Fatalf("preview failed or wrote: %d %s", p.Code, p.Body)
	}
	var session review.Session
	if err := json.Unmarshal(p.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"` + session.ID + `","title":"Test","selected":{"google-1":"asset-1"}}`
	for i := 0; i < 2; i++ {
		w := reviewRequest(t, s, "POST", "/api/review/confirm", body)
		if w.Code != 200 {
			t.Fatalf("confirm: %d %s", w.Code, w.Body)
		}
	}
	if creates != 1 || adds != 1 {
		t.Fatalf("duplicate writes: %d %d", creates, adds)
	}
	// New server represents a process restart, reading the same disk journal.
	restart := NewServer(path)
	restart.reviews = review.New(filepath.Join(dir, "reviews"))
	w := reviewRequest(t, restart, "POST", "/api/review/confirm", body)
	if w.Code != 200 || creates != 1 {
		t.Fatalf("restart duplicated: %d %s", w.Code, w.Body)
	}
	denied := reviewRequest(t, s, "GET", "/api/review/thumbnail?session="+session.ID+"&asset=foreign", "")
	if denied.Code != 404 {
		t.Fatal("foreign thumbnail accepted")
	}
	forbidden := reviewRequest(t, s, "GET", "/api/review/thumbnail?session="+session.ID+"&asset=asset-1", "")
	if forbidden.Code != 403 || !strings.Contains(forbidden.Body.String(), "asset.view") {
		t.Fatalf("permission error was hidden: %d %s", forbidden.Code, forbidden.Body)
	}
	cfg := reviewRequest(t, s, "GET", "/api/review/config", "")
	if strings.Contains(cfg.Body.String(), "private-test-key") {
		t.Fatal("API key leaked")
	}
}
func TestReviewCancelAndSyncDisabled(t *testing.T) {
	s := NewServer(filepath.Join(t.TempDir(), "config.json"))
	s.ReviewOnly = true
	for _, path := range []string{"/api/sync", "/sync_now", "/api/config"} {
		if w := reviewRequest(t, s, "POST", path, `{}`); w.Code != 403 {
			t.Fatalf("legacy mutation allowed: %s %d", path, w.Code)
		}
	}
	w := reviewRequest(t, s, "GET", "/", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Recreate your albums") {
		t.Fatal("review mode did not serve review UI")
	}
}
func TestReviewRefusesCrossOriginAndFormPosts(t *testing.T) {
	s := NewServer("unused")
	for _, tc := range []struct{ origin, ct string }{{"https://evil.test", "application/json"}, {"", "application/x-www-form-urlencoded"}} {
		r := httptest.NewRequest("POST", "http://localhost/api/review/config", strings.NewReader(`{}`))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Content-Type", tc.ct)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("unsafe request accepted: %d", w.Code)
		}
	}
}
func TestReviewSettingsPreserveLegacyFieldsAndSecrets(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	os.WriteFile(p, []byte(`{"apiKey":"secret","apiURL":"http://old/api","workers":7,"googlePhotos":[{"url":"legacy"}]}`), 0600)
	s := NewServer(p)
	w := reviewRequest(t, s, "POST", "/api/review/config", `{"apiURL":"http://new/api","apiKey":""}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	data, _ := os.ReadFile(p)
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	if cfg["apiKey"] != "secret" || cfg["workers"] != float64(7) || len(cfg["googlePhotos"].([]any)) != 1 {
		t.Fatalf("configuration lost: %s", data)
	}
}
func TestReviewConfigDoesNotOverwriteInvalidFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`broken`), 0600)
	s := NewServer(p)
	w := reviewRequest(t, s, "POST", "/api/review/config", `{"apiURL":"http://immich/api","apiKey":"secret"}`)
	data, _ := os.ReadFile(p)
	if w.Code != 500 || string(data) != "broken" {
		t.Fatal("invalid config overwritten")
	}
}
