package resolve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bjarneo/cliamp/playlist"
)

func TestEnrichSoundCloudTracks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tracks" {
			q := r.URL.Query()
			ids := q.Get("ids")
			if ids == "101,102" {
				tracks := []scTrackInfo{
					{
						ID:           101,
						Title:        "First Song",
						PermalinkURL: "https://soundcloud.com/artist1/first-song",
						Duration:     120000,
						User: struct {
							Username string `json:"username"`
						}{Username: "Artist One"},
					},
					{
						ID:           102,
						Title:        "Second Song",
						PermalinkURL: "https://soundcloud.com/artist2/second-song",
						Duration:     180000,
						User: struct {
							Username string `json:"username"`
						}{Username: "Artist Two"},
					},
				}
				_ = json.NewEncoder(w).Encode(tracks)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	// Override client ID
	scClientIDMu.Lock()
	oldID := scClientID
	scClientID = "test_client_id_1234567890123456"
	scClientIDMu.Unlock()
	t.Cleanup(func() {
		scClientIDMu.Lock()
		scClientID = oldID
		scClientIDMu.Unlock()
	})

	// Override scAPIBase
	oldAPIBase := scAPIBase
	scAPIBase = server.URL
	t.Cleanup(func() {
		scAPIBase = oldAPIBase
	})

	// Override httpClient
	oldClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() {
		httpClient = oldClient
	})

	tracks := []playlist.Track{
		{
			Path:   "https://api-v2.soundcloud.com/tracks/101",
			Title:  "101",
			Artist: "PlaylistCreator",
		},
		{
			Path:   "https://api-v2.soundcloud.com/tracks/102",
			Title:  "102",
			Artist: "PlaylistCreator",
		},
		{
			Path:   "https://soundcloud.com/regular/track",
			Title:  "Regular Track",
			Artist: "Regular Artist",
		},
	}

	enrichSoundCloudTracks(context.Background(), tracks)

	if got, want := tracks[0].Title, "First Song"; got != want {
		t.Errorf("tracks[0].Title = %q, want %q", got, want)
	}
	if got, want := tracks[0].Artist, "Artist One"; got != want {
		t.Errorf("tracks[0].Artist = %q, want %q", got, want)
	}
	if got, want := tracks[0].Path, "https://soundcloud.com/artist1/first-song"; got != want {
		t.Errorf("tracks[0].Path = %q, want %q", got, want)
	}
	if got, want := tracks[0].DurationSecs, 120; got != want {
		t.Errorf("tracks[0].DurationSecs = %d, want %d", got, want)
	}

	if got, want := tracks[1].Title, "Second Song"; got != want {
		t.Errorf("tracks[1].Title = %q, want %q", got, want)
	}
	if got, want := tracks[1].Artist, "Artist Two"; got != want {
		t.Errorf("tracks[1].Artist = %q, want %q", got, want)
	}

	// Verify regular track unchanged
	if got, want := tracks[2].Title, "Regular Track"; got != want {
		t.Errorf("tracks[2].Title = %q, want %q", got, want)
	}
}

func TestParseYTDLSoundCloudAPIURL(t *testing.T) {
	input := `{"url":"https://api-v2.soundcloud.com/tracks/101","webpage_url":"https://soundcloud.com/artist/song","title":"101"}`
	tracks, entries, err := parseYTDLTracks(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseYTDLTracks() error: %v", err)
	}
	if entries != 1 || len(tracks) != 1 {
		t.Fatalf("parseYTDLTracks() = %d entries, %d tracks; want 1 each", entries, len(tracks))
	}
	if got, want := tracks[0].Path, "https://api-v2.soundcloud.com/tracks/101"; got != want {
		t.Errorf("track path = %q, want %q", got, want)
	}
}
