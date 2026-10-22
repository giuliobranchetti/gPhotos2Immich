package review

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"warreth.dev/gphotos2immich/pkg/googlephotos"
	"warreth.dev/gphotos2immich/pkg/immich"
)

type library struct {
	assets                  map[string][]immich.ReviewAsset
	albums                  []immich.Album
	creates, adds, searches int
	createErr, addErr       error
}

func (l *library) SearchOriginalFilename(_ context.Context, n string) ([]immich.ReviewAsset, error) {
	l.searches++
	return l.assets[n], nil
}
func (l *library) GetAlbums(context.Context) ([]immich.Album, error) { return l.albums, nil }
func (l *library) CreateReviewAlbum(context.Context, string) (string, error) {
	l.creates++
	return "album-1", l.createErr
}
func (l *library) AddReviewAssets(_ context.Context, _ string, ids []string) ([]string, error) {
	l.adds++
	if l.addErr != nil {
		return ids, l.addErr
	}
	return nil, nil
}

var taken = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

func asset(id, name string, t time.Time) immich.ReviewAsset {
	return immich.ReviewAsset{ID: id, OriginalFileName: name, FileCreatedAt: t}
}
func prepare(t *testing.T) (*Service, *Session, *library) {
	t.Helper()
	s := New(t.TempDir())
	l := &library{assets: map[string][]immich.ReviewAsset{"a.jpg": {asset("a", "a.jpg", taken)}}}
	p, e := s.Preview(context.Background(), &googlephotos.Album{Title: "Trip", Photos: []googlephotos.Photo{{ID: "g1", OriginalFileName: "a.jpg", TakenAt: taken}}}, l, "target")
	if e != nil {
		t.Fatal(e)
	}
	return s, p, l
}

func TestPreviewClassificationsAndReadOnly(t *testing.T) {
	l := &library{assets: map[string][]immich.ReviewAsset{
		"exact.jpg":   {asset("1", "exact.jpg", taken.In(time.FixedZone("Rome", 7200)))},
		"wrong.jpg":   {asset("2", "wrong.jpg", taken.Add(time.Hour))},
		"amb.jpg":     {asset("3", "amb.jpg", taken), asset("4", "amb.jpg", taken.AddDate(1, 0, 0))},
		"unknown.jpg": {asset("5", "unknown.jpg", time.Time{})},
	}}
	photos := []googlephotos.Photo{}
	names := []string{"exact.jpg", "wrong.jpg", "amb.jpg", "missing.jpg", "unknown.jpg", ""}
	want := []string{"matched", "date_mismatch", "ambiguous", "missing", "unknown_date", "filename_unavailable"}
	for i, n := range names {
		photos = append(photos, googlephotos.Photo{ID: fmt.Sprint(i), OriginalFileName: n, TakenAt: taken})
	}
	p, err := New(t.TempDir()).Preview(context.Background(), &googlephotos.Album{Title: "Test", Photos: photos}, l, "target")
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range p.Items {
		if item.Status != want[i] {
			t.Errorf("%s: got %s, want %s", names[i], item.Status, want[i])
		}
	}
	if l.creates != 0 || l.adds != 0 {
		t.Fatal("preview wrote to Immich")
	}
	if len(p.Items[1].Candidates) != 1 {
		t.Fatal("mismatched date candidate was hidden")
	}
}
func TestConfirmSurvivesRestartAndRepeatedClicks(t *testing.T) {
	s, p, l := prepare(t)
	selected := map[string]string{"g1": "a"}
	p, err := s.Confirm(context.Background(), p.ID, "target", "Trip", selected, l)
	if err != nil || p.Status != "complete" {
		t.Fatalf("%+v %v", p, err)
	}
	s = New(s.Dir)
	p, err = s.Confirm(context.Background(), p.ID, "target", "Trip", selected, l)
	if err != nil || p.Status != "complete" || l.creates != 1 || l.adds != 1 {
		t.Fatalf("duplicate creation or replay failure: %+v %v creates=%d adds=%d", p, err, l.creates, l.adds)
	}
}
func TestConcurrentConfirmCreatesOnce(t *testing.T) {
	s, p, l := prepare(t)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, l); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if l.creates != 1 || l.adds != 1 {
		t.Fatal("double submission created duplicates")
	}
}
func TestCancelHasNoWrites(t *testing.T) {
	s, p, l := prepare(t)
	if _, err := s.Cancel(p.ID, "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, l); err == nil {
		t.Fatal("cancelled preview confirmed")
	}
	if l.creates != 0 || l.adds != 0 {
		t.Fatal("cancel wrote")
	}
}
func TestLostCreateResponseNeverRetries(t *testing.T) {
	s, p, l := prepare(t)
	l.createErr = errors.New("response lost")
	for i := 0; i < 2; i++ {
		if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, l); err == nil {
			t.Fatal("expected uncertain creation error")
		}
		s = New(s.Dir)
	}
	if l.creates != 1 || l.adds != 0 {
		t.Fatal("uncertain creation was retried")
	}
}
func TestInterruptedCreatingJournalBlocksRetry(t *testing.T) {
	s, p, l := prepare(t)
	p.Status = "creating"
	if err := s.save(p); err != nil {
		t.Fatal(err)
	}
	if _, err := New(s.Dir).Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, l); err == nil {
		t.Fatal("interrupted creation retried")
	}
	if l.creates != 0 {
		t.Fatal("created an album")
	}
}
func TestPartialRetryUsesSameAlbum(t *testing.T) {
	s, p, l := prepare(t)
	l.addErr = errors.New("asset unavailable")
	p, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, l)
	if err == nil || p.Status != "partial" || p.AlbumID != "album-1" {
		t.Fatalf("%+v %v", p, err)
	}
	l.addErr = nil
	p, err = New(s.Dir).Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, l)
	if err != nil || p.Status != "complete" || l.creates != 1 || l.adds != 2 {
		t.Fatalf("%+v %v", p, err)
	}
}
func TestRejectInvalidSelectionAndChangedTarget(t *testing.T) {
	for _, tc := range []struct {
		target    string
		selection map[string]string
	}{{"other", map[string]string{"g1": "a"}}, {"target", map[string]string{"g1": "alien"}}, {"target", map[string]string{"other": "a"}}, {"target", nil}} {
		s, p, l := prepare(t)
		if _, err := s.Confirm(context.Background(), p.ID, tc.target, "Trip", tc.selection, l); err == nil {
			t.Fatal("invalid confirm accepted")
		}
		if l.creates != 0 {
			t.Fatal("invalid confirm created album")
		}
	}
}
func TestRejectExpiredAndExistingAlbum(t *testing.T) {
	s, p, l := prepare(t)
	p.CreatedAt = time.Now().Add(-25 * time.Hour)
	s.save(p)
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, l); err == nil {
		t.Fatal("expired accepted")
	}
	p.CreatedAt = time.Now()
	s.save(p)
	l.albums = []immich.Album{{AlbumName: "Trip", Id: "existing"}}
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, l); err == nil {
		t.Fatal("duplicate album title accepted")
	}
	if l.creates != 0 {
		t.Fatal("created duplicate")
	}
}
func TestMultipleGoogleItemsReuseOneAsset(t *testing.T) {
	s, _, l := prepare(t)
	p, err := s.Preview(context.Background(), &googlephotos.Album{Title: "Trip", Photos: []googlephotos.Photo{{ID: "g1", OriginalFileName: "a.jpg", TakenAt: taken}, {ID: "g2", OriginalFileName: "a.jpg", TakenAt: taken}}}, l, "target")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a", "g2": "a"}, l)
	if err != nil {
		t.Fatal(err)
	}
}
func TestTraversalAndChangedSelection(t *testing.T) {
	s, p, l := prepare(t)
	if _, err := s.Get("../config", "target"); err == nil {
		t.Fatal("traversal accepted")
	}
	l.addErr = errors.New("partial")
	s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, l)
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Changed", map[string]string{"g1": "a"}, l); err == nil {
		t.Fatal("changed title accepted after creation")
	}
}
