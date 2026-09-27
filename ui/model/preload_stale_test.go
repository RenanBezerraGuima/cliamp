package model

import (
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/bjarneo/cliamp/ipc"
	"github.com/bjarneo/cliamp/playlist"
)

// settle runs cmd like the program loop would, feeding preload completions
// back through Update so the model sees them. Other messages are dropped. A
// completion that arms yet another preload is a loop, and fails the test.
func settle(t *testing.T, m Model, cmd tea.Cmd, depth int) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	if depth > 3 {
		t.Fatal("preload completions keep arming new preloads")
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			m = settle(t, m, c, depth)
		}
	case streamPreloadedMsg:
		next, more := m.Update(msg)
		m = settle(t, next.(Model), more, depth+1)
	}
	return m
}

func update(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, cmd := m.Update(msg)
	return settle(t, next.(Model), cmd, 0)
}

// reconcileModel plays a.mp3 one second before its end, followed by next and
// then c.mp3, with nothing armed yet.
func reconcileModel(next playlist.Track) (Model, *playbackFakeEngine) {
	player := &playbackFakeEngine{playing: true, duration: 180 * time.Second, position: 179 * time.Second}
	p := playlist.New()
	p.Replace([]playlist.Track{
		{Title: "A", Path: "a.mp3", DurationSecs: 180},
		next,
		{Title: "C", Path: "c.mp3", DurationSecs: 180},
	})
	p.SetIndex(0)
	m := Model{
		player:      player,
		playlist:    p,
		focus:       focusPlaylist,
		configSaver: &recordingConfigSaver{},
	}
	return m, player
}

func reply() chan ipc.Response { return make(chan ipc.Response, 1) }

// Streams and yt-dlp tracks are armed only within their lead time before the
// current track ends, then on the next message once it opens. Local files are
// armed at once. Without a known duration a stream waits, while yt-dlp, slow
// to start, is armed anyway.
func TestUpdateArmsWithinLeadTime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		next     playlist.Track
		duration time.Duration
		early    time.Duration // position that is still too early, 0 if none
		inWindow time.Duration
	}{
		{name: "local", next: playlist.Track{Path: "b.mp3"}, duration: 180 * time.Second, inWindow: time.Second},
		{name: "stream", next: playlist.Track{Path: "https://example.com/b.mp3", Stream: true}, duration: 180 * time.Second, early: 170 * time.Second, inWindow: 178 * time.Second},
		{name: "yt-dlp", next: playlist.Track{Path: "https://www.youtube.com/watch?v=NF-kLy44Hls", Stream: true}, duration: 180 * time.Second, early: 160 * time.Second, inWindow: 170 * time.Second},
		{name: "yt-dlp, duration unknown", next: playlist.Track{Path: "https://www.youtube.com/watch?v=NF-kLy44Hls", Stream: true}, inWindow: time.Second},
		{name: "stream, duration unknown", next: playlist.Track{Path: "https://example.com/b.mp3", Stream: true}, early: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, player := reconcileModel(tc.next)
			player.duration = tc.duration
			if tc.early > 0 {
				player.position = tc.early
				m = update(t, m, ShowStatusMsg{})
				if len(player.preloadCalls) != 0 {
					t.Fatalf("armed %v before the lead time", player.preloadCalls)
				}
			}
			if tc.inWindow == 0 {
				return
			}
			player.position = tc.inWindow
			update(t, m, ShowStatusMsg{})
			if len(player.preloadCalls) != 1 || player.preloadCalls[0] != tc.next.Path {
				t.Fatalf("preloadCalls = %v, want %s armed", player.preloadCalls, tc.next.Path)
			}
			if viaYTDL := len(player.ytdlPreloadCalls) == 1; viaYTDL != playlist.IsYTDL(tc.next.Path) {
				t.Fatalf("armed through yt-dlp = %v, want %v", viaYTDL, playlist.IsYTDL(tc.next.Path))
			}
		})
	}
}

// A preload that fails is not retried in a loop: playback will start that
// track without gapless. A different next track is still armed.
func TestUpdateDoesNotRetryFailedPreload(t *testing.T) {
	m, player := reconcileModel(playlist.Track{Title: "B", Path: "b.mp3", DurationSecs: 180})
	player.preloadErr = errors.New("decode failed")
	m = update(t, m, ShowStatusMsg{})
	m = update(t, m, ShowStatusMsg{})
	if len(player.preloadCalls) != 1 || player.hasPreload {
		t.Fatalf("preloadCalls = %v, want one failed attempt for b.mp3", player.preloadCalls)
	}

	// A new track starting gives b.mp3 another try.
	m = update(t, m, PluginQueueMsg{Op: "jump", Index: 0})
	if len(player.preloadCalls) != 2 || player.preloadCalls[1] != "b.mp3" {
		t.Fatalf("preloadCalls = %v, want b.mp3 retried once after a new track started", player.preloadCalls)
	}

	player.preloadErr = nil
	update(t, m, PluginQueueMsg{Op: "remove", Index: 1})
	if len(player.preloadCalls) != 3 || player.preloadCalls[2] != "c.mp3" {
		t.Fatalf("preloadCalls = %v, want c.mp3 armed after the next track changed", player.preloadCalls)
	}
}

// Only one preload runs at a time: messages that arrive while it is still
// loading do not start another.
func TestUpdateStartsOnePreloadAtATime(t *testing.T) {
	m, player := reconcileModel(playlist.Track{Title: "B", Path: "b.mp3", DurationSecs: 180})
	next, first := m.Update(ShowStatusMsg{})
	next, second := next.(Model).Update(ShowStatusMsg{})
	if first == nil || second != nil || player.preloadGeneration != 1 || !next.(Model).preloading {
		t.Fatalf("started %d preloads before the first completed, want 1", player.preloadGeneration)
	}
}

// For every kind of next track, Update arms it, keeps it through an edit that
// leaves it next, and swaps it for the new next track after one that doesn't.
func TestUpdateKeepsValidAndReplacesStalePreload(t *testing.T) {
	for _, next := range []playlist.Track{
		{Title: "B", Path: "b.mp3", DurationSecs: 180},
		{Title: "B", Path: "https://example.com/b.mp3", Stream: true, DurationSecs: 180},
		{Title: "B", Path: "https://www.youtube.com/watch?v=NF-kLy44Hls", Stream: true, DurationSecs: 180},
	} {
		t.Run(next.Path, func(t *testing.T) {
			m, player := reconcileModel(next)
			m = update(t, m, PluginQueueMsg{Op: "add_track", Track: playlist.Track{Path: "d.mp3"}})
			if len(player.preloadCalls) != 1 || player.preloadCalls[0] != next.Path || m.preloading {
				t.Fatalf("preloadCalls = %v (preloading %v), want %s armed", player.preloadCalls, m.preloading, next.Path)
			}

			m = update(t, m, PluginQueueMsg{Op: "add_track", Track: playlist.Track{Path: "e.mp3"}})
			if player.clearPreloadCalls != 0 || len(player.preloadCalls) != 1 {
				t.Fatalf("append after the next track re-armed it (ClearPreload %d, preloadCalls %v)", player.clearPreloadCalls, player.preloadCalls)
			}

			m = update(t, m, PluginQueueMsg{Op: "remove", Index: 1})
			if player.clearPreloadCalls != 1 || len(player.preloadCalls) != 2 || player.preloadCalls[1] != "c.mp3" || m.preloadFor != "c.mp3" {
				t.Fatalf("after removing the next track: ClearPreload %d, preloadCalls %v; want c.mp3 armed", player.clearPreloadCalls, player.preloadCalls)
			}
		})
	}
}

// Every way of changing the next track drops the armed b.mp3 and arms the new
// next track in the same Update; changes that leave b.mp3 next keep it.
func TestUpdateDropsOnlyStalePreload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cursor   int
		msg      tea.Msg
		wantNext string // "" when the armed b.mp3 is still right
	}{
		{name: "plugin swap next away", msg: PluginQueueMsg{Op: "move", Index: 1, To: 2}, wantNext: "c.mp3"},
		{name: "TUI move next down", cursor: 1, msg: tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift}, wantNext: "c.mp3"},
		{name: "TUI delete next", cursor: 1, msg: tea.KeyPressMsg{Text: "x", Code: 'x'}, wantNext: "c.mp3"},
		{name: "TUI play next", cursor: 2, msg: tea.KeyPressMsg{Text: "a", Code: 'a'}, wantNext: "c.mp3"},
		{name: "IPC play next", msg: ipc.QueueRequestMsg{Op: "queue.enqueue", Index: 2, Reply: reply()}, wantNext: "c.mp3"},
		{name: "IPC repeat one", msg: ipc.RepeatMsg{Name: "one", Reply: reply()}, wantNext: "a.mp3"},
		{name: "plugin append", msg: PluginQueueMsg{Op: "add_track", Track: playlist.Track{Path: "e.mp3"}}},
		{name: "plugin remove below next", msg: PluginQueueMsg{Op: "remove", Index: 2}},
		{name: "IPC repeat all", msg: ipc.RepeatMsg{Name: "all", Reply: reply()}},
		{name: "YouTube radio batch", msg: ytdlBatchMsg{tracks: []playlist.Track{{Path: "https://www.youtube.com/watch?v=NF-kLy44Hls"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, player := reconcileModel(playlist.Track{Title: "B", Path: "b.mp3", DurationSecs: 180})
			m = update(t, m, ShowStatusMsg{})
			if m.preloadFor != "b.mp3" || len(player.preloadCalls) != 1 {
				t.Fatalf("setup: preloadCalls = %v, want b.mp3 armed", player.preloadCalls)
			}
			m.plCursor = tc.cursor

			m = update(t, m, tc.msg)
			if tc.wantNext == "" {
				if player.clearPreloadCalls != 0 || len(player.preloadCalls) != 1 || m.preloadFor != "b.mp3" {
					t.Fatalf("still-valid b.mp3 was dropped (ClearPreload %d, preloadCalls %v)", player.clearPreloadCalls, player.preloadCalls)
				}
				return
			}
			if player.clearPreloadCalls != 1 || len(player.preloadCalls) != 2 || player.preloadCalls[1] != tc.wantNext || m.preloadFor != tc.wantNext {
				t.Fatalf("ClearPreload %d, preloadCalls %v; want b.mp3 dropped and %s armed", player.clearPreloadCalls, player.preloadCalls, tc.wantNext)
			}
		})
	}
}

// When the queue now ends after the playing track, the armed preload is
// dropped and nothing replaces it.
func TestUpdateDropsPreloadWhenNothingPlaysNext(t *testing.T) {
	m, player := reconcileModel(playlist.Track{Title: "B", Path: "b.mp3", DurationSecs: 180})
	m = update(t, m, PluginQueueMsg{Op: "remove", Index: 2})
	m = update(t, m, PluginQueueMsg{Op: "remove", Index: 1})
	if player.clearPreloadCalls != 1 || player.hasPreload || m.preloading || len(player.preloadCalls) != 1 {
		t.Fatalf("ClearPreload %d, armed %v, preloadCalls %v; want b.mp3 dropped and nothing armed", player.clearPreloadCalls, player.hasPreload, player.preloadCalls)
	}
}

// A change made while paused drops the stale preload but arms nothing until
// playback resumes; the unpause itself then arms the new next track.
func TestUpdateArmsOnUnpauseAfterPausedEdit(t *testing.T) {
	m, player := reconcileModel(playlist.Track{Title: "B", Path: "b.mp3", DurationSecs: 180})
	m = update(t, m, ShowStatusMsg{})
	player.paused = true

	m = update(t, m, PluginQueueMsg{Op: "remove", Index: 1})
	if player.clearPreloadCalls != 1 || len(player.preloadCalls) != 1 {
		t.Fatalf("while paused: ClearPreload %d, preloadCalls %v; want b.mp3 dropped and nothing armed", player.clearPreloadCalls, player.preloadCalls)
	}

	update(t, m, tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if player.paused || len(player.preloadCalls) != 2 || player.preloadCalls[1] != "c.mp3" {
		t.Fatalf("after unpause: paused %v, preloadCalls %v; want c.mp3 armed", player.paused, player.preloadCalls)
	}
}

// A preload still loading in the background is dropped as well, and its
// completion no longer counts as the current request.
func TestUpdateDropsStaleInFlightPreload(t *testing.T) {
	m, player := reconcileModel(playlist.Track{Title: "B", Path: "b.mp3", DurationSecs: 180})
	m.preloading, m.preloadFor = true, "b.mp3"
	before := m.requests.preload

	next, _ := m.Update(PluginQueueMsg{Op: "remove", Index: 1})
	nm := next.(Model)
	if player.clearPreloadCalls != 1 || nm.preloadFor != "c.mp3" || nm.requests.preload == before {
		t.Fatalf("in-flight b.mp3 kept (ClearPreload %d, preloadFor %q)", player.clearPreloadCalls, nm.preloadFor)
	}
	// The stale completion must not clear the flag of the new preload.
	stale, _ := nm.Update(streamPreloadedMsg{path: "b.mp3", gen: before})
	if !stale.(Model).preloading {
		t.Fatal("the dropped preload's completion cleared the new one's in-flight flag")
	}
}
