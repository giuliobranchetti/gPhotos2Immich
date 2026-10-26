package immich

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ReviewAsset struct {
	ID               string    `json:"id"`
	Type             string    `json:"type"`
	OriginalFileName string    `json:"originalFileName"`
	FileCreatedAt    time.Time `json:"fileCreatedAt"`
	ExifInfo         *struct {
		DateTimeOriginal *time.Time `json:"dateTimeOriginal"`
	} `json:"exifInfo"`
}

func (a ReviewAsset) TakenAt() time.Time {
	if a.ExifInfo != nil && a.ExifInfo.DateTimeOriginal != nil {
		return *a.ExifInfo.DateTimeOriginal
	}
	// Immich's fileCreatedAt is the capture/file timestamp, not uploaded-at createdAt.
	return a.FileCreatedAt
}

// SearchOriginalFilename deliberately does not filter by date. Immich's filename
// search is a pattern search, so results are checked for exact equality locally.
func (c *Client) SearchOriginalFilename(ctx context.Context, name string) ([]ReviewAsset, error) {
	return c.searchReviewAssets(ctx, func(page int, cursor string) map[string]any {
		query := map[string]any{"originalFileName": name, "size": 1000, "withExif": true, "withStacked": true}
		if cursor != "" {
			delete(query, "originalFileName")
			query["filter"] = map[string]any{"originalFileName": map[string]string{"eq": name}}
			query["cursor"] = cursor
		} else {
			query["page"] = page
		}
		return query
	}, func(a ReviewAsset) bool {
		return a.OriginalFileName == name
	})
}

// SearchTakenRange returns capture-time alternatives, including both boundaries.
// Keep filters on every page, including Immich 3.2+'s cursor-based pages.
func (c *Client) SearchTakenRange(ctx context.Context, after, before time.Time, isVideo bool) ([]ReviewAsset, error) {
	if after.IsZero() || before.IsZero() || after.After(before) {
		return nil, fmt.Errorf("invalid capture date range")
	}
	kind := "IMAGE"
	if isVideo {
		kind = "VIDEO"
	}
	from, to := after.UTC().Format(time.RFC3339Nano), before.UTC().Format(time.RFC3339Nano)
	return c.searchReviewAssets(ctx, func(page int, cursor string) map[string]any {
		query := map[string]any{"size": 1000, "withExif": true, "withStacked": true}
		if cursor != "" {
			query["cursor"] = cursor
			query["filter"] = map[string]any{
				"takenAt": map[string]string{"gte": from, "lte": to},
				"type":    map[string]string{"eq": kind},
			}
		} else {
			query["page"] = page
			query["takenAfter"], query["takenBefore"], query["type"] = from, to, kind
		}
		return query
	}, func(a ReviewAsset) bool {
		taken := a.TakenAt()
		return a.Type == kind && !taken.IsZero() && !taken.Before(after) && !taken.After(before)
	})
}

// SearchReviewAlbumAssets reads every page, including Immich 3.2+ cursors.
// Recent Immich versions no longer include assets in GET /albums/{id}.
func (c *Client) SearchReviewAlbumAssets(ctx context.Context, albumID string) ([]ReviewAsset, error) {
	if albumID == "" {
		return nil, fmt.Errorf("missing destination album ID")
	}
	return c.searchReviewAssets(ctx, func(page int, cursor string) map[string]any {
		query := map[string]any{"size": 1000, "withStacked": true}
		if cursor != "" {
			query["cursor"] = cursor
			query["filter"] = map[string]any{"albumIds": map[string]any{"any": []string{albumID}}}
		} else {
			query["page"] = page
			query["albumIds"] = []string{albumID}
		}
		return query
	}, func(ReviewAsset) bool { return true })
}

func (c *Client) searchReviewAssets(ctx context.Context, queryForPage func(int, string) map[string]any, matches func(ReviewAsset) bool) ([]ReviewAsset, error) {
	result := []ReviewAsset{}
	seen := map[string]bool{}
	seenPages := map[string]bool{}
	page := 1
	cursor := ""
	for {
		payload, _ := json.Marshal(queryForPage(page, cursor))
		body, err := c.request(ctx, http.MethodPost, "search/metadata", payload, "")
		if err != nil {
			return nil, fmt.Errorf("Immich search failed: %w", err)
		}
		var response struct {
			Assets *struct {
				Items      []ReviewAsset   `json:"items"`
				NextPage   json.RawMessage `json:"nextPage"`
				NextCursor string          `json:"nextCursor"`
			} `json:"assets"`
		}
		if err := json.Unmarshal(body, &response); err != nil || response.Assets == nil {
			return nil, fmt.Errorf("unexpected Immich search response")
		}
		for _, a := range response.Assets.Items {
			if a.ID != "" && matches(a) && !seen[a.ID] {
				seen[a.ID] = true
				result = append(result, a)
			}
		}
		next := strings.Trim(string(response.Assets.NextPage), "\"")
		if response.Assets.NextCursor != "" {
			next = response.Assets.NextCursor
			if seenPages[next] || len(response.Assets.Items) == 0 || len(seenPages) >= 10000 {
				return nil, fmt.Errorf("Immich cursor pagination did not complete")
			}
			seenPages[next] = true
			cursor = next
			continue
		}
		if next == "" || next == "null" {
			break
		}
		if seenPages[next] || len(response.Assets.Items) == 0 || page >= 10000 {
			return nil, fmt.Errorf("Immich search pagination did not complete")
		}
		seenPages[next] = true
		var nextPage int
		if _, err := fmt.Sscanf(next, "%d", &nextPage); err != nil || nextPage <= page {
			return nil, fmt.Errorf("unsupported Immich search pagination")
		}
		page = nextPage
	}
	return result, nil
}

// CreateReviewAlbum must NEVER retry POST: a lost response may hide a created album.
func (c *Client) CreateReviewAlbum(ctx context.Context, name string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"albumName": name})
	body, _, err := c.doHTTP(ctx, http.MethodPost, "albums", strings.NewReader(string(payload)), "")
	if err != nil {
		return "", err
	}
	var album Album
	if err := json.Unmarshal(body, &album); err != nil || album.Id == "" {
		return "", fmt.Errorf("album creation returned no valid ID")
	}
	return album.Id, nil
}

// AddReviewAssets checks each result; HTTP 200 alone is not success.
func (c *Client) AddReviewAssets(ctx context.Context, albumID string, ids []string) ([]string, error) {
	failed := []string{}
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		chunk := ids[start:end]
		payload, _ := json.Marshal(map[string]any{"ids": chunk})
		body, err := c.request(ctx, http.MethodPut, "albums/"+url.PathEscape(albumID)+"/assets", payload, "")
		if err != nil {
			return append(failed, ids[start:]...), fmt.Errorf("adding photos failed: %w", err)
		}
		var results []struct {
			ID      string `json:"id"`
			Success bool   `json:"success"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(body, &results); err != nil {
			return append(failed, ids[start:]...), fmt.Errorf("invalid add-assets response")
		}
		ok := map[string]bool{}
		for _, r := range results {
			if r.Success || strings.EqualFold(r.Error, "duplicate") {
				ok[r.ID] = true
			}
		}
		for _, id := range chunk {
			if !ok[id] {
				failed = append(failed, id)
			}
		}
	}
	if len(failed) > 0 {
		return failed, fmt.Errorf("%d selected photos were not added", len(failed))
	}
	return failed, nil
}

type ThumbnailError struct {
	StatusCode            int
	MissingViewPermission bool
}

func (e *ThumbnailError) Error() string {
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return "Immich rejected the API key (HTTP 401). Check the Immich connection."
	case http.StatusForbidden:
		if e.MissingViewPermission {
			return "The Immich API key is missing the asset.view permission required for previews. Enable it in the API key settings and reload the page."
		}
		return "Immich denied access to this photo (HTTP 403). Check the API key permissions and access to the photo."
	case http.StatusNotFound:
		return "Thumbnail unavailable in Immich (HTTP 404)."
	default:
		return fmt.Sprintf("Immich did not return a valid thumbnail (HTTP %d).", e.StatusCode)
	}
}

func (c *Client) ReviewThumbnail(ctx context.Context, id string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIURL+"/assets/"+url.PathEscape(id)+"/thumbnail?size=thumbnail", nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("x-api-key", c.APIKey)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK {
		failure := &ThumbnailError{StatusCode: resp.StatusCode}
		if resp.StatusCode == http.StatusForbidden {
			var response struct {
				Message string `json:"message"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&response) == nil && response.Message == "Missing required permission: asset.view" {
				failure.MissingViewPermission = true
			}
		}
		return nil, "", failure
	}
	if !(strings.HasPrefix(ct, "image/jpeg") || strings.HasPrefix(ct, "image/png") || strings.HasPrefix(ct, "image/webp") || strings.HasPrefix(ct, "image/avif")) {
		return nil, "", &ThumbnailError{StatusCode: http.StatusBadGateway}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024+1))
	if len(data) > 10*1024*1024 {
		return nil, "", fmt.Errorf("thumbnail too large")
	}
	return data, ct, err
}
