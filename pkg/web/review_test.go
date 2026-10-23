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

func TestReviewDateFallbackCanBePreviewedAndConfirmed(t *testing.T) {
	taken := time.Date(2024, 1, 2, 0, 30, 0, 0, time.FixedZone("UTC+2", 7200))
	searches, creates, adds := 0, 0, 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/search/metadata":
			searches++
			var query map[string]any
			json.NewDecoder(r.Body).Decode(&query)
			if query["originalFileName"] == "google-name.jpg" {
				io.WriteString(w, `{"assets":{"items":[],"nextPage":null}}`)
				return
			}
			if query["takenAfter"] != taken.Add(-2*time.Hour).UTC().Format(time.RFC3339Nano) || query["takenBefore"] != taken.Add(2*time.Hour).UTC().Format(time.RFC3339Nano) || query["type"] != "IMAGE" {
				t.Errorf("incorrect date search: %v", query)
			}
			json.NewEncoder(w).Encode(map[string]any{"assets": map[string]any{"items": []map[string]any{
				{"id": "nearby", "type": "IMAGE", "originalFileName": "immich-name.jpg", "fileCreatedAt": taken.Add(time.Hour)},
				{"id": "outside", "type": "IMAGE", "originalFileName": "outside.jpg", "fileCreatedAt": taken.Add(3 * time.Hour)},
			}}})
		case "GET /api/assets/nearby/thumbnail":
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, "preview")
		case "GET /api/albums":
			io.WriteString(w, `[]`)
		case "POST /api/albums":
			creates++
			io.WriteString(w, `{"id":"new-album"}`)
		case "PUT /api/albums/new-album/assets":
			adds++
			var request struct {
				IDs []string `json:"ids"`
			}
			json.NewDecoder(r.Body).Decode(&request)
			if len(request.IDs) != 1 || request.IDs[0] != "nearby" {
				t.Errorf("unexpected selected alternatives: %v", request.IDs)
			}
			io.WriteString(w, `[{"id":"nearby","success":true}]`)
		default:
			t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	}))
	defer fake.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"apiURL":"`+fake.URL+`/api","apiKey":"test-key","dataDir":"`+filepath.ToSlash(dir)+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(path)
	s.ReviewOnly = true
	s.readAlbum = func(context.Context, string) (*googlephotos.Album, error) {
		return &googlephotos.Album{Title: "Trip", Photos: []googlephotos.Photo{{ID: "google-1", OriginalFileName: "google-name.jpg", TakenAt: taken}}}, nil
	}
	w := reviewRequest(t, s, "POST", "/api/review/preview", `{"url":"https://photos.app.goo.gl/test"}`)
	var session review.Session
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil || w.Code != 200 || searches != 2 || creates != 0 || adds != 0 {
		t.Fatalf("date preview failed or wrote to Immich: %d %s", w.Code, w.Body)
	}
	if len(session.Items) != 1 || session.Items[0].Status != "date_suggestions" || len(session.Items[0].Candidates) != 1 || session.Items[0].Candidates[0].ID != "nearby" {
		t.Fatalf("wrong date alternatives: %+v", session.Items)
	}
	if w := reviewRequest(t, s, "GET", "/api/review/thumbnail?session="+session.ID+"&asset=nearby", ""); w.Code != 200 {
		t.Fatalf("date alternative thumbnail rejected: %d %s", w.Code, w.Body)
	}
	if w := reviewRequest(t, s, "GET", "/api/review/thumbnail?session="+session.ID+"&asset=outside", ""); w.Code != 404 {
		t.Fatalf("out-of-range thumbnail allowed: %d", w.Code)
	}
	w = reviewRequest(t, s, "POST", "/api/review/confirm", `{"id":"`+session.ID+`","title":"Trip","selected":{"google-1":"nearby"}}`)
	if w.Code != 200 || creates != 1 || adds != 1 {
		t.Fatalf("date alternative confirmation failed: %d %s", w.Code, w.Body)
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

func TestReviewUpdatesAnExplicitlyChosenExistingAlbumWithoutDuplicates(t *testing.T) {
	creates, adds := 0, 0
	albums := []map[string]any{
		{"id": "existing-1", "albumName": "Trip", "assetCount": 2, "createdAt": "2024-01-01T12:00:00Z", "assets": []map[string]string{{"id": "unrelated-photo"}}},
		{"id": "existing-2", "albumName": "Trip", "assetCount": 15, "createdAt": "2025-01-01T12:00:00Z"},
	}
	existingPhotos := map[string]bool{"a": true, "unrelated-photo": true}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/search/metadata":
			var query map[string]any
			json.NewDecoder(r.Body).Decode(&query)
			name, _ := query["originalFileName"].(string)
			id := strings.TrimSuffix(name, ".jpg")
			if id != "a" && id != "b" {
				t.Errorf("unexpected filename: %s", name)
			}
			json.NewEncoder(w).Encode(map[string]any{"assets": map[string]any{"items": []map[string]any{{"id": id, "originalFileName": name, "fileCreatedAt": "2020-01-02T03:04:05Z"}}}})
		case "GET /api/albums":
			json.NewEncoder(w).Encode(albums)
		case "POST /api/albums":
			creates++
			t.Error("existing album update created a new album")
			io.WriteString(w, `{"id":"wrong-new-album"}`)
		case "PUT /api/albums/existing-1/assets":
			adds++
			var body struct {
				IDs []string `json:"ids"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			results := []map[string]any{}
			for _, id := range body.IDs {
				if id != "a" && id != "b" {
					t.Errorf("unexpected addition %s", id)
				}
				if existingPhotos[id] {
					results = append(results, map[string]any{"id": id, "success": false, "error": "duplicate"})
				} else {
					existingPhotos[id] = true
					results = append(results, map[string]any{"id": id, "success": true})
				}
			}
			json.NewEncoder(w).Encode(results)
		default:
			t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	}))
	defer fake.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg, _ := json.Marshal(map[string]any{"apiURL": fake.URL + "/api", "apiKey": "private-test-key", "dataDir": dir})
	if err := os.WriteFile(path, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(path)
	s.ReviewOnly = true
	s.readAlbum = func(context.Context, string) (*googlephotos.Album, error) {
		return &googlephotos.Album{Title: "Source title", Photos: []googlephotos.Photo{
			{ID: "g1", OriginalFileName: "a.jpg", TakenAt: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)},
			{ID: "g2", OriginalFileName: "b.jpg", TakenAt: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)},
		}}, nil
	}
	preview := reviewRequest(t, s, "POST", "/api/review/preview", `{"url":"https://photos.app.goo.gl/test"}`)
	var session review.Session
	if err := json.Unmarshal(preview.Body.Bytes(), &session); err != nil || preview.Code != 200 {
		t.Fatalf("preview failed: %d %s", preview.Code, preview.Body)
	}
	body := map[string]any{"id": session.ID, "title": "Trip", "selected": map[string]string{"g1": "a", "g2": "b"}}
	payload, _ := json.Marshal(body)
	conflict := reviewRequest(t, s, "POST", "/api/review/confirm", string(payload))
	var response struct {
		Code    string               `json:"code"`
		Albums  []review.AlbumOption `json:"albums"`
		Session review.Session       `json:"session"`
	}
	if err := json.Unmarshal(conflict.Body.Bytes(), &response); err != nil || conflict.Code != 409 || response.Code != "album_exists" || len(response.Albums) != 2 || response.Session.Status != "ready" || response.Session.Title != "Trip" || creates != 0 || adds != 0 {
		t.Fatalf("existing albums were not offered without writes: %d %s", conflict.Code, conflict.Body)
	}
	if strings.Contains(conflict.Body.String(), "unrelated-photo") || strings.Contains(conflict.Body.String(), "private-test-key") || response.Albums[0].CreatedAt == nil || response.Albums[0].AssetCount == nil || *response.Albums[0].AssetCount != 2 {
		t.Fatal("album options leaked assets/credentials or omitted decision metadata")
	}
	body["existingAlbumId"] = "foreign"
	payload, _ = json.Marshal(body)
	if w := reviewRequest(t, s, "POST", "/api/review/confirm", string(payload)); w.Code != 409 || adds != 0 || creates != 0 {
		t.Fatal("foreign destination accepted")
	}
	// Recheck the destination against Immich if it disappears after being offered.
	originalAlbums := albums
	albums = albums[1:]
	body["existingAlbumId"] = "existing-1"
	payload, _ = json.Marshal(body)
	if w := reviewRequest(t, s, "POST", "/api/review/confirm", string(payload)); w.Code != 409 || adds != 0 || creates != 0 {
		t.Fatal("stale destination created or updated an album")
	}
	albums = originalAlbums
	updated := reviewRequest(t, s, "POST", "/api/review/confirm", string(payload))
	if err := json.Unmarshal(updated.Body.Bytes(), &session); err != nil || updated.Code != 200 || session.Status != "complete" || !session.ExistingAlbum || session.AlbumID != "existing-1" || creates != 0 || adds != 1 {
		t.Fatalf("existing album update failed: %d %s", updated.Code, updated.Body)
	}
	if len(existingPhotos) != 3 || !existingPhotos["a"] || !existingPhotos["b"] || !existingPhotos["unrelated-photo"] {
		t.Fatalf("existing contents were not preserved: %v", existingPhotos)
	}
	restarted := NewServer(path)
	repeated := reviewRequest(t, restarted, "POST", "/api/review/confirm", string(payload))
	if repeated.Code != 200 || adds != 1 || creates != 0 {
		t.Fatalf("restarted update repeated writes: %d %s", repeated.Code, repeated.Body)
	}
}
