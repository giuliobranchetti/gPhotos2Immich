// Package review reconstructs albums using existing assets only. Its interfaces
// deliberately expose no upload, delete or asset-metadata mutation operations.
package review

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"warreth.dev/gphotos2immich/pkg/googlephotos"
	"warreth.dev/gphotos2immich/pkg/immich"
)

type Library interface {
	SearchOriginalFilename(context.Context, string) ([]immich.ReviewAsset, error)
	SearchTakenRange(context.Context, time.Time, time.Time, bool) ([]immich.ReviewAsset, error)
	GetAlbums(context.Context) ([]immich.Album, error)
	SearchReviewAlbumAssets(context.Context, string) ([]immich.ReviewAsset, error)
	CreateReviewAlbum(context.Context, string) (string, error)
	AddReviewAssets(context.Context, string, []string) ([]string, error)
}

const dateFallbackTolerance = 2 * time.Hour

type AlbumOption struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	AssetCount *int       `json:"assetCount,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
}

type ExistingAlbumError struct {
	Albums []AlbumOption
}

func (e *ExistingAlbumError) Error() string {
	return "An album with this title already exists. Choose an existing album to add the selected photos, or use a different title."
}

type Candidate struct {
	ID         string     `json:"id"`
	Filename   string     `json:"filename"`
	TakenAt    *time.Time `json:"takenAt"`
	DateStatus string     `json:"dateStatus"`
}
type Item struct {
	ID               string      `json:"id"`
	Filename         string      `json:"filename"`
	TakenAt          *time.Time  `json:"takenAt"`
	UTCOffsetSeconds *int        `json:"utcOffsetSeconds"`
	Thumbnail        string      `json:"thumbnail"`
	IsVideo          bool        `json:"isVideo"`
	Status           string      `json:"status"`
	Warning          string      `json:"warning,omitempty"`
	Candidates       []Candidate `json:"candidates"`
}
type Session struct {
	ID            string            `json:"id"`
	Title         string            `json:"title"`
	CreatedAt     time.Time         `json:"createdAt"`
	Target        string            `json:"target"`
	Status        string            `json:"status"`
	Items         []Item            `json:"items"`
	AlbumID       string            `json:"albumId,omitempty"`
	ExistingAlbum bool              `json:"existingAlbum,omitempty"`
	Selected      map[string]string `json:"selected,omitempty"`
	FailedIDs     []string          `json:"failedIds,omitempty"`
	Message       string            `json:"message,omitempty"`
}
type Service struct {
	Dir string
	mu  sync.Mutex
}

func New(dir string) *Service { return &Service{Dir: dir} }
func instant(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}
func dateStatus(source, candidate time.Time) string {
	if source.IsZero() || candidate.IsZero() {
		return "unknown_date"
	}
	delta := source.Sub(candidate)
	if delta < 0 {
		delta = -delta
	}
	if delta <= time.Second {
		return "matched"
	}
	return "date_mismatch"
}

func (s *Service) Preview(ctx context.Context, album *googlephotos.Album, client Library, target string) (*Session, error) {
	id := make([]byte, 24)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	session := &Session{ID: hex.EncodeToString(id), Title: album.Title, CreatedAt: time.Now().UTC(), Target: target, Status: "ready", Items: []Item{}}
	cache := map[string][]immich.ReviewAsset{}
	type dateKey struct {
		TakenAt time.Time
		IsVideo bool
	}
	dateCache := map[dateKey][]immich.ReviewAsset{}
	seen := map[string]bool{}
	for _, photo := range album.Photos {
		if photo.ID == "" || seen[photo.ID] {
			return nil, fmt.Errorf("album contains missing or duplicate identifiers")
		}
		seen[photo.ID] = true
		item := Item{ID: photo.ID, Filename: photo.OriginalFileName, TakenAt: instant(photo.TakenAt), UTCOffsetSeconds: photo.UTCOffsetSeconds, IsVideo: photo.IsVideo, Status: "missing", Candidates: []Candidate{}}
		item.Thumbnail = googlephotos.PreviewURL(photo.URL)
		if photo.OriginalFileName == "" {
			item.Status = "filename_unavailable"
			item.Warning = photo.FilenameError
			session.Items = append(session.Items, item)
			continue
		}
		assets, cached := cache[photo.OriginalFileName]
		if !cached {
			var err error
			assets, err = client.SearchOriginalFilename(ctx, photo.OriginalFileName)
			if err != nil {
				return nil, err
			}
			cache[photo.OriginalFileName] = assets
		}
		for _, asset := range assets {
			if asset.OriginalFileName != photo.OriginalFileName || asset.ID == "" {
				continue
			}
			item.Candidates = append(item.Candidates, Candidate{ID: asset.ID, Filename: asset.OriginalFileName, TakenAt: instant(asset.TakenAt()), DateStatus: dateStatus(photo.TakenAt, asset.TakenAt())})
		}
		sort.SliceStable(item.Candidates, func(i, j int) bool {
			return item.Candidates[i].DateStatus == "matched" && item.Candidates[j].DateStatus != "matched"
		})
		if len(item.Candidates) == 1 {
			item.Status = item.Candidates[0].DateStatus
		} else if len(item.Candidates) > 1 {
			item.Status = "ambiguous"
		}
		if len(item.Candidates) == 0 && !photo.TakenAt.IsZero() {
			key := dateKey{TakenAt: photo.TakenAt.UTC(), IsVideo: photo.IsVideo}
			alternatives, cached := dateCache[key]
			if !cached {
				var err error
				alternatives, err = client.SearchTakenRange(ctx, photo.TakenAt.Add(-dateFallbackTolerance), photo.TakenAt.Add(dateFallbackTolerance), photo.IsVideo)
				if err != nil {
					return nil, fmt.Errorf("Immich capture date search failed: %w", err)
				}
				dateCache[key] = alternatives
			}
			seenAssets := map[string]bool{}
			for _, asset := range alternatives {
				taken := asset.TakenAt()
				if asset.ID == "" || seenAssets[asset.ID] || taken.IsZero() || taken.Sub(photo.TakenAt).Abs() > dateFallbackTolerance {
					continue
				}
				seenAssets[asset.ID] = true
				item.Candidates = append(item.Candidates, Candidate{ID: asset.ID, Filename: asset.OriginalFileName, TakenAt: instant(taken), DateStatus: "nearby_date"})
			}
			sort.Slice(item.Candidates, func(i, j int) bool {
				di := item.Candidates[i].TakenAt.Sub(photo.TakenAt).Abs()
				dj := item.Candidates[j].TakenAt.Sub(photo.TakenAt).Abs()
				if di == dj {
					return item.Candidates[i].ID < item.Candidates[j].ID
				}
				return di < dj
			})
			if len(item.Candidates) > 0 {
				item.Status = "date_suggestions"
				item.Warning = "No exact filename match. These alternatives were taken within ±2 hours. Check the previews and select a photo manually."
			}
		}
		session.Items = append(session.Items, item)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.save(session); err != nil {
		return nil, err
	}
	return session, nil
}
func validID(id string) bool { b, err := hex.DecodeString(id); return err == nil && len(b) == 24 }
func (s *Service) save(session *Session) error {
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".review-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, filepath.Join(s.Dir, session.ID+".json"))
}
func (s *Service) load(id, target string) (*Session, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid preview identifier")
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, id+".json"))
	if err != nil {
		return nil, fmt.Errorf("preview not found")
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	if session.Target != target {
		return nil, fmt.Errorf("Immich connection changed; generate a new preview")
	}
	return &session, nil
}
func (s *Service) Get(id, target string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(id, target)
}
func (s *Service) Cancel(id, target string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.load(id, target)
	if err != nil {
		return nil, err
	}
	if session.Status != "ready" && session.Status != "cancelled" {
		return session, fmt.Errorf("the operation has already started; this preview cannot be cancelled")
	}
	session.Status = "cancelled"
	return session, s.save(session)
}

// CheckAlbums is read-only: the destination is still rechecked on confirmation.
func (s *Service) CheckAlbums(ctx context.Context, id, target, title string, client Library) ([]AlbumOption, error) {
	session, err := s.Get(id, target)
	if err != nil {
		return nil, err
	}
	if session.Status != "ready" || time.Since(session.CreatedAt) > 24*time.Hour {
		return nil, fmt.Errorf("preview is no longer ready; generate it again")
	}
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 500 {
		return nil, fmt.Errorf("enter an album title of at most 500 bytes")
	}
	return matchingAlbums(ctx, title, client)
}

// AlbumMembership exposes only candidate IDs from this preview, never unrelated
// album contents. Choosing a destination here does not start an operation.
func (s *Service) AlbumMembership(ctx context.Context, id, target, title, albumID string, client Library) ([]string, error) {
	options, err := s.CheckAlbums(ctx, id, target, title, client)
	if err != nil {
		return nil, err
	}
	available := false
	for _, option := range options {
		if option.ID == albumID {
			available = true
			break
		}
	}
	if !available {
		return nil, fmt.Errorf("the selected album is no longer available with this title; check Immich and choose again")
	}
	session, err := s.Get(id, target)
	if err != nil {
		return nil, err
	}
	assets, err := client.SearchReviewAlbumAssets(ctx, albumID)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, item := range session.Items {
		for _, candidate := range item.Candidates {
			allowed[candidate.ID] = true
		}
	}
	present := []string{}
	for _, asset := range assets {
		if allowed[asset.ID] {
			present = append(present, asset.ID)
			delete(allowed, asset.ID)
		}
	}
	sort.Strings(present)
	return present, nil
}

func matchingAlbums(ctx context.Context, title string, client Library) ([]AlbumOption, error) {
	albums, err := client.GetAlbums(ctx)
	if err != nil {
		return nil, err
	}
	matches := []AlbumOption{}
	seen := map[string]bool{}
	for _, album := range albums {
		if album.AlbumName != title {
			continue
		}
		if album.Id == "" {
			return nil, fmt.Errorf("Immich returned an album without a valid ID; check Immich before continuing")
		}
		if !seen[album.Id] {
			matches = append(matches, AlbumOption{ID: album.Id, Title: album.AlbumName, AssetCount: album.AssetCount, CreatedAt: album.CreatedAt})
			seen[album.Id] = true
		}
	}
	return matches, nil
}

func (s *Service) Confirm(ctx context.Context, id, target, title string, selected map[string]string, existingAlbumID string, client Library) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.load(id, target)
	if err != nil {
		return nil, err
	}
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 500 {
		return session, fmt.Errorf("enter an album title of at most 500 bytes")
	}
	if session.Status == "cancelled" {
		return session, fmt.Errorf("preview was cancelled")
	}
	if session.Status == "creating" || session.Status == "creation_unknown" {
		return session, fmt.Errorf("creation outcome is uncertain; check Immich before creating another album")
	}
	if session.Status != "ready" && (title != session.Title || !reflect.DeepEqual(selected, session.Selected)) {
		return session, fmt.Errorf("a started operation cannot change title or selection")
	}
	if session.Status != "ready" && existingAlbumID != "" && (!session.ExistingAlbum || existingAlbumID != session.AlbumID) {
		return session, fmt.Errorf("a started operation cannot change its destination album")
	}
	if session.Status == "complete" {
		return session, nil
	}
	if session.Status == "ready" && time.Since(session.CreatedAt) > 24*time.Hour {
		return session, fmt.Errorf("preview expired; generate it again")
	}
	allowed := map[string]map[string]bool{}
	for _, item := range session.Items {
		allowed[item.ID] = map[string]bool{}
		for _, c := range item.Candidates {
			allowed[item.ID][c.ID] = true
		}
	}
	ids := []string{}
	seen := map[string]bool{}
	for itemID, assetID := range selected {
		if !allowed[itemID][assetID] {
			return session, fmt.Errorf("selection contains a photo outside this preview")
		}
		if !seen[assetID] {
			seen[assetID] = true
			ids = append(ids, assetID)
		}
	}
	if len(ids) == 0 {
		return session, fmt.Errorf("select at least one photo")
	}
	sort.Strings(ids)
	if session.Status == "ready" {
		session.Title = title
		matches, err := matchingAlbums(ctx, title, client)
		if err != nil {
			return session, err
		}
		seenAlbums := map[string]bool{}
		for _, album := range matches {
			seenAlbums[album.ID] = true
		}
		if existingAlbumID == "" && len(matches) > 0 {
			return session, &ExistingAlbumError{Albums: matches}
		}
		if existingAlbumID != "" && !seenAlbums[existingAlbumID] {
			return session, fmt.Errorf("the selected album is no longer available with this title; check Immich and choose again")
		}
		session.Selected = selected
		if existingAlbumID == "" {
			session.Status = "creating"
			if err := s.save(session); err != nil {
				return session, err
			}
			albumID, err := client.CreateReviewAlbum(ctx, title)
			if err != nil {
				session.Status = "creation_unknown"
				session.Message = "Check Immich: the server may have created the album. No automatic retry."
				_ = s.save(session)
				return session, fmt.Errorf("album creation outcome is uncertain; check Immich")
			}
			session.AlbumID = albumID
		} else {
			session.AlbumID = existingAlbumID
			session.ExistingAlbum = true
		}
		session.Status = "adding"
		if err := s.save(session); err != nil {
			if !session.ExistingAlbum {
				return session, fmt.Errorf("album %s created, but journal could not be saved: %w", session.AlbumID, err)
			}
			return session, fmt.Errorf("cannot save the operation for album %s; no photos were added: %w", session.AlbumID, err)
		}
	}
	if session.AlbumID == "" {
		return session, fmt.Errorf("missing album ID; manual verification required")
	}
	failed, addErr := client.AddReviewAssets(ctx, session.AlbumID, ids)
	session.FailedIDs = failed
	if addErr != nil {
		session.Status = "partial"
		session.Message = "Some photos were not added. Retry to add the selected photos to the same album."
	} else {
		session.Status = "complete"
		session.Message = "The selected photos are in the album. Photos already in the album were kept."
	}
	if err := s.save(session); err != nil {
		return session, err
	}
	return session, addErr
}
