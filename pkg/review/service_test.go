package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	albumErr                error
	albumReads              int
	albumAssets             map[string][]immich.ReviewAsset
	membershipErr           error
	membershipReads         int
	dateAssets              []immich.ReviewAsset
	dateSearches            int
	dateErr                 error
	dateAfter, dateBefore   time.Time
	dateIsVideo             bool
	addedTo                 []string
	onAdd                   func(string, []string)
}

func (l *library) SearchTakenRange(_ context.Context, after, before time.Time, isVideo bool) ([]immich.ReviewAsset, error) {
	l.dateSearches++
	l.dateAfter, l.dateBefore, l.dateIsVideo = after, before, isVideo
	return l.dateAssets, l.dateErr
}

func (l *library) SearchOriginalFilename(_ context.Context, n string) ([]immich.ReviewAsset, error) {
	l.searches++
	return l.assets[n], nil
}
func (l *library) GetAlbums(context.Context) ([]immich.Album, error) {
	l.albumReads++
	return l.albums, l.albumErr
}
func (l *library) SearchReviewAlbumAssets(_ context.Context, albumID string) ([]immich.ReviewAsset, error) {
	l.membershipReads++
	return l.albumAssets[albumID], l.membershipErr
}
func (l *library) CreateReviewAlbum(context.Context, string) (string, error) {
	l.creates++
	return "album-1", l.createErr
}
func (l *library) AddReviewAssets(_ context.Context, albumID string, ids []string) ([]string, error) {
	l.adds++
	l.addedTo = append(l.addedTo, albumID)
	if l.onAdd != nil {
		l.onAdd(albumID, ids)
	}
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

func TestPreviewSuggestsNearbyDatesOnlyAfterFilenameMiss(t *testing.T) {
	source := time.Date(2020, 1, 2, 0, 30, 0, 0, time.FixedZone("UTC+2", 7200))
	l := &library{
		assets: map[string][]immich.ReviewAsset{
			"exact.jpg": {asset("filename-match", "exact.jpg", source.AddDate(-1, 0, 0))},
			// Partial filename matches must not prevent the date fallback.
			"missing.jpg": {asset("wrong-name", "other.jpg", source)},
		},
		dateAssets: []immich.ReviewAsset{
			asset("upper", "upper.jpg", source.Add(2*time.Hour)),
			asset("outside", "outside.jpg", source.Add(2*time.Hour+time.Nanosecond)),
			asset("unknown", "unknown.jpg", time.Time{}),
			asset("near", "near.jpg", source.Add(30*time.Minute)),
			asset("lower", "lower.jpg", source.Add(-2*time.Hour)),
			asset("exact", "renamed.jpg", source.UTC()),
			asset("near", "near.jpg", source.Add(30*time.Minute)),
			asset("", "empty-id.jpg", source),
		},
	}
	s := New(t.TempDir())
	photos := []googlephotos.Photo{
		{ID: "filename", OriginalFileName: "exact.jpg", TakenAt: source},
		{ID: "missing", OriginalFileName: "missing.jpg", TakenAt: source},
		{ID: "same-time", OriginalFileName: "another-missing.jpg", TakenAt: source.UTC()},
		{ID: "no-time", OriginalFileName: "no-time.jpg"},
		{ID: "no-name", TakenAt: source},
	}
	p, err := s.Preview(context.Background(), &googlephotos.Album{Title: "Trip", Photos: photos}, l, "target")
	if err != nil {
		t.Fatal(err)
	}
	if l.dateSearches != 1 || !l.dateAfter.Equal(source.Add(-2*time.Hour)) || !l.dateBefore.Equal(source.Add(2*time.Hour)) || l.dateIsVideo {
		t.Fatalf("unexpected date searches: %+v", l)
	}
	if p.Items[0].Status != "date_mismatch" || p.Items[0].Candidates[0].ID != "filename-match" {
		t.Fatal("filename match was replaced with date suggestions")
	}
	for _, i := range []int{1, 2} {
		item := p.Items[i]
		ids := []string{}
		for _, c := range item.Candidates {
			ids = append(ids, c.ID)
			if c.DateStatus != "nearby_date" {
				t.Fatalf("unmarked date alternative: %+v", c)
			}
		}
		if item.Status != "date_suggestions" || item.Warning == "" || !reflect.DeepEqual(ids, []string{"exact", "near", "lower", "upper"}) {
			t.Fatalf("incorrect alternatives: %+v", item)
		}
	}
	if p.Items[3].Status != "missing" || p.Items[4].Status != "filename_unavailable" {
		t.Fatal("searched without a source date or filename")
	}
	if l.creates != 0 || l.adds != 0 {
		t.Fatal("date preview wrote to Immich")
	}
	// The alternatives are persisted and can be selected after a restart.
	p, err = New(s.Dir).Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"missing": "near"}, "", l)
	if err != nil || p.Status != "complete" || l.creates != 1 || l.adds != 1 {
		t.Fatalf("date alternative could not be confirmed: %+v %v", p, err)
	}
}

func TestPreviewDateFallbackDistinguishesVideosAndFailsOnSearchError(t *testing.T) {
	for _, isVideo := range []bool{false, true} {
		t.Run(fmt.Sprint(isVideo), func(t *testing.T) {
			l := &library{dateErr: errors.New("date search unavailable")}
			p, err := New(t.TempDir()).Preview(context.Background(), &googlephotos.Album{Photos: []googlephotos.Photo{{ID: "missing", OriginalFileName: "missing", TakenAt: taken, IsVideo: isVideo}}}, l, "target")
			if err == nil || !strings.Contains(err.Error(), "date search unavailable") || p != nil || l.dateSearches != 1 || l.dateIsVideo != isVideo {
				t.Fatalf("incorrect fallback failure: %+v %v", p, err)
			}
		})
	}
}
func TestConfirmSurvivesRestartAndRepeatedClicks(t *testing.T) {
	s, p, l := prepare(t)
	selected := map[string]string{"g1": "a"}
	p, err := s.Confirm(context.Background(), p.ID, "target", "Trip", selected, "", l)
	if err != nil || p.Status != "complete" {
		t.Fatalf("%+v %v", p, err)
	}
	s = New(s.Dir)
	p, err = s.Confirm(context.Background(), p.ID, "target", "Trip", selected, "", l)
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
			if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "", l); err != nil {
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
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "", l); err == nil {
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
		if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "", l); err == nil {
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
	if _, err := New(s.Dir).Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "", l); err == nil {
		t.Fatal("interrupted creation retried")
	}
	if l.creates != 0 {
		t.Fatal("created an album")
	}
}
func TestPartialRetryUsesSameAlbum(t *testing.T) {
	s, p, l := prepare(t)
	l.addErr = errors.New("asset unavailable")
	p, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "", l)
	if err == nil || p.Status != "partial" || p.AlbumID != "album-1" {
		t.Fatalf("%+v %v", p, err)
	}
	l.addErr = nil
	p, err = New(s.Dir).Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "", l)
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
		if _, err := s.Confirm(context.Background(), p.ID, tc.target, "Trip", tc.selection, "", l); err == nil {
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
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "", l); err == nil {
		t.Fatal("expired accepted")
	}
	p.CreatedAt = time.Now()
	s.save(p)
	l.albums = []immich.Album{{AlbumName: "Trip", Id: "existing"}}
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "", l); err == nil {
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
	_, err = s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a", "g2": "a"}, "", l)
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
	s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "", l)
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Changed", map[string]string{"g1": "a"}, "", l); err == nil {
		t.Fatal("changed title accepted after creation")
	}
}

func TestCheckAlbumsBeforeConfirmationIsReadOnly(t *testing.T) {
	s, p, l := prepare(t)
	count := 79
	l.albums = []immich.Album{
		{Id: "first", AlbumName: "Trip", AssetCount: &count},
		{Id: "first", AlbumName: "Trip"},
		{Id: "second", AlbumName: "Trip"},
		{Id: "other", AlbumName: "Other"},
	}
	journal := filepath.Join(s.Dir, p.ID+".json")
	before, _ := os.ReadFile(journal)
	albums, err := s.CheckAlbums(context.Background(), p.ID, "target", "  Trip  ", l)
	if err != nil || len(albums) != 2 || albums[0].ID != "first" || albums[1].ID != "second" || albums[0].AssetCount == nil || *albums[0].AssetCount != count {
		t.Fatalf("existing albums not detected before confirmation: %+v %v", albums, err)
	}
	albums, err = s.CheckAlbums(context.Background(), p.ID, "target", "Other", l)
	if err != nil || len(albums) != 1 || albums[0].ID != "other" {
		t.Fatalf("changed title not checked: %+v %v", albums, err)
	}
	albums, err = s.CheckAlbums(context.Background(), p.ID, "target", "New trip", l)
	if err != nil || albums == nil || len(albums) != 0 {
		t.Fatalf("unused title did not return an empty list: %+v %v", albums, err)
	}
	after, _ := os.ReadFile(journal)
	if string(before) != string(after) || l.creates != 0 || l.adds != 0 {
		t.Fatal("checking destinations changed the preview or Immich")
	}
	l.albumErr = errors.New("album access denied")
	if _, err := s.CheckAlbums(context.Background(), p.ID, "target", "Trip", l); !errors.Is(err, l.albumErr) {
		t.Fatalf("album lookup failure was hidden: %v", err)
	}
	l.albumErr = nil
	l.albums = []immich.Album{{AlbumName: "Trip"}}
	if _, err := s.CheckAlbums(context.Background(), p.ID, "target", "Trip", l); err == nil {
		t.Fatal("album without ID offered as a destination")
	}
}

func TestCheckAlbumsRejectsUnavailablePreviews(t *testing.T) {
	for _, kind := range []string{"foreign", "expired", "cancelled", "adding", "invalid_title"} {
		t.Run(kind, func(t *testing.T) {
			s, p, l := prepare(t)
			target, title := "target", "Trip"
			switch kind {
			case "foreign":
				target = "other"
			case "expired":
				p.CreatedAt = time.Now().Add(-25 * time.Hour)
			case "cancelled", "adding":
				p.Status = kind
			case "invalid_title":
				title = " "
			}
			if err := s.save(p); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CheckAlbums(context.Background(), p.ID, target, title, l); err == nil || l.albumReads != 0 {
				t.Fatalf("invalid check queried Immich or succeeded: %v, reads=%d", err, l.albumReads)
			}
		})
	}
}

func TestAlbumMembershipIsScopedToDestinationAndPreview(t *testing.T) {
	s, p, l := prepare(t)
	p.Items[0].Candidates = append(p.Items[0].Candidates, Candidate{ID: "b", Filename: "a.jpg"})
	if err := s.save(p); err != nil {
		t.Fatal(err)
	}
	l.albums = []immich.Album{{Id: "first", AlbumName: "Trip"}, {Id: "second", AlbumName: "Trip"}}
	l.albumAssets = map[string][]immich.ReviewAsset{
		"first":  {{ID: "a"}, {ID: "unrelated"}, {ID: "a"}, {}},
		"second": {{ID: "b"}},
	}
	journal := filepath.Join(s.Dir, p.ID+".json")
	before, _ := os.ReadFile(journal)
	for _, tc := range []struct{ album, asset string }{{"first", "a"}, {"second", "b"}} {
		ids, err := s.AlbumMembership(context.Background(), p.ID, "target", "Trip", tc.album, l)
		if err != nil || !reflect.DeepEqual(ids, []string{tc.asset}) {
			t.Fatalf("membership leaked or mixed album contents: %v %v", ids, err)
		}
	}
	for _, tc := range []struct{ target, title, album string }{
		{"target", "Trip", "foreign"}, {"other", "Trip", "first"}, {"target", "Renamed", "first"},
	} {
		if _, err := s.AlbumMembership(context.Background(), p.ID, tc.target, tc.title, tc.album, l); err == nil || l.membershipReads != 2 {
			t.Fatalf("invalid destination fetched membership: %+v %v", tc, err)
		}
	}
	l.membershipErr = errors.New("membership access denied")
	ids, err := s.AlbumMembership(context.Background(), p.ID, "target", "Trip", "first", l)
	if !errors.Is(err, l.membershipErr) || ids != nil {
		t.Fatalf("failed lookup treated as an empty album: %v %v", ids, err)
	}
	after, _ := os.ReadFile(journal)
	if string(before) != string(after) || l.creates != 0 || l.adds != 0 {
		t.Fatal("reading membership changed the preview or Immich")
	}
}

func TestExistingAlbumRequiresExplicitChoiceAndNeverCreates(t *testing.T) {
	s, p, l := prepare(t)
	count := 79
	created := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	l.albums = []immich.Album{
		{Id: "first", AlbumName: "Trip", AssetCount: &count, CreatedAt: &created},
		{Id: "second", AlbumName: "Trip"},
		{Id: "unrelated", AlbumName: "Other"},
	}
	selected := map[string]string{"g1": "a"}
	result, err := s.Confirm(context.Background(), p.ID, "target", "Trip", selected, "", l)
	var conflict *ExistingAlbumError
	if !errors.As(err, &conflict) || result.Status != "ready" || len(conflict.Albums) != 2 || l.creates != 0 || l.adds != 0 {
		t.Fatalf("existing album was not offered without writes: %+v %v", result, err)
	}
	if conflict.Albums[0].ID != "first" || conflict.Albums[0].AssetCount == nil || *conflict.Albums[0].AssetCount != 79 || !conflict.Albums[0].CreatedAt.Equal(created) {
		t.Fatalf("missing album choice details: %+v", conflict.Albums)
	}
	for _, id := range []string{"unrelated", "foreign", "deleted"} {
		if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", selected, id, l); err == nil || l.creates != 0 || l.adds != 0 {
			t.Fatalf("unavailable destination accepted: %q %v", id, err)
		}
	}
	result, err = s.Confirm(context.Background(), p.ID, "target", "Trip", selected, "second", l)
	if err != nil || result.Status != "complete" || !result.ExistingAlbum || result.AlbumID != "second" || l.creates != 0 || l.adds != 1 || l.addedTo[0] != "second" {
		t.Fatalf("explicit album update failed: %+v %v", result, err)
	}
	if _, err := New(s.Dir).Confirm(context.Background(), p.ID, "target", "Trip", selected, "", l); err != nil || l.adds != 1 {
		t.Fatalf("replay repeated the operation: %v adds=%d", err, l.adds)
	}
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", selected, "first", l); err == nil || l.adds != 1 {
		t.Fatal("completed operation changed its destination")
	}
}

func TestExistingAlbumDestinationIsJournalledBeforeAddingAndRetriedAfterRestart(t *testing.T) {
	s, p, l := prepare(t)
	l.albums = []immich.Album{{Id: "existing", AlbumName: "Trip"}, {Id: "another", AlbumName: "Trip"}}
	l.addErr = errors.New("connection interrupted")
	l.onAdd = func(albumID string, ids []string) {
		data, err := os.ReadFile(filepath.Join(s.Dir, p.ID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var saved Session
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if (saved.Status != "adding" && saved.Status != "partial") || (l.adds == 1 && saved.Status != "adding") || !saved.ExistingAlbum || saved.AlbumID != "existing" || albumID != "existing" || saved.Selected["g1"] != "a" {
			t.Fatalf("destination was not saved before writing: %+v", saved)
		}
	}
	selected := map[string]string{"g1": "a"}
	result, err := s.Confirm(context.Background(), p.ID, "target", "Trip", selected, "existing", l)
	if err == nil || result.Status != "partial" || !result.ExistingAlbum || l.creates != 0 || len(result.FailedIDs) != 1 {
		t.Fatalf("partial existing album update was not retained: %+v %v", result, err)
	}
	restart := New(s.Dir)
	for _, attempt := range []struct {
		title, id string
		selected  map[string]string
	}{
		{"Trip", "another", selected},
		{"Changed", "existing", selected},
		{"Trip", "existing", map[string]string{}},
	} {
		if _, err := restart.Confirm(context.Background(), p.ID, "target", attempt.title, attempt.selected, attempt.id, l); err == nil || l.adds != 1 {
			t.Fatal("partial operation changed destination, title or selection")
		}
	}
	l.addErr = nil
	result, err = restart.Confirm(context.Background(), p.ID, "target", "Trip", selected, "existing", l)
	if err != nil || result.Status != "complete" || !result.ExistingAlbum || l.creates != 0 || l.adds != 2 || !reflect.DeepEqual(l.addedTo, []string{"existing", "existing"}) {
		t.Fatalf("existing album retry changed destination: %+v %v", result, err)
	}
}

func TestConcurrentExistingAlbumConfirmAddsOnce(t *testing.T) {
	s, p, l := prepare(t)
	l.albums = []immich.Album{{Id: "existing", AlbumName: "Trip"}}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "existing", l); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if l.creates != 0 || l.adds != 1 {
		t.Fatalf("concurrent updates repeated writes: creates=%d adds=%d", l.creates, l.adds)
	}
}

func TestExistingAlbumCannotBypassExpiredPreviewOrAttachToStartedNewAlbum(t *testing.T) {
	s, p, l := prepare(t)
	l.albums = []immich.Album{{Id: "existing", AlbumName: "Trip"}}
	p.CreatedAt = time.Now().Add(-25 * time.Hour)
	if err := s.save(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "existing", l); err == nil || l.adds != 0 {
		t.Fatal("expired preview updated an album")
	}
	s, p, l = prepare(t)
	l.addErr = errors.New("partial add")
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "", l); err == nil {
		t.Fatal("expected partial creation")
	}
	if _, err := s.Confirm(context.Background(), p.ID, "target", "Trip", map[string]string{"g1": "a"}, "existing", l); err == nil || l.creates != 1 || l.adds != 1 {
		t.Fatal("started creation switched to an existing album")
	}
}
