# -------------------------------------------------------
# Build stage
# -------------------------------------------------------
FROM golang:1.22-bookworm AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0

RUN go build -o amogusbot .

# -------------------------------------------------------
# Runtime stage
# -------------------------------------------------------
FROM debian:bookworm-slim

WORKDIR /app

RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		ca-certificates \
		ffmpeg \
		python3 \
		python3-venv \
	&& python3 -m venv /opt/search-venv \
	&& /opt/search-venv/bin/pip install --no-cache-dir \
		flask \
		waitress \
		yt-dlp \
		ytmusicapi \
	&& rm -rf /var/lib/apt/lists/*

ENV PATH="/opt/search-venv/bin:${PATH}"
ENV PYTHONUNBUFFERED=1
ENV PORT=5000

COPY --from=builder /app/amogusbot .
COPY search.py .

CMD ["sh", "-c", "python3 search.py & exec ./amogusbot"]
