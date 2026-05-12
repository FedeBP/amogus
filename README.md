# amogus

A Discord bot written in **Go** that plays audio from **YouTube** in voice channels (search, direct URLs, and playlists via the YouTube Data API). Respect copyright, YouTube’s Terms of Service, and your local laws. This project is for personal/educational use.

---

## Dependencies

### Runtime

| Requirement | Notes |
|-------------|--------|
| **Go 1.22+** | Matches `go.mod` / toolchain. Run `go version`. |
| **CGO enabled + C compiler** | Required by [`layeh.com/gopus`](https://layeh.com/gopus) (Opus encoding). On Windows install a MinGW-w64 toolchain (e.g. [WinLibs](https://winlibs.com/) via `winget install BrechtSanders.WinLibs.POSIX.UCRT`) so `gcc` is on `PATH`. |
| **yt-dlp** (preferred) or **youtube-dl** | Downloads/extracts audio. Example: `winget install yt-dlp.yt-dlp`. |
| **ffmpeg** | Decodes audio for streaming to Discord. Example: `winget install Gyan.FFmpeg`. |

After installing tools on Windows, **open a new terminal** (or refresh `PATH`) so `gcc`, `yt-dlp`, and `ffmpeg` resolve.

### Accounts & APIs

| Requirement | Notes |
|-------------|--------|
| **Discord application / bot token** | [Discord Developer Portal](https://discord.com/developers/applications) → your app → **Bot** → token. |
| **YouTube Data API v3 key** | [Google Cloud Console](https://console.cloud.google.com/) → enable **YouTube Data API v3** → **Credentials** → API key. |

### Library note (voice / DAVE)

Some Discord voice regions require **E2EE (DAVE)**. This repo uses a **`replace`** in `go.mod` to build [`discordgo`](https://github.com/bwmarrin/discordgo) from a fork that includes DAVE support. When upstream merges that work, you can remove the `replace` and use a normal `go get` on `github.com/bwmarrin/discordgo`.

---

## Configuration

Create `config.json` next to the executable (or run from the repo root so `./config.json` is found):

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
| `APIKey` | Yes | YouTube Data API v3 key (search + playlist expansion). |
| `botPrefix` | Loaded but unused | Reserved; commands are fixed (`&play`, etc.). |

**Environment variables (recommended on servers):** if set, these override `config.json`:

| Variable | Overrides |
|----------|-----------|
| `DISCORD_BOT_TOKEN` | Discord bot token |
| `YOUTUBE_API_KEY` | YouTube Data API v3 key |
| `BOT_PREFIX` | Prefix string (optional; still mainly informational) |

Keep secrets out of Git: `config.json` is listed in `.gitignore`. If it was committed before, run `git rm --cached config.json` and rotate any exposed tokens in the Discord and Google consoles.

### Discord application settings

1. **Bot** → enable **Message Content Intent** (needed to read `&play` text in servers).
2. **OAuth2 → URL Generator**: scope **bot**, permissions such as **View Channels**, **Send Messages**, **Connect**, **Speak**, **Read Message History**. Invite the bot with that URL.
3. Voice channels that enforce DAVE need a client that supports it (handled by the `discordgo` fork above).

---

## Build & run

From the repository root:

```powershell
# Windows: refresh PATH if gcc / yt-dlp / ffmpeg were just installed
$env:Path = [System.Environment]::GetEnvironmentVariable("Path","Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path","User")
$env:CGO_ENABLED = "1"
$env:CGO_CFLAGS = "-O2 -Wno-stringop-overread"

go run .
```

Build a binary:

```powershell
go build -o amogus.exe .
./amogus.exe
```

- If you see `build constraints exclude all Go files` for `gopus`, **CGO is off** (`CGO_ENABLED=0`) or **`gcc` is missing from PATH**.
- Optional: keep build logs quieter with `CGO_CFLAGS` as above.

---

## Hosting (Docker, secrets, Render)

This bot needs a **long-running process** (Discord gateway + voice), **ffmpeg**, **yt-dlp**, and (because of voice encoding) **CGO + libopus** at runtime.

### Protecting keys

Use your platform’s **encrypted environment variables** or **secret manager**—never bake tokens into the image or commit them. Set `DISCORD_BOT_TOKEN` and `YOUTUBE_API_KEY` there.

### Docker

From the repo root (Docker installs ffmpeg and yt-dlp inside the image):

```bash
docker build -t amogus .
docker run --rm -e DISCORD_BOT_TOKEN="..." -e YOUTUBE_API_KEY="..." amogus
```

### Render.com specifically

Render’s **free web services spin down** after idle time, which **disconnects** a normal Discord bot. **Background Workers** (the usual choice for bots) are **not** on Render’s free instance type—you need a **paid** worker or another host.

Reasonable directions:

- **Render**: deploy this repo as a **Background Worker** on a **paid** instance and set env vars in the dashboard (private Git repo supported).
- **Always-free VPS**: e.g. **Oracle Cloud Free Tier** (ARM VM)—step-by-step setup is in [`docs/oracle-cloud.md`](docs/oracle-cloud.md).
- **Other PaaS**: **Fly.io**, **Railway**, etc.—same pattern: Dockerfile + secrets in the dashboard; confirm the plan does not sleep the process.

---

## Using the bot

1. **Join a voice channel** in your server.
2. In a **text channel** the bot can read, run the commands below.

| Command | Description |
|---------|-------------|
| `&play <query or URL>` | **Search** text, play a **single video URL**, or queue a **playlist** (`list=` in the URL). You must already be in a voice channel. Playlists resolve in pages so playback can start before the full list is fetched (cap: 200 tracks per request). |
| `&skip` or `&next` | Skip the **current** track: stops yt-dlp download or ffmpeg playback and continues with the rest of the queue (aliases). |
| `&shuffle` | Shuffles the internal song queue (if more than one track). |
| `&stop` | Clears the queue, removes temporary audio files in the working directory, and disconnects from voice in that guild. |

Commands use the **`&`** prefix in code (not `botPrefix`). Leading/trailing spaces after `&play` are OK.

**Temporary files:** the bot writes downloads matching `audio.mp3*` in its **current working directory**. Use `&stop` or disconnect the bot from voice (see below) to clean up.

---

## Behaviour notes

- **Disconnect / kick:** If the bot is removed from voice while tracks remain, the queue is cleared and it will **not** auto-rejoin to drain the queue.
- **Idle:** After playback, the bot disconnects from voice after **15 minutes** of inactivity (timer resets each track).

---

## Troubleshooting

| Problem | Things to check |
|---------|------------------|
| `gcc` / CGO / `gopus` errors | `CGO_ENABLED=1`, MinGW on `PATH`, new shell after install. |
| `yt-dlp` / `ffmpeg` not found | Install both; confirm `yt-dlp --version` and `ffmpeg -version`. |
| Voice **4017 / DAVE** | Ensure you build with the `replace` fork in `go.mod`; update when upstream ships DAVE. |
| Bot ignores messages | Message Content Intent enabled; bot has channel permissions. |
| Playlist errors | Use the full YouTube URL including `list=…`; API key must have **YouTube Data API v3** enabled and quota. |

---

## License

See `LICENSE` in the repository.
