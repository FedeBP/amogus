package main

// Discord music bot streams audio from YouTube via yt-dlp and ffmpeg.
func main() {
	GetConfig()
	Start()
	<-make(chan struct{}) // block forever; bot runs on goroutines/handlers
}
