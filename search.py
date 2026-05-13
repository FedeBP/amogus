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

        if any(w in title_lower for w in BAD_WORDS):
            continue

        filtered.append({
            "title": title,
            "artist": (
                r.get("artists", [{}])[0].get("name", "")
                if r.get("artists")
                else ""
            ),
            "videoId": r.get("videoId", ""),
            "duration": r.get("duration", ""),
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

print("YTMusic search backend started on :5000")

serve(app, host="127.0.0.1", port=5000, threads=8)