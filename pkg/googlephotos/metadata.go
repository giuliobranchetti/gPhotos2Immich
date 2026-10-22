package googlephotos

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// ValidateAlbumURL permits only public Google Photos album links.
func ValidateAlbumURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return fmt.Errorf("use an HTTPS Google Photos shared album link")
	}
	if u.Hostname() == "photos.app.goo.gl" && len(strings.Trim(u.Path, "/")) > 0 {
		return nil
	}
	if u.Hostname() == "photos.google.com" && strings.HasPrefix(u.Path, "/share/") {
		return nil
	}
	return fmt.Errorf("use a photos.app.goo.gl or photos.google.com/share album link")
}

func allowedGoogleURL(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := u.Hostname()
	return host == "photos.google.com" || host == "photos.app.goo.gl" || strings.HasSuffix(host, ".googleusercontent.com")
}

func PreviewURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !allowedGoogleURL(u) || !strings.HasSuffix(u.Hostname(), ".googleusercontent.com") {
		return ""
	}
	return raw + "=w320-h240"
}

// NewMetadataClient fails closed on incomplete albums and unexpected redirects.
func NewMetadataClient(logger *slog.Logger) *Client {
	c := NewClient(logger, 4)
	c.requireComplete = true
	c.client.Timeout = 30 * time.Second
	c.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !allowedGoogleURL(req.URL) {
			return fmt.Errorf("unsupported Google Photos redirect")
		}
		return nil
	}
	return c
}

func (c *Client) OriginalFilename(ctx context.Context, photo Photo) (string, error) {
	u, err := url.Parse(photo.URL)
	if err != nil || !allowedGoogleURL(u) || !strings.HasSuffix(u.Hostname(), ".googleusercontent.com") {
		return "", fmt.Errorf("unsupported media URL")
	}
	suffix := "=d"
	if photo.IsVideo {
		suffix = "=dv"
	}
	resp, err := c.Head(ctx, photo.URL+suffix)
	if err != nil {
		return "", fmt.Errorf("filename request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("filename request returned HTTP %d", resp.StatusCode)
	}
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil {
		return "", fmt.Errorf("original filename is unavailable")
	}
	name := params["filename"]
	if name == "" || name != path.Base(name) || strings.ContainsAny(name, "\\\x00\r\n") {
		return "", fmt.Errorf("original filename is unavailable or invalid")
	}
	return name, nil
}

// ReadMetadata fetches album metadata and HEAD headers only, never media bodies.
func (c *Client) ReadMetadata(ctx context.Context, raw string) (*Album, error) {
	if err := ValidateAlbumURL(raw); err != nil {
		return nil, err
	}
	album, err := ScrapeAlbum(ctx, c, raw)
	if err != nil {
		return nil, fmt.Errorf("cannot read complete shared album: %w", err)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				name, err := c.OriginalFilename(ctx, album.Photos[i])
				album.Photos[i].OriginalFileName = name
				if err != nil {
					album.Photos[i].FilenameError = err.Error()
				}
			}
		}()
	}
	for i := range album.Photos {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return album, nil
}
