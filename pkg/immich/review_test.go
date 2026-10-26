package immich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestSearchTakenRangeKeepsBoundsAndMediaTypeOnEveryPage(t *testing.T) {
	for _, cursor := range []bool{false, true} {
		for _, isVideo := range []bool{false, true} {
			t.Run(fmt.Sprintf("cursor=%t/video=%t", cursor, isVideo), func(t *testing.T) {
				after := time.Date(2024, 1, 1, 23, 0, 0, 123000000, time.FixedZone("UTC+2", 7200))
				before := after.Add(4 * time.Hour)
				from, to := after.UTC().Format(time.RFC3339Nano), before.UTC().Format(time.RFC3339Nano)
				kind, otherKind := "IMAGE", "VIDEO"
				if isVideo {
					kind, otherKind = otherKind, kind
				}
				calls := 0
				c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					var q map[string]any
					if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
						t.Fatal(err)
					}
					if q["originalFileName"] != nil || q["createdAfter"] != nil || q["withExif"] != true || q["withStacked"] != true {
						t.Fatalf("wrong capture date query: %v", q)
					}
					if cursor && calls == 2 {
						filter := q["filter"].(map[string]any)
						rangeFilter := filter["takenAt"].(map[string]any)
						if q["cursor"] != "next" || q["page"] != nil || rangeFilter["gte"] != from || rangeFilter["lte"] != to || filter["type"].(map[string]any)["eq"] != kind {
							t.Fatalf("cursor lost filters: %v", q)
						}
					} else if q["takenAfter"] != from || q["takenBefore"] != to || q["type"] != kind || q["page"] != float64(calls) {
						t.Fatalf("page lost filters: %v", q)
					}
					asset := func(id string, date time.Time, typ string) map[string]any {
						return map[string]any{"id": id, "originalFileName": id + ".jpg", "fileCreatedAt": date, "type": typ}
					}
					response := map[string]any{"items": []any{
						asset("upper", before, kind), asset("lower", after, kind),
						asset("too-late", before.Add(time.Nanosecond), kind),
						asset("too-early", after.Add(-time.Nanosecond), kind),
						asset("other-media", after, otherKind), asset("unknown-date", time.Time{}, kind),
						asset("", after, kind),
					}}
					if calls == 1 {
						if cursor {
							response["nextCursor"] = "next"
						} else {
							response["nextPage"] = "2"
						}
					} else {
						response["items"] = []any{asset("lower", after, kind), asset("middle", after.Add(time.Hour), kind)}
					}
					json.NewEncoder(w).Encode(map[string]any{"assets": response})
				}))
				assets, err := c.SearchTakenRange(context.Background(), after, before, isVideo)
				if err != nil || calls != 2 || len(assets) != 3 {
					t.Fatalf("unexpected date results: %+v %v calls=%d", assets, err, calls)
				}
				if assets[0].ID != "upper" || assets[1].ID != "lower" || assets[2].ID != "middle" {
					t.Fatalf("invalid or duplicate date candidates: %+v", assets)
				}
			})
		}
	}
}

func TestSearchTakenRangeRejectsInvalidRange(t *testing.T) {
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("invalid date range made a request")
	}))
	now := time.Now()
	for _, span := range [][2]time.Time{{{}, now}, {now, {}}, {now, now.Add(-time.Hour)}} {
		if _, err := c.SearchTakenRange(context.Background(), span[0], span[1], false); err == nil {
			t.Fatal("invalid date range accepted")
		}
	}
}
func TestSearchReviewAlbumAssetsPaginatesWithAlbumFilter(t *testing.T) {
	for _, cursor := range []bool{false, true} {
		t.Run(fmt.Sprintf("cursor=%t", cursor), func(t *testing.T) {
			calls := 0
			c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var q map[string]any
				json.NewDecoder(r.Body).Decode(&q)
				if r.Method != "POST" || r.URL.Path != "/api/search/metadata" || q["withStacked"] != true || q["size"] != float64(1000) || q["originalFileName"] != nil || q["takenAfter"] != nil {
					t.Fatalf("wrong membership query: %v", q)
				}
				if cursor && calls == 2 {
					filter := q["filter"].(map[string]any)["albumIds"].(map[string]any)["any"].([]any)
					if q["cursor"] != "next" || q["page"] != nil || len(filter) != 1 || filter[0] != "album-1" {
						t.Fatalf("cursor lost the destination album: %v", q)
					}
				} else {
					ids := q["albumIds"].([]any)
					if len(ids) != 1 || ids[0] != "album-1" || q["page"] != float64(calls) {
						t.Fatalf("page lost the destination album: %v", q)
					}
				}
				next := ""
				if calls == 1 {
					if cursor {
						next = `,"nextCursor":"next"`
					} else {
						next = `,"nextPage":"2"`
					}
				}
				fmt.Fprintf(w, `{"assets":{"items":[{"id":"a"},{"id":"page-%d"},{"id":""}]%s}}`, calls, next)
			}))
			assets, err := c.SearchReviewAlbumAssets(context.Background(), "album-1")
			if err != nil || len(assets) != 3 || calls != 2 || assets[2].ID != "page-2" {
				t.Fatalf("incomplete or duplicated membership: %+v %v calls=%d", assets, err, calls)
			}
		})
	}
}

func TestSearchReviewAlbumAssetsRejectsIncompleteResults(t *testing.T) {
	calls := 0
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			io.WriteString(w, `{"assets":{"items":[{"id":"a"}],"nextPage":"2"}}`)
		} else {
			io.WriteString(w, `{"error":"album not accessible"}`)
		}
	}))
	assets, err := c.SearchReviewAlbumAssets(context.Background(), "album-1")
	if err == nil || assets != nil || calls != 2 {
		t.Fatalf("partial membership treated as complete: %+v %v calls=%d", assets, err, calls)
	}
	if _, err := c.SearchReviewAlbumAssets(context.Background(), ""); err == nil || calls != 2 {
		t.Fatal("empty destination ID sent to Immich")
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

func TestThumbnailExplainsMissingViewPermission(t *testing.T) {
	c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"Missing required permission: asset.view"}`)
	}))
	_, _, err := c.ReviewThumbnail(context.Background(), "a")
	var failure *ThumbnailError
	if !errors.As(err, &failure) || failure.StatusCode != 403 || !failure.MissingViewPermission || !strings.Contains(err.Error(), "asset.view") || strings.Contains(err.Error(), "test-key") {
		t.Fatalf("unhelpful thumbnail error: %v", err)
	}
}

func TestThumbnailDistinguishesOtherAccessErrors(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		c := reviewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, `{"message":"other error"}`)
		}))
		_, _, err := c.ReviewThumbnail(context.Background(), "a")
		var failure *ThumbnailError
		if !errors.As(err, &failure) || failure.StatusCode != status || failure.MissingViewPermission {
			t.Fatalf("wrong status: %v", err)
		}
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
