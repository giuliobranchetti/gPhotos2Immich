package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/googlephotos"
	"warreth.dev/gphotos2immich/pkg/immich"
	"warreth.dev/gphotos2immich/pkg/review"
)

func replyJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func reviewError(w http.ResponseWriter, status int, err error) {
	replyJSON(w, status, map[string]string{"error": err.Error()})
}
func decodeReview(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("a single JSON object is required")
	}
	return nil
}
func (s *Server) reviewConnection() (*immich.Client, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := config.ReadConfig(s.configPath)
	if err != nil || cfg.ApiKey == "" || cfg.ApiURL == "" {
		return nil, "", fmt.Errorf("configure the Immich connection first")
	}
	u, err := url.Parse(cfg.ApiURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, "", fmt.Errorf("invalid Immich API URL")
	}
	sum := sha256.Sum256([]byte(strings.TrimRight(cfg.ApiURL, "/") + "\x00" + cfg.ApiKey))
	return immich.NewClient(cfg.ApiURL, cfg.ApiKey, 4), hex.EncodeToString(sum[:]), nil
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		reviewError(w, 405, fmt.Errorf("method not allowed"))
		return
	}
	if r.Method == http.MethodPost {
		origin := r.Header.Get("Origin")
		if origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				reviewError(w, 403, fmt.Errorf("cross-origin request refused"))
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			reviewError(w, 403, fmt.Errorf("same-origin JSON request required"))
			return
		}
	}
	if r.URL.Path == "/api/review/config" {
		s.handleReviewConfig(w, r)
		return
	}
	client, target, err := s.reviewConnection()
	if err != nil {
		reviewError(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	switch r.URL.Path {
	case "/api/review/album-membership":
		if r.Method != http.MethodPost {
			reviewError(w, 405, fmt.Errorf("POST required"))
			return
		}
		var request struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			AlbumID string `json:"albumId"`
		}
		if err := decodeReview(w, r, &request); err != nil {
			reviewError(w, 400, err)
			return
		}
		membershipCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
		defer stop()
		ids, err := s.reviews.AlbumMembership(membershipCtx, request.ID, target, request.Title, request.AlbumID, client)
		if err != nil {
			reviewError(w, 502, fmt.Errorf("could not check photos already in the album: %w", err))
			return
		}
		replyJSON(w, 200, map[string]any{"assetIds": ids})
	case "/api/review/albums":
		if r.Method != http.MethodPost {
			reviewError(w, 405, fmt.Errorf("POST required"))
			return
		}
		var request struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		if err := decodeReview(w, r, &request); err != nil {
			reviewError(w, 400, err)
			return
		}
		checkCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		defer stop()
		albums, err := s.reviews.CheckAlbums(checkCtx, request.ID, target, request.Title, client)
		if err != nil {
			reviewError(w, 502, fmt.Errorf("could not check existing albums: %w", err))
			return
		}
		replyJSON(w, 200, map[string]any{"albums": albums})
	case "/api/review/preview":
		if r.Method != http.MethodPost {
			reviewError(w, 405, fmt.Errorf("POST required"))
			return
		}
		var request struct {
			URL string `json:"url"`
		}
		if err := decodeReview(w, r, &request); err != nil {
			reviewError(w, 400, err)
			return
		}
		readAlbum := s.readAlbum
		if readAlbum == nil {
			gp := googlephotos.NewMetadataClient(slog.New(slog.NewTextHandler(os.Stderr, nil)))
			readAlbum = gp.ReadMetadata
		}
		album, err := readAlbum(ctx, strings.TrimSpace(request.URL))
		if err != nil {
			reviewError(w, 502, fmt.Errorf("Google Photos album could not be read completely. Check the public link and try again."))
			return
		}
		session, err := s.reviews.Preview(ctx, album, client, target)
		if err != nil {
			reviewError(w, 502, fmt.Errorf("preview failed: %w", err))
			return
		}
		replyJSON(w, 200, session)
	case "/api/review/session":
		if r.Method != http.MethodGet {
			reviewError(w, 405, fmt.Errorf("GET required"))
			return
		}
		session, err := s.reviews.Get(r.URL.Query().Get("id"), target)
		if err != nil {
			reviewError(w, 404, err)
			return
		}
		replyJSON(w, 200, session)
	case "/api/review/cancel":
		if r.Method != http.MethodPost {
			reviewError(w, 405, fmt.Errorf("POST required"))
			return
		}
		var request struct {
			ID string `json:"id"`
		}
		if err := decodeReview(w, r, &request); err != nil {
			reviewError(w, 400, err)
			return
		}
		session, err := s.reviews.Cancel(request.ID, target)
		if err != nil {
			reviewError(w, 409, err)
			return
		}
		replyJSON(w, 200, session)
	case "/api/review/confirm":
		if r.Method != http.MethodPost {
			reviewError(w, 405, fmt.Errorf("POST required"))
			return
		}
		var request struct {
			ID              string            `json:"id"`
			Title           string            `json:"title"`
			Selected        map[string]string `json:"selected"`
			ExistingAlbumID string            `json:"existingAlbumId"`
		}
		if err := decodeReview(w, r, &request); err != nil {
			reviewError(w, 400, err)
			return
		}
		// Finish journalling even if the browser disconnects after confirmation.
		commitCtx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
		defer stop()
		session, err := s.reviews.Confirm(commitCtx, request.ID, target, request.Title, request.Selected, request.ExistingAlbumID, client)
		if err != nil {
			response := map[string]any{"error": err.Error(), "session": session}
			var existing *review.ExistingAlbumError
			if errors.As(err, &existing) {
				response["code"] = "album_exists"
				response["albums"] = existing.Albums
			}
			replyJSON(w, 409, response)
			return
		}
		replyJSON(w, 200, session)
	case "/api/review/thumbnail":
		if r.Method != http.MethodGet {
			reviewError(w, 405, fmt.Errorf("GET required"))
			return
		}
		session, err := s.reviews.Get(r.URL.Query().Get("session"), target)
		if err != nil {
			reviewError(w, 404, err)
			return
		}
		id := r.URL.Query().Get("asset")
		allowed := false
		for _, item := range session.Items {
			for _, candidate := range item.Candidates {
				if candidate.ID == id {
					allowed = true
				}
			}
		}
		if !allowed {
			reviewError(w, 404, fmt.Errorf("photo outside this preview"))
			return
		}
		data, ct, err := client.ReviewThumbnail(ctx, id)
		if err != nil {
			status := http.StatusBadGateway
			var thumbnailError *immich.ThumbnailError
			if errors.As(err, &thumbnailError) && thumbnailError.StatusCode >= 400 && thumbnailError.StatusCode <= 599 {
				status = thumbnailError.StatusCode
			}
			reviewError(w, status, err)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=120")
		w.Header().Set("Content-Type", ct)
		w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleReviewConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodGet {
		cfg, err := config.ReadConfig(s.configPath)
		if err != nil {
			replyJSON(w, 200, map[string]any{"apiURL": "", "configured": false})
			return
		}
		replyJSON(w, 200, map[string]any{"apiURL": cfg.ApiURL, "configured": cfg.ApiKey != "" && cfg.ApiURL != "", "managedByEnvironment": os.Getenv("IMMICH_API_KEY") != "" || os.Getenv("IMMICH_API_URL") != ""})
		return
	}
	var request struct {
		APIURL string `json:"apiURL"`
		APIKey string `json:"apiKey"`
	}
	if err := decodeReview(w, r, &request); err != nil {
		reviewError(w, 400, err)
		return
	}
	u, err := url.Parse(strings.TrimSpace(request.APIURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		reviewError(w, 400, fmt.Errorf("enter a valid Immich URL, including /api"))
		return
	}
	if os.Getenv("IMMICH_API_KEY") != "" || os.Getenv("IMMICH_API_URL") != "" {
		reviewError(w, 400, fmt.Errorf("Immich settings are managed by environment variables"))
		return
	}
	values := map[string]any{}
	data, err := os.ReadFile(s.configPath)
	if err == nil {
		if json.Unmarshal(data, &values) != nil || values == nil {
			reviewError(w, 500, fmt.Errorf("existing configuration is invalid; it was not overwritten"))
			return
		}
	} else if !os.IsNotExist(err) {
		reviewError(w, 500, err)
		return
	}
	values["apiURL"] = strings.TrimRight(u.String(), "/")
	if key := strings.TrimSpace(request.APIKey); key != "" {
		values["apiKey"] = key
	}
	if key, _ := values["apiKey"].(string); key == "" {
		reviewError(w, 400, fmt.Errorf("enter an Immich API key"))
		return
	}
	output, _ := json.MarshalIndent(values, "", "  ")
	if err := os.WriteFile(s.configPath, output, 0600); err != nil {
		reviewError(w, 500, fmt.Errorf("cannot save connection settings"))
		return
	}
	replyJSON(w, 200, map[string]bool{"configured": true})
}
