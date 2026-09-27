package model

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/bjarneo/cliamp/playlist"
)

// streamPreloadLeadTime is how far before the end of a stream we arm the
// gapless next pipeline. Opening the preload HTTP connection too early can
// cause the server to close the current stream (e.g., per-user concurrent
// stream limits on Navidrome), which makes the mp3 decoder error out and
// triggers a premature gapless transition. 3 seconds is short enough that
// most servers won't enforce a concurrency limit for such a brief overlap,
// and any resulting early skip is imperceptible (≤3 s from the true end).
const streamPreloadLeadTime = 3 * time.Second

// ytdlPreloadLeadTime is the lead time used for yt-dlp (YouTube/SoundCloud)
// URLs. These need longer because spinning up the yt-dlp | ffmpeg pipe chain
// takes 3-10 seconds, so we start preloading much earlier.
const ytdlPreloadLeadTime = 15 * time.Second

// preloadTarget returns the track the gapless pipeline should hold next. It is
// false when nothing should be armed: a live stream is playing, the queue ends
// here, or the next yt-dlp track is the one already playing.
func (m *Model) preloadTarget() (playlist.Track, bool) {
	// Live streams do not have a track boundary. Preloading another station
	// would turn a transient EOF into a gapless switch instead of reconnecting
	// the station the user selected.
	current, currentIdx := m.currentPlaybackTrack()
	if currentIdx >= 0 && m.currentPlaybackIsLive(current) {
		return playlist.Track{}, false
	}
	var next playlist.Track
	var ok bool
	if m.playbackDetached {
		var idx int
		next, idx = m.playlist.Current()
		ok = idx >= 0
	} else {
		next, ok = m.playlist.PeekNext()
	}
	if !ok || (playlist.IsYTDL(next.Path) && currentIdx >= 0 && next.Path == current.Path) {
		return playlist.Track{}, false
	}
	return next, true
}

// reconcilePreload keeps the gapless pipeline in step with the queue. Update
// runs it after every message. It drops an armed or in-flight preload that is
// no longer for the track that plays next, then arms the next track when
// playback can take one. It is the only place that arms, so a queue change,
// unpausing, a seek, a new track and a gapless advance all arm here, and a
// preload that is still right is kept.
func (m *Model) reconcilePreload() tea.Cmd {
	if m.player == nil || m.playlist == nil {
		return nil
	}
	if m.preloading || m.player.HasPreload() {
		if next, ok := m.preloadTarget(); !ok || next.Path != m.preloadFor {
			m.clearPreload()
		}
	}
	// Arm only while audio plays: a preload armed while paused can go stale,
	// the current track may still be starting while buffering, only one
	// preload runs at a time, and each page of a paged load remixes the
	// upcoming order, so anything armed mid-load would be stale by the next.
	if !m.player.IsPlaying() || m.player.IsPaused() || m.buffering || m.preloading || m.tracksPaging || m.player.HasPreload() {
		return nil
	}
	return m.preloadNext()
}

// clearPreload discards any armed or in-flight preload. A preload still in
// flight completes into a no-op: the player rejects its stale generation, and
// the next arm bumps the request so its completion is ignored.
func (m *Model) clearPreload() {
	m.preloading = false
	m.preloadFor = ""
	m.preloadFailed = ""
	if m.player != nil {
		m.player.ClearPreload()
	}
}

// preloadNext arms the next track for a gapless transition, or returns nil
// when there is none, it is too early, or its preload already failed. A failed
// preload is not retried: playback falls back to a non-gapless start.
//
// HTTP streams wait until the current track is within streamPreloadLeadTime
// of its end, and are not armed at all without a known duration. An early
// connection can go stale for the whole track, or make the player skip ahead
// if the decoder signals EOF prematurely (e.g. a mis-estimated Content-Length
// from a transcoding server). yt-dlp tracks wait for ytdlPreloadLeadTime but
// are armed at once when the duration is unknown, since their pipe chain is
// slow to start. reconcilePreload retries after every message, so a deferred
// preload is armed once the window opens.
func (m *Model) preloadNext() tea.Cmd {
	next, ok := m.preloadTarget()
	if !ok || next.Path == m.preloadFailed {
		return nil
	}
	build, lead := preloadCmd, time.Duration(0)
	switch {
	case playlist.IsYTDL(next.Path):
		build, lead = preloadYTDLStreamCmd, ytdlPreloadLeadTime
	case next.Stream:
		lead = streamPreloadLeadTime
	}
	if lead > 0 {
		dur := m.player.Duration()
		if dur <= 0 && !playlist.IsYTDL(next.Path) {
			return nil
		}
		if dur > 0 && dur-m.player.Position() > lead {
			return nil
		}
	}
	// Mark in flight so no second preload starts before this one has armed,
	// and record the track so reconcilePreload can tell whether it is stale.
	m.preloading, m.preloadFor = true, next.Path
	nextDur := time.Duration(next.DurationSecs) * time.Second
	return build(m.player, next.Path, nextDur, nextRequest(&m.requests.preload), m.player.BeginPreload())
}
