from flask import Flask, request, jsonify
from waitress import serve
from ytmusicapi import YTMusic

app = Flask(__name__)

yt = YTMusic()

BAD_WORDS = [
    "live",
    "karaoke",
    "nightcore",
    "slowed",
    "sped up",
]

def clean_results(results):
    filtered = []

    for r in results:
        title = r.get("title", "")
        title_lower = title.lower()
        video_id = r.get("videoId", "")

        if not video_id:
            continue
        if any(w in title_lower for w in BAD_WORDS):
            continue

        artists = r.get("artists") or []
        artist = ""
        if artists and isinstance(artists[0], dict):
            artist = artists[0].get("name", "")

        filtered.append({
            "title": title,
            "artist": artist,
            "videoId": video_id,
            "duration": r.get("duration") or r.get("length", ""),
        })

    return filtered

@app.route("/search")
def search():
    query = request.args.get("q", "")

    if not query:
        return jsonify([])

    results = yt.search(query, filter="songs", limit=5)

    return jsonify(clean_results(results))

@app.route("/first")
def first():
    query = request.args.get("q", "")

    if not query:
        return ""

    results = yt.search(query, filter="songs", limit=1)
    results = clean_results(results)

    if not results:
        return ""

    return results[0]["videoId"]

@app.route("/radio")
def radio():
    video_id = request.args.get("videoId", "").strip()

    if not video_id:
        return jsonify([])

    try:
        limit = int(request.args.get("limit", "25"))
    except ValueError:
        limit = 25
    limit = max(1, min(limit, 50))

    try:
        playlist = yt.get_watch_playlist(videoId=video_id, limit=limit, radio=True)
    except Exception:
        app.logger.exception("YTMusic radio lookup failed")
        return jsonify([])

    return jsonify(clean_results(playlist.get("tracks", [])))

print("YTMusic search backend started on :5000")

serve(app, host="127.0.0.1", port=5000, threads=8)
