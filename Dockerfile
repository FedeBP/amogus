# Production image: Go + CGO (gopus/Opus) + ffmpeg + yt-dlp
FROM golang:1.22-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
	gcc libc6-dev pkg-config libopus-dev \
	&& rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 CGO_CFLAGS="-O2 -Wno-stringop-overread" \
	go build -trimpath -ldflags="-s -w" -o /out/amogus .

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
	ca-certificates ffmpeg curl python3 python3-pip libopus0 \
	&& pip3 install --no-cache-dir --break-system-packages yt-dlp \
	&& rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /out/amogus .

ENV DISCORD_BOT_TOKEN=""
ENV YOUTUBE_API_KEY=""

CMD ["./amogus"]
