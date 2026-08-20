package main

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// Track is metadata for a single playable item.
type Track struct {
	URL      string
	Title    string
	Artist   string
	Duration string
}

// Song pairs a Track with the Discord IDs needed to play it.
type Song struct {
	guildID   string
	channelID string
	track     Track
}

// SearchResult is the JSON shape returned by the Python search sidecar.
type SearchResult struct {
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	VideoID  string `json:"videoId"`
	Duration string `json:"duration"`
}

type guildAction func()

type audioPipeline struct {
	name        string
	ytdlpFormat string
	ffmpegArgs  []string
}
