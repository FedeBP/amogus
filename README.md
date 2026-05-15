# amogus

A Discord music bot written in Go. It plays YouTube / YouTube Music audio in voice channels through slash commands, with search and autocomplete powered by a small Python YTMusic sidecar.

The audio path streams directly:

```text
yt-dlp stdout -> ffmpeg libopus OGG stdout -> OGG demux -> Discord OpusSend
```

There is no Go-side PCM decode/encode step anymore, and the project no longer depends on `layeh.com/gopus`.

Respect copyright, YouTube's Terms of Service, and your local laws. This project is for personal/educational use.

## Requirements

| Requirement | Notes |
|-------------|-------|
| Go 1.22+ | Matches `go.mod`. |
| Python 3 | Runs the local YTMusic search/radio sidecar. |
| Python packages | `flask`, `waitress`, and `ytmusicapi`. |
| yt-dlp | Downloads/extracts source audio. `youtube-dl` is accepted as a fallback. |
| ffmpeg | Must include `libopus`, which normal package-manager builds do. |
| Discord bot token | From the Discord Developer Portal. |
| YouTube Data API v3 key | Used for playlist expansion. Search/radio uses the YTMusic sidecar. |

CGO, a C compiler, and `gopus` are not required for the current audio pipeline.

## Configuration

Create `config.json` next to the executable, or run from the repo root:

```json
{
  "token": "your-discord-bot-token",
  "botPrefix": "&",
  "APIKey": "your-youtube-data-api-v3-key"
}
```

| Field | Required | Meaning |
|-------|----------|---------|
| `token` | Yes | Discord bot token. |
| `APIKey` | Yes | YouTube Data API v3 key for playlist expansion. |
| `botPrefix` | No | Legacy field; commands are slash commands now. |

Environment variables override `config.json`:

| Variable | Overrides |
|----------|-----------|
| `DISCORD_BOT_TOKEN` | Discord bot token |
| `YOUTUBE_API_KEY` | YouTube Data API v3 key |
| `BOT_PREFIX` | Legacy prefix field |

Keep secrets out of Git. `config.json` is ignored by this repo.

## Discord Setup

Invite the bot with these OAuth scopes:

| Scope | Why |
|-------|-----|
| `bot` | Lets the bot join voice and send messages. |
| `applications.commands` | Lets Discord expose slash commands. |

Useful bot permissions: View Channels, Send Messages, Connect, Speak, and Read Message History.

The bot uses guild and voice-state gateway intents. Message Content Intent is not needed for slash commands.

Some Discord voice regions require E2EE/DAVE. This repo still uses a `replace` in `go.mod` to build `discordgo` from a fork with DAVE support. When upstream ships that support, the replace can be removed.

## Local Run

Install the Python sidecar dependencies once:

```powershell
python -m pip install flask waitress ytmusicapi
```

Run the sidecar in one terminal:

```powershell
python search.py
```

Run the bot in another terminal:

```powershell
go run .
```

Build a binary:

```powershell
go build -o amogus.exe .
./amogus.exe
```

## Docker

The Docker image builds a static Go binary, installs `ffmpeg`, `yt-dlp`, and the Python search sidecar dependencies, then starts both the sidecar and the bot.

```bash
docker build -t amogus .
docker run --rm \
  -e DISCORD_BOT_TOKEN="..." \
  -e YOUTUBE_API_KEY="..." \
  amogus
```

Never bake tokens into the image. Use your hosting platform's secret manager or encrypted environment variables.

## Hosting Notes

This bot needs a long-running process for the Discord gateway and voice connection. Free web services that sleep after idle time will disconnect the bot.

Reasonable options:

| Host | Notes |
|------|-------|
| Render | Use a paid Background Worker. Free web services sleep. |
| Oracle Cloud Free Tier | See [docs/oracle-cloud.md](docs/oracle-cloud.md). |
| Fly.io / Railway / other PaaS | Use Docker and configure secrets in the dashboard. Confirm the plan does not sleep. |

## Slash Commands

Join a voice channel, then use the commands in a text channel where the bot can respond.

| Command | Description |
|---------|-------------|
| `/play query:<query or URL>` | Search YouTube Music, play a single YouTube URL, or queue a playlist URL containing `list=`. Playlists resolve in pages so playback can start before the full list is fetched. Limit: 200 tracks per request. |
| `/skip` | Skip the current track and continue with the queue. |
| `/stop` | Stop playback, clear the queue, and disconnect from voice. |
| `/shuffle` | Shuffle the queued tracks. |
| `/queue [page]` | Show the current track, upcoming tracks, queue count, and autoplay status. |
| `/autoplay [enabled]` | Toggle autoplay, or set it explicitly with `enabled:true` / `enabled:false`. When enabled, the bot adds a related YouTube Music radio suggestion after the queue runs out. |

## Behavior Notes

- Playback is streamed through pipes; the bot should not create `audio.mp3*` temporary files.
- `/autoplay` is per server for the current voice session and resets when playback is stopped or the bot disconnects.
- Autoplay skips recently played video IDs to avoid immediately looping the same suggestions.
- `/stop` clears playback state and prevents autoplay from requeueing after a manual stop.
- If the bot is kicked from voice, the queue is cleared and it will not auto-rejoin.
- After playback ends, the bot disconnects from voice after 15 minutes of inactivity.

## Troubleshooting

| Problem | Things to check |
|---------|-----------------|
| `yt-dlp` / `ffmpeg` not found | Install both and confirm `yt-dlp --version` and `ffmpeg -version`. |
| No search/autocomplete/autoplay suggestions | Confirm `python search.py` is running on `127.0.0.1:5000` and that `flask`, `waitress`, and `ytmusicapi` are installed. |
| Playlist errors | Use the full YouTube URL including `list=...`; the API key must have YouTube Data API v3 enabled and quota available. |
| Voice 4017 / DAVE errors | Keep the `discordgo` replace in `go.mod` until upstream DAVE support is available. |
| Slash commands do not appear | Reinvite with the `applications.commands` scope, then restart the bot so it bulk-overwrites command definitions. |

## License

See [LICENSE](LICENSE).
