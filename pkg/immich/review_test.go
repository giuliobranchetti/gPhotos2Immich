package immich

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reviewTestClient(h http.Handler) *Client {
	c := NewClient("https://immich.test/api", "test-key", 1)
	c.client.Transport = reviewTransport(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	return c
}
func TestSearchFilenamePaginatesAndDoesNotFilterDates(t *testing.T) {
	calls := 0
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var q map[string]any
		json.NewDecoder(r.Body).Decode(&q)
		if q["takenAfter"] != nil || q["takenBefore"] != nil || q["originalFileName"] != "a.jpg" {
			t.Errorf("wrong query: %v", q)
		}
		if q["page"] == float64(1) {
			io.WriteString(w, `{"assets":{"items":[{"id":"a","originalFileName":"a.jpg"},{"id":"near","originalFileName":"other-a.jpg"}],"nextPage":"2"}}`)
		} else {
			io.WriteString(w, `{"assets":{"items":[{"id":"b","originalFileName":"a.jpg"}],"nextPage":null}}`)
		}
	}))
	assets, err := c.SearchOriginalFilename(context.Background(), "a.jpg")
	if err != nil || len(assets) != 2 || calls != 2 {
		t.Fatalf("%+v %v calls=%d", assets, err, calls)
	}
}
func TestSearchSupportsCursorAndRejectsRepeatedCursor(t *testing.T) {
	calls := 0
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var q map[string]any
		json.NewDecoder(r.Body).Decode(&q)
		if calls == 2 && (q["cursor"] != "next" || q["page"] != nil) {
			t.Errorf("wrong cursor request: %v", q)
		}
		io.WriteString(w, `{"assets":{"items":[{"id":"a","originalFileName":"a.jpg"}],"nextCursor":"next"}}`)
	}))
	if _, err := c.SearchOriginalFilename(context.Background(), "a.jpg"); err == nil || calls != 2 {
		t.Fatalf("repeated cursor accepted, calls=%d err=%v", calls, err)
	}
}
func TestCreateAlbumNeverRetriesServerFailure(t *testing.T) {
	calls := 0
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "lost", 500) }))
	if _, err := c.CreateReviewAlbum(context.Background(), "Trip"); err == nil || calls != 1 {
		t.Fatalf("creation retried: %d %v", calls, err)
	}
}
func TestAddAssetsChecksIndividualResults(t *testing.T) {
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"a","success":true},{"id":"b","success":false,"error":"duplicate"},{"id":"c","success":false,"error":"not_found"}]`)
	}))
	failed, err := c.AddReviewAssets(context.Background(), "album", []string{"a", "b", "c"})
	if err == nil || len(failed) != 1 || failed[0] != "c" {
		t.Fatalf("partial response treated as success: %v %v", failed, err)
	}
}
func TestAddAssetsRejectsMissingResults(t *testing.T) {
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `[]`) }))
	if _, err := c.AddReviewAssets(context.Background(), "album", []string{"a"}); err == nil {
		t.Fatal("missing results accepted")
	}
}
func TestThumbnailDoesNotExposeKeyOrAcceptHTML(t *testing.T) {
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Error("key missing")
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<script>alert(1)</script>")
	}))
	if _, _, err := c.ReviewThumbnail(context.Background(), "a"); err == nil {
		t.Fatal("HTML accepted as image")
	}
}
func TestSearchRejectsUnexpectedResponse(t *testing.T) {
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"unexpected":true}`) }))
	if _, err := c.SearchOriginalFilename(context.Background(), "a.jpg"); err == nil {
		t.Fatal("invalid response treated as missing photos")
	}
}
func TestAddReviewBatches(t *testing.T) {
	calls := 0
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/assets") {
			t.Fatal(r.URL.Path)
		}
		var q struct {
			IDs []string `json:"ids"`
		}
		json.NewDecoder(r.Body).Decode(&q)
		results := []map[string]any{}
		for _, id := range q.IDs {
			results = append(results, map[string]any{"id": id, "success": true})
		}
		json.NewEncoder(w).Encode(results)
	}))
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = strings.Repeat("a", i+1)
	}
	if _, err := c.AddReviewAssets(context.Background(), "album", ids); err != nil || calls != 3 {
		t.Fatalf("%d %v", calls, err)
	}
}
