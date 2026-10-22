// album-probe verifies metadata extraction without connecting to Immich.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"log/slog"
	"os"
	"time"

	"warreth.dev/gphotos2immich/pkg/googlephotos"
)

func main() {
	raw := flag.String("url", "", "Public Google Photos album URL")
	flag.Parse()
	if *raw == "" {
		log.Fatal("use -url with a public shared album")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	album, err := googlephotos.NewMetadataClient(slog.Default()).ReadMetadata(ctx, *raw)
	if err != nil {
		log.Fatal(err)
	}
	type item struct {
		Filename         string    `json:"filename"`
		TakenAt          time.Time `json:"takenAt"`
		UTCOffsetSeconds *int      `json:"utcOffsetSeconds"`
		IsVideo          bool      `json:"isVideo"`
		Error            string    `json:"error,omitempty"`
	}
	items := []item{}
	for _, p := range album.Photos {
		items = append(items, item{p.OriginalFileName, p.TakenAt, p.UTCOffsetSeconds, p.IsVideo, p.FilenameError})
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(map[string]any{"title": album.Title, "count": len(items), "items": items}); err != nil {
		log.Fatal(err)
	}
}
