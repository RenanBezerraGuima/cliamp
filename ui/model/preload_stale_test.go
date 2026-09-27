package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/bjarneo/cliamp/ipc"
	"github.com/bjarneo/cliamp/playlist"
)

// staleTestModel plays a.mp3 of a, b, c, d with b.mp3 armed as the gapless next
// track.
func staleTestModel() (Model, *playbackFakeEngine) {
	player := &playbackFakeEngine{playing: true, hasPreload: true}
	p := playlist.New()
	p.Replace([]playlist.Track{
		{Title: "A", Path: "a.mp3", DurationSecs: 180},
		{Title: "B", Path: "b.mp3", DurationSecs: 180},
		{Title: "C", Path: "c.mp3", DurationSecs: 180},
		{Title: "D", Path: "d.mp3", DurationSecs: 180},
	})
	p.SetIndex(0)
	m := Model{
		player:      player,
		playlist:    p,
		preloadFor:  "b.mp3",
		focus:       focusPlaylist,
		configSaver: &recordingConfigSaver{},
	}
	return m, player
}

func reply() chan ipc.Response { return make(chan ipc.Response, 1) }

// Whatever changes the queue, Update leaves either no preload or the one for
// the track that plays next. A stale preload is dropped once and the next arm
// targets the new next track; a preload that is still right is kept.
func TestUpdateDropsOnlyStalePreload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cursor   int
		msg      tea.Msg
		wantNext string // "" when the armed b.mp3 is still right
	}{
		{name: "plugin swap next away", msg: PluginQueueMsg{Op: "move", Index: 1, To: 3}, wantNext: "d.mp3"},
		{name: "plugin remove next", msg: PluginQueueMsg{Op: "remove", Index: 1}, wantNext: "c.mp3"},
		{name: "TUI move next down", cursor: 1, msg: tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift}, wantNext: "c.mp3"},
		{name: "TUI delete next", cursor: 1, msg: tea.KeyPressMsg{Text: "x", Code: 'x'}, wantNext: "c.mp3"},
		{name: "IPC play next", msg: ipc.QueueRequestMsg{Op: "queue.enqueue", Index: 3, Reply: reply()}, wantNext: "d.mp3"},
		{name: "IPC repeat one", msg: ipc.RepeatMsg{Name: "one", Reply: reply()}, wantNext: "a.mp3"},
		{name: "plugin append", msg: PluginQueueMsg{Op: "add_track", Track: playlist.Track{Path: "e.mp3"}}},
		{name: "plugin swap below next", msg: PluginQueueMsg{Op: "move", Index: 3, To: 2}},
		{name: "plugin remove below next", msg: PluginQueueMsg{Op: "remove", Index: 3}},
		{name: "TUI move below next", cursor: 2, msg: tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift}},
		{name: "IPC repeat all", msg: ipc.RepeatMsg{Name: "all", Reply: reply()}},
		{name: "YouTube radio batch", msg: ytdlBatchMsg{tracks: []playlist.Track{{Path: "https://www.youtube.com/watch?v=NF-kLy44Hls"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, player := staleTestModel()
			m.plCursor = tc.cursor

			next, _ := m.Update(tc.msg)
			nm := next.(Model)
			if tc.wantNext == "" {
				if player.clearPreloadCalls != 0 || !player.hasPreload || nm.preloadFor != "b.mp3" {
					t.Fatalf("still-valid preload of b.mp3 was dropped (ClearPreload calls = %d)", player.clearPreloadCalls)
				}
				return
			}
			if player.clearPreloadCalls != 1 || player.hasPreload || nm.preloading {
				t.Fatalf("stale preload of b.mp3 kept (ClearPreload calls = %d, armed = %v)", player.clearPreloadCalls, player.hasPreload)
			}
			if target, ok := nm.preloadTarget(); !ok || target.Path != tc.wantNext {
				t.Fatalf("next track = %q, %v; want %q", target.Path, ok, tc.wantNext)
			}
			cmd := nm.preloadNext()
			if cmd == nil {
				t.Fatal("preloadNext() = nil, want the new next track armed")
			}
			cmd()
			if len(player.preloadCalls) != 1 || player.preloadCalls[0] != tc.wantNext {
				t.Fatalf("preloadCalls = %v, want [%s]", player.preloadCalls, tc.wantNext)
			}
		})
	}
}

// A preload still loading in the background is dropped as well, and its
// completion no longer counts as the current request.
func TestUpdateDropsStaleInFlightPreload(t *testing.T) {
	m, player := staleTestModel()
	player.hasPreload = false
	m.preloading = true
	before := m.requests.preload

	next, _ := m.Update(PluginQueueMsg{Op: "remove", Index: 1})
	nm := next.(Model)
	if player.clearPreloadCalls != 1 || nm.preloading || nm.requests.preload == before {
		t.Fatalf("in-flight preload of b.mp3 kept (ClearPreload calls = %d, preloading = %v)", player.clearPreloadCalls, nm.preloading)
	}
}

// A preload armed by preloadNext (the tick loop's path) records its track, so
// an edit that leaves the next track alone keeps it.
func TestArmedPreloadSurvivesUnrelatedEdit(t *testing.T) {
	m, player := staleTestModel()
	player.hasPreload = false
	m.preloadFor = ""
	cmd := m.preloadNext()
	if cmd == nil {
		t.Fatal("preloadNext() = nil, want b.mp3 armed")
	}
	cmd()

	next, _ := m.Update(PluginQueueMsg{Op: "add_track", Track: playlist.Track{Path: "e.mp3"}})
	if player.clearPreloadCalls != 0 || next.(Model).preloadFor != "b.mp3" {
		t.Fatalf("armed b.mp3 dropped by an append after it (ClearPreload calls = %d)", player.clearPreloadCalls)
	}
}
