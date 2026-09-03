package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const defaultYTMusicSearchBaseURL = "http://127.0.0.1:5000"

// searchCache caches YTMusic search results for 10 minutes to avoid
// redundant HTTP round-trips to the Python sidecar during autocomplete.
var (
	searchCacheMu      sync.Mutex
	searchCache        = map[string]cachedSearch{}
	trackMetadataCache = map[string]cachedTrackMetadata{}
)

type cachedSearch struct {
	results []SearchResult
	expires time.Time
}

type cachedTrackMetadata struct {
	track   Track
	expires time.Time
}

// searchHTTP is a shared HTTP client for the Python search sidecar.
// Using a single client reuses TCP connections across requests.
var searchHTTP = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

// ---------------------------------------------------------------------------
// URL helpers
// ---------------------------------------------------------------------------

// extractYouTubeVideoID parses a YouTube URL and returns the video ID,
// or an empty string if the input is not a recognised YouTube URL.
// Handles youtube.com/watch, youtu.be short links, and /shorts/ URLs.
func extractYouTubeVideoID(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimPrefix(u.Host, "www."))
	switch host {
	case "youtube.com", "m.youtube.com", "music.youtube.com":
		if strings.HasPrefix(u.Path, "/watch") {
			return u.Query().Get("v")
		}
		if strings.HasPrefix(u.Path, "/shorts/") {
			id := strings.TrimPrefix(u.Path, "/shorts/")
			if i := strings.Index(id, "/"); i >= 0 {
				id = id[:i]
			}
			return id
		}
	case "youtu.be":
		id := strings.Trim(strings.TrimPrefix(u.Path, "/"), "/")
		if i := strings.Index(id, "/"); i >= 0 {
			id = id[:i]
		}
		return id
	}
	return ""
}

func trackVideoID(t Track) string {
	return extractYouTubeVideoID(t.URL)
}

func trackFromSearchResult(r SearchResult) Track {
	return Track{
		URL:      "https://www.youtube.com/watch?v=" + r.VideoID,
		Title:    r.Title,
		Artist:   r.Artist,
		Duration: r.Duration,
	}
}

func ytMusicSidecarURL(path string, params url.Values) string {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("YTMUSIC_SEARCH_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = defaultYTMusicSearchBaseURL
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	reqURL := baseURL + path
	if len(params) > 0 {
		reqURL += "?" + params.Encode()
	}
	return reqURL
}

// playedVideoIDsLocked returns a snapshot of tracks played in the current
// voice session. The caller must hold gs.mu.
func (gs *guildState) playedVideoIDsLocked() map[string]struct{} {
	played := make(map[string]struct{}, len(gs.playedVideoIDs))
	for id := range gs.playedVideoIDs {
		played[id] = struct{}{}
	}
	return played
}

// rememberPlayedTrackLocked records an exact video ID for the current session.
// The caller must hold gs.mu.
func (gs *guildState) rememberPlayedTrackLocked(track Track) {
	id := trackVideoID(track)
	if id == "" {
		return
	}
	if gs.playedVideoIDs == nil {
		gs.playedVideoIDs = make(map[string]struct{})
	}
	gs.playedVideoIDs[id] = struct{}{}
}

// ytDownloaderPath returns the absolute path of yt-dlp (preferred) or
// youtube-dl, whichever is found first on PATH.
func ytDownloaderPath() (string, error) {
	for _, name := range []string{"yt-dlp", "youtube-dl"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("yt-dlp not found in PATH")
}

// ---------------------------------------------------------------------------
// Search / track resolution
// ---------------------------------------------------------------------------

func cacheTrackMetadataLocked(results []SearchResult, expires time.Time) {
	now := time.Now()
	pruneExpiredTrackMetadataLocked(now)
	for _, r := range results {
		if r.VideoID == "" {
			continue
		}
		trackMetadataCache[r.VideoID] = cachedTrackMetadata{
			track:   trackFromSearchResult(r),
			expires: expires,
		}
	}
	trimTrackMetadataCacheLocked(trackCacheMaxEntries)
}

func cachedSearchResultsLocked(query string, now time.Time) ([]SearchResult, bool) {
	cached, ok := searchCache[query]
	if !ok {
		return nil, false
	}
	if !now.Before(cached.expires) {
		delete(searchCache, query)
		return nil, false
	}
	return cached.results, true
}

func cacheSearchResultsLocked(query string, results []SearchResult, expires time.Time) {
	now := time.Now()
	pruneExpiredSearchCacheLocked(now)
	searchCache[query] = cachedSearch{
		results: results,
		expires: expires,
	}
	trimSearchCacheLocked(searchCacheMaxEntries)
}

func pruneExpiredSearchCacheLocked(now time.Time) {
	for query, cached := range searchCache {
		if !now.Before(cached.expires) {
			delete(searchCache, query)
		}
	}
}

func pruneExpiredTrackMetadataLocked(now time.Time) {
	for videoID, cached := range trackMetadataCache {
		if !now.Before(cached.expires) {
			delete(trackMetadataCache, videoID)
		}
	}
}

func trimSearchCacheLocked(maxEntries int) {
	for len(searchCache) > maxEntries {
		var oldestKey string
		var oldestExpires time.Time
		for query, cached := range searchCache {
			if oldestKey == "" || cached.expires.Before(oldestExpires) {
				oldestKey = query
				oldestExpires = cached.expires
			}
		}
		if oldestKey == "" {
			return
		}
		delete(searchCache, oldestKey)
	}
}

func trimTrackMetadataCacheLocked(maxEntries int) {
	for len(trackMetadataCache) > maxEntries {
		var oldestKey string
		var oldestExpires time.Time
		for videoID, cached := range trackMetadataCache {
			if oldestKey == "" || cached.expires.Before(oldestExpires) {
				oldestKey = videoID
				oldestExpires = cached.expires
			}
		}
		if oldestKey == "" {
			return
		}
		delete(trackMetadataCache, oldestKey)
	}
}

func cachedTrackByVideoID(videoID string) (Track, bool) {
	searchCacheMu.Lock()
	defer searchCacheMu.Unlock()

	now := time.Now()
	pruneExpiredTrackMetadataLocked(now)
	cached, ok := trackMetadataCache[videoID]
	if !ok {
		return Track{}, false
	}
	if !now.Before(cached.expires) {
		delete(trackMetadataCache, videoID)
		return Track{}, false
	}
	return cached.track, true
}

// resolveTrack turns a raw query string into a playable Track.
//
//   - If query is a YouTube URL, the video ID is extracted directly.
//   - Otherwise the Python YTMusic sidecar is queried and the first
//     suitable result is returned.
func resolveTrack(ctx context.Context, query string) (Track, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Track{}, errors.New("empty query")
	}

	if id := extractYouTubeVideoID(query); id != "" {
		if track, ok := cachedTrackByVideoID(id); ok {
			return track, nil
		}
		return Track{URL: "https://www.youtube.com/watch?v=" + id}, nil
	}

	results, err := ytMusicSearch(ctx, query)
	if err != nil {
		return Track{}, err
	}
	if len(results) == 0 {
		return Track{}, errors.New("no suitable results found")
	}

	r := results[0]
	return trackFromSearchResult(r), nil
}

// ytMusicSearch queries the Python search sidecar for up to 5 song results.
// Results are cached for 10 minutes, so rapid autocomplete keystrokes do not
// hammer the sidecar with duplicate requests.
func ytMusicSearch(ctx context.Context, query string) ([]SearchResult, error) {
	now := time.Now()
	searchCacheMu.Lock()
	if cached, ok := cachedSearchResultsLocked(query, now); ok {
		searchCacheMu.Unlock()
		return cached, nil
	}
	searchCacheMu.Unlock()

	reqURL := ytMusicSidecarURL("/search", url.Values{"q": []string{query}})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := searchHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("search sidecar status %s", resp.Status)
	}

	var results []SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}

	expires := time.Now().Add(searchCacheTTL)
	searchCacheMu.Lock()
	cacheSearchResultsLocked(query, results, expires)
	cacheTrackMetadataLocked(results, expires)
	searchCacheMu.Unlock()

	return results, nil
}

// ytMusicRadio asks the Python sidecar for YouTube Music radio suggestions
// based on a seed video ID.
func ytMusicRadio(ctx context.Context, videoID string) ([]SearchResult, error) {
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return nil, errors.New("empty seed video ID")
	}

	params := url.Values{}
	params.Set("videoId", videoID)

	reqURL := ytMusicSidecarURL("/radio", params)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := searchHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("radio sidecar status %s", resp.Status)
	}

	var results []SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}
	searchCacheMu.Lock()
	cacheTrackMetadataLocked(results, time.Now().Add(searchCacheTTL))
	searchCacheMu.Unlock()
	return results, nil
}

func chooseAutoplayTrack(results []SearchResult, seedID string, playedVideoIDs map[string]struct{}) (Track, error) {
	for _, r := range results {
		if r.VideoID == "" || r.VideoID == seedID {
			continue
		}
		if _, played := playedVideoIDs[r.VideoID]; played {
			continue
		}
		return trackFromSearchResult(r), nil
	}
	return Track{}, errors.New("no radio suggestions found")
}

func resolveAutoplayTrack(ctx context.Context, seed Track, playedVideoIDs map[string]struct{}) (Track, error) {
	seedID := trackVideoID(seed)
	if seedID == "" {
		return Track{}, errors.New("last track has no YouTube video ID")
	}

	results, err := ytMusicRadio(ctx, seedID)
	if err != nil {
		return Track{}, err
	}
	return chooseAutoplayTrack(results, seedID, playedVideoIDs)
}

// youtubeAutocompleteChoices returns Discord autocomplete choices for the
// given partial query string. Returns an empty slice (never nil) on error
// so Discord always receives a valid (possibly empty) autocomplete response.
func youtubeAutocompleteChoices(ctx context.Context, query string) ([]*discordgo.ApplicationCommandOptionChoice, error) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return []*discordgo.ApplicationCommandOptionChoice{}, nil
	}

	results, err := ytMusicSearch(ctx, query)
	if err != nil {
		return nil, err
	}

	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(results))
	for _, r := range results {
		if r.VideoID == "" {
			continue
		}
		label := r.Title
		if r.Artist != "" {
			label += " · " + r.Artist
		}
		if r.Duration != "" {
			label += " [" + r.Duration + "]"
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
			Name:  truncateChoiceLabel(label, discordChoiceNameMax),
			Value: "https://www.youtube.com/watch?v=" + r.VideoID,
		})
	}
	return choices, nil
}

// truncateChoiceLabel shortens s to at most max runes, appending "…" if cut.
func truncateChoiceLabel(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max-1]) + "…"
}

// ---------------------------------------------------------------------------
// Playlist support
// ---------------------------------------------------------------------------

// fetchPlaylistEnqueue resolves a YouTube playlist URL and enqueues each
// video using the YouTube Data API v3.  It pages through results in batches
// of 50, capped at maxPlaylistTracks, so playback can start before the full
// list has been fetched.
func fetchPlaylistEnqueue(ctx context.Context, s *discordgo.Session, guildID, textChannelID, voiceChannelID, playlistURL string) (int, error) {
	u, err := url.Parse(strings.TrimSpace(playlistURL))
	if err != nil {
		return 0, fmt.Errorf("invalid URL: %w", err)
	}
	playlistID := strings.TrimSpace(u.Query().Get("list"))
	if playlistID == "" {
		return 0, errors.New("no list= parameter in URL — paste the full playlist URL")
	}

	svc, err := youtube.NewService(ctx, option.WithAPIKey(APIKey))
	if err != nil {
		return 0, fmt.Errorf("youtube client: %w", err)
	}

	total := 0
	call := svc.PlaylistItems.List([]string{"contentDetails"}).
		PlaylistId(playlistID).MaxResults(50).Context(ctx)

	err = call.Pages(ctx, func(page *youtube.PlaylistItemListResponse) error {
		for _, item := range page.Items {
			if item.ContentDetails == nil || item.ContentDetails.VideoId == "" {
				continue
			}
			if total >= maxPlaylistTracks {
				return errPlaylistMaxSize
			}
			_ = enqueueTrack(s, guildID, textChannelID, voiceChannelID,
				Track{URL: "https://www.youtube.com/watch?v=" + item.ContentDetails.VideoId})
			total++
		}
		return nil
	})

	if errors.Is(err, errPlaylistMaxSize) {
		return total, errPlaylistMaxSize
	}
	return total, err
}
