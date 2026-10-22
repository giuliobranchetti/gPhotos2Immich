package googlephotos

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOriginalFilenameUsesHEADOnly(t *testing.T) {
	for _, tc := range []struct {
		header, want string
		video        bool
	}{{`attachment;filename="example-photo.JPG"`, "example-photo.JPG", false}, {`attachment; filename*=UTF-8''vacanza%20%C3%A8.jpg`, "vacanza è.jpg", false}, {`attachment;filename="clip.MOV"`, "clip.MOV", true}} {
		calls := 0
		client := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method != "HEAD" {
				t.Error("media body requested")
			}
			suffix := "=d"
			if tc.video {
				suffix = "=dv"
			}
			if !strings.HasSuffix(r.URL.Path, suffix) {
				t.Errorf("wrong download variant %s", r.URL.Path)
			}
			w.Header().Set("Content-Disposition", tc.header)
		}))
		name, err := client.OriginalFilename(context.Background(), Photo{URL: "https://lh3.googleusercontent.com/photo", IsVideo: tc.video})
		if err != nil || name != tc.want || calls != 1 {
			t.Fatalf("%q %v calls=%d", name, err, calls)
		}
	}
}
func TestMissingAndUnsafeFilenameRemainUnmatched(t *testing.T) {
	for _, header := range []string{"", `attachment;filename="../secret.jpg"`, `attachment;filename="C:\\secret.jpg"`} {
		c := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Disposition", header) }))
		if _, err := c.OriginalFilename(context.Background(), Photo{URL: "https://lh3.googleusercontent.com/photo"}); err == nil {
			t.Fatalf("unsafe filename accepted: %s", header)
		}
	}
}
func TestRejectUnexpectedAlbumAndMediaHosts(t *testing.T) {
	for _, raw := range []string{"http://photos.app.goo.gl/test", "https://localhost/album", "https://photos.google.com.evil.test/share/a", "https://photos.app.goo.gl:443/a", "https://user@photos.app.goo.gl/a"} {
		if ValidateAlbumURL(raw) == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	c := NewMetadataClient(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.OriginalFilename(context.Background(), Photo{URL: "http://127.0.0.1/private"}); err == nil {
		t.Fatal("unsafe media URL accepted")
	}
	req := httptest.NewRequest("GET", "https://evil.test", nil)
	if c.client.CheckRedirect(req, nil) == nil {
		t.Fatal("unsafe redirect accepted")
	}
}
func albumPage(token string, items []any) string {
	data, _ := json.Marshal([]any{nil, items, token, []any{"album-key"}})
	return `<meta property="og:title" content="Fixture · Dates">` + `<script>AF_initDataCallback({key: 'ds:1',data:` + string(data) + `});</script>`
}
func fakeItem(id string) []any {
	return []any{id, []any{"https://lh3.googleusercontent.com/" + id, 100, 80}, float64(1577934245000), nil, float64(7200000)}
}
func TestMetadataPreviewFailsClosedOnIncompletePagination(t *testing.T) {
	c := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			http.Error(w, "pagination unavailable", 400)
			return
		}
		io.WriteString(w, albumPage("next", []any{fakeItem("a")}))
	}))
	c.requireComplete = true
	if _, err := ScrapeAlbum(context.Background(), c, "https://photos.google.com/share/album-key"); err == nil {
		t.Fatal("incomplete album accepted")
	}
}
func TestMetadataReadsNamesAndPreservesMissingHeaders(t *testing.T) {
	c := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			if strings.Contains(r.URL.Path, "a=d") {
				w.Header().Set("Content-Disposition", `attachment;filename="a.jpg"`)
			}
			return
		}
		io.WriteString(w, albumPage("", []any{fakeItem("a"), fakeItem("b")}))
	}))
	c.requireComplete = true
	album, err := c.ReadMetadata(context.Background(), "https://photos.google.com/share/album-key")
	if err != nil {
		t.Fatal(err)
	}
	if album.Title != "Fixture" || len(album.Photos) != 2 || album.Photos[0].OriginalFileName != "a.jpg" || album.Photos[1].FilenameError == "" {
		t.Fatalf("unexpected metadata: %+v", album)
	}
	if album.Photos[0].UTCOffsetSeconds == nil || *album.Photos[0].UTCOffsetSeconds != 7200 {
		t.Fatal("timezone missing")
	}
}
func TestTakenTimeDoesNotFallBackToUpload(t *testing.T) {
	if !extractTimestamp([]any{"id", nil, nil, nil, nil, float64(1700000000000)}).IsZero() {
		t.Fatal("upload date used as capture date")
	}
	old := time.Date(1980, 1, 2, 0, 0, 0, 0, time.UTC)
	got := extractTimestamp([]any{"id", nil, float64(old.UnixMilli())})
	if !got.Equal(old) {
		t.Fatalf("old capture date lost: %s", got)
	}
}

func TestStructuredTitleAndCountMismatch(t *testing.T) {
	for _, count := range []int{1, 2} {
		meta := make([]any, 22)
		meta[0] = "album-key"
		meta[1] = "Family · Summer"
		meta[21] = count
		data, _ := json.Marshal([]any{nil, []any{fakeItem("a")}, "", meta})
		page := `<script>AF_initDataCallback({key: 'ds:1',data:` + string(data) + `});</script>`
		c := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, page) }))
		c.requireComplete = true
		album, err := ScrapeAlbum(context.Background(), c, "https://photos.google.com/share/album-key")
		if count == 2 {
			if err == nil {
				t.Fatal("declared count mismatch accepted")
			}
			continue
		}
		if err != nil || album.Title != "Family · Summer" {
			t.Fatalf("structured title lost: %+v %v", album, err)
		}
	}
}

func TestPaginationCollectsAllPages(t *testing.T) {
	c := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			payload, _ := json.Marshal([]any{nil, []any{fakeItem("b")}, ""})
			envelope, _ := json.Marshal([]any{[]any{"wrb.fr", "snAcKc", string(payload)}})
			io.WriteString(w, string(envelope))
			return
		}
		io.WriteString(w, albumPage("next", []any{fakeItem("a")}))
	}))
	c.requireComplete = true
	album, err := ScrapeAlbum(context.Background(), c, "https://photos.google.com/share/album-key")
	if err != nil || len(album.Photos) != 2 {
		t.Fatalf("pagination incomplete: %+v %v", album, err)
	}
}

// This optional test uses an explicit environment URL, never a private fixture.
func TestIntegrationMetadataHeaders(t *testing.T) {
	raw := os.Getenv("GP2IMMICH_METADATA_URL")
	if raw == "" {
		t.Skip("set GP2IMMICH_METADATA_URL to test a public shared album")
	}
	c := NewMetadataClient(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	album, err := c.ReadMetadata(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(album.Photos) == 0 {
		t.Fatal("empty test album")
	}
	for _, p := range album.Photos {
		if p.OriginalFileName == "" {
			t.Errorf("missing filename: %s", p.FilenameError)
		}
	}
}
