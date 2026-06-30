from flask import Flask, request, jsonify
from waitress import serve
from ytmusicapi import YTMusic

app = Flask(__name__)

yt = YTMusic()

def serialize_results(results):
    serialized = []

    for r in results:
        title = r.get("title", "")
        video_id = r.get("videoId", "")

        if not video_id:
            continue

        artists = r.get("artists") or []
        artist = ""
        if artists and isinstance(artists[0], dict):
            artist = artists[0].get("name", "")

        serialized.append({
            "title": title,
            "artist": artist,
            "videoId": video_id,
            "duration": r.get("duration") or r.get("length", ""),
        })

    return serialized

@app.route("/search")
def search():
    query = request.args.get("q", "")

    if not query:
        return jsonify([])

    results = yt.search(query, filter="songs", limit=5)

    return jsonify(serialize_results(results))

@app.route("/first")
def first():
    query = request.args.get("q", "")

    if not query:
        return ""

    results = yt.search(query, filter="songs", limit=1)
    results = serialize_results(results)

    if not results:
        return ""

    return results[0]["videoId"]

@app.route("/radio")
def radio():
    video_id = request.args.get("videoId", "").strip()

    if not video_id:
        return jsonify([])

    try:
        playlist = yt.get_watch_playlist(videoId=video_id, radio=True)
    except Exception:
        app.logger.exception("YTMusic radio lookup failed")
        return jsonify([])

    return jsonify(serialize_results(playlist.get("tracks", [])))

print("YTMusic search backend started on :5000")

serve(app, host="127.0.0.1", port=5000, threads=4)
