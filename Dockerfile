# -------------------------------------------------------
# Build stage
# -------------------------------------------------------

	FROM golang:1.24-bookworm AS builder

	WORKDIR /app
	
	COPY go.mod go.sum ./
	RUN go mod download
	
	COPY . .
	
	ENV CGO_ENABLED=1
	ENV CGO_CFLAGS="-O2 -Wno-stringop-overread"
	
	RUN go build -o amogusbot .
	
	# -------------------------------------------------------
	# Runtime stage
	# -------------------------------------------------------
	
	FROM debian:bookworm-slim
	
	WORKDIR /app
	
	# System dependencies
	RUN apt-get update && apt-get install -y \
		ffmpeg \
		yt-dlp \
		python3 \
		python3-pip \
		ca-certificates \
		&& rm -rf /var/lib/apt/lists/*
	
	# Python dependencies
	RUN pip3 install --break-system-packages \
		flask \
		ytmusicapi \
		waitress
	
	# Copy binary
	COPY --from=builder /app/amogusbot .
	
	# Copy python search service
	COPY search.py .
	
	# Fly.io uses PORT sometimes; harmless here
	ENV PORT=5000
	
	# Start BOTH services
	CMD sh -c "python3 search.py & ./amogusbot"