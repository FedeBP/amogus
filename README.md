# amogus

A private Discord music bot for playing YouTube and YouTube Music audio in voice channels through slash commands.

The bot is built for small-server use: join voice, queue music from chat, keep the queue tidy, and optionally let autoplay continue the session with related tracks.

## How To Use

1. Join a voice channel.
2. Use `/play query:<song, artist, YouTube URL, or playlist URL>` in a text channel where the bot can respond.
3. Manage playback and the queue with the Now Playing buttons or slash commands.

The bot searches YouTube Music for normal text queries. It also accepts direct YouTube video URLs and playlist URLs containing `list=`.

## Commands

| Command | What it does |
|---------|--------------|
| `/play query:<query or URL>` | Plays or queues a song, YouTube URL, or playlist URL. Playlists load up to 200 tracks. |
| `/queue [page]` | Shows the current song, upcoming tracks, queue count, and autoplay status. |
| `/skip` | Skips the current song. If autoplay is on and the queue is empty, the bot looks for another related track. |
| `/stop` | Stops playback, clears the queue, resets autoplay, and disconnects from voice. |
| `/shuffle` | Shuffles upcoming tracks. |
| `/remove index:<number>` | Removes an upcoming track by queue number. Use `/queue` to see numbers. |
| `/move from:<number> to:<number>` | Moves an upcoming track to another queue position. |
| `/clear` | Clears upcoming tracks without stopping the current song. |
| `/autoplay [enabled]` | Toggles autoplay, or sets it with `enabled:true` / `enabled:false`. |

Now Playing messages also include buttons for skip, stop, queue, and autoplay.

## Autoplay

Autoplay is per server and lasts only for the current voice session. It resets when the bot is stopped, disconnected, or kicked from voice.

When autoplay is enabled and the queue runs out, the bot asks the YouTube Music sidecar for related tracks. It keeps recent track history so it can avoid exact repeats and near-duplicate title/artist loops.

## Queue Tips

- `/queue` is the best way to check what the bot thinks is happening.
- Queue numbers only apply to upcoming tracks. The current song is shown separately.
- `/clear` does not stop the current song.
- `/stop` is the hard reset: current song, queue, voice connection, and autoplay all end.

## Behavior Notes

- The bot should not auto-rejoin when someone disconnects it from voice.
- If the bot is kicked from voice, playback state and autoplay are cleared.
- After playback ends, the bot disconnects after 5 minutes of inactivity.
- Search, autocomplete, and autoplay suggestions require the local Python sidecar.

## Operator Setup

### Requirements

| Requirement | Notes |
|-------------|-------|
| Go 1.22+ | Matches `go.mod`. |
| Python 3 | Runs the local YTMusic search/radio sidecar. |
| Python packages | `flask`, `waitress`, and `ytmusicapi`. |
| yt-dlp | Downloads/extracts source audio. `youtube-dl` is accepted as a fallback. |
| ffmpeg | Must include `libopus`, which normal package-manager builds do. |
| Discord bot token | From the Discord Developer Portal. |
| YouTube Data API v3 key | Used for playlist expansion. Search/radio uses the YTMusic sidecar. |

CGO, a C compiler, and `gopus` are not required by the current audio pipeline.

### Configuration

Create `config.json` next to the executable, or run from the repo root:

```json
{
  "token": "your-discord-bot-token",
  "botPrefix": "&",
  "APIKey": "your-youtube-data-api-v3-key"
}
```

Environment variables override `config.json`:

| Variable | Meaning |
|----------|---------|
| `DISCORD_BOT_TOKEN` | Discord bot token |
| `YOUTUBE_API_KEY` | YouTube Data API v3 key |
| `BOT_PREFIX` | Legacy prefix field |

Keep secrets out of Git. `config.json` is ignored by this repo.

### Discord Setup

Invite the bot with these OAuth scopes:

| Scope | Why |
|-------|-----|
| `bot` | Lets the bot join voice and send messages. |
| `applications.commands` | Lets Discord expose slash commands. |

Useful bot permissions: View Channels, Send Messages, Connect, Speak, and Read Message History.

The bot uses guild and voice-state gateway intents. Message Content Intent is not needed for slash commands.

Some Discord voice regions require E2EE/DAVE. This repo still uses a `replace` in `go.mod` to build `discordgo` from a fork with DAVE support. When upstream ships that support, the replace can be removed.

### Local Run

Install the Python sidecar dependencies once:

```powershell
python -m pip install flask waitress ytmusicapi
```

Run a clean local session:

```powershell
Start-Process powershell -ArgumentList "-NoExit", "-Command", "python search.py"
go clean -cache
go clean -testcache
$env:CGO_ENABLED="1"
$env:CGO_CFLAGS="-O2 -Wno-stringop-overread"
go run -a .
```

Build a binary:

```powershell
go clean -cache
go build -a -o amogus.exe .
.\amogus.exe
```

### Docker

The Docker image builds the Go binary, installs `ffmpeg`, `yt-dlp`, and the Python search sidecar dependencies, then starts both the sidecar and the bot.

```bash
docker build -t amogus .
docker run --rm \
  -e DISCORD_BOT_TOKEN="..." \
  -e YOUTUBE_API_KEY="..." \
  amogus
```

Never bake tokens into the image. Use your hosting platform's secret manager or encrypted environment variables.

### Hosting Notes

This bot needs a long-running process for the Discord gateway and voice connection. Free web services that sleep after idle time will disconnect the bot.

For Fly.io, run one active machine per Discord bot token unless you add sharding or leader election. Multiple identical machines can receive the same interactions and fight over voice state. Use `fly scale count 1` for the current single-process bot.

Reasonable options:

| Host | Notes |
|------|-------|
| Render | Use a paid Background Worker. Free web services sleep. |
| Oracle Cloud Free Tier | See [docs/oracle-cloud.md](docs/oracle-cloud.md). |
| Fly.io / Railway / other PaaS | Use Docker and configure secrets in the dashboard. Confirm the plan does not sleep. |

## Troubleshooting

| Problem | Things to check |
|---------|-----------------|
| `yt-dlp` / `ffmpeg` not found | Install both and confirm `yt-dlp --version` and `ffmpeg -version`. |
| No search/autocomplete/autoplay suggestions | Confirm `python search.py` is running on `127.0.0.1:5000` and that `flask`, `waitress`, and `ytmusicapi` are installed. |
| Playlist errors | Use the full YouTube URL including `list=...`; the API key must have YouTube Data API v3 enabled and quota available. |
| Voice 4017 / DAVE errors | Keep the `discordgo` replace in `go.mod` until upstream DAVE support is available. |
| Slash commands do not appear | Restart the bot so it bulk-overwrites commands, then refresh Discord with `Ctrl+R`. |

## Audio Pipeline

The bot streams directly:

```text
yt-dlp stdout -> ffmpeg libopus OGG stdout -> OGG demux -> Discord OpusSend
```

There is no Go-side PCM decode/encode step.

## License

See [LICENSE](LICENSE).
