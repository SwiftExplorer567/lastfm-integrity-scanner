# lastfm-integrity-scanner

An admin backend that reads a Last.fm account's scrobble history, works out whether the plays could come from a real person, and writes a report as **JSON, HTML, PDF, PNG or SVG**. Use it to keep faked scrobbles off a leaderboard without punishing people whose setup is just messy.

```
demo-honest              score   0 Clean     leaderboard: keep    27,628 → 27,628
demo-two-devices         score   0 Clean     leaderboard: keep    38,835 → 38,830
demo-double-scrobbler    score   0 Clean     leaderboard: adjust  48,134 → 27,884
demo-scripted            score  75 Suspect   leaderboard: exclude 110,760
demo-faker               score 100 Suspect   leaderboard: exclude 919,755
```

## How it tells honest mess from cheating

Honest people also produce overlapping plays. For example, a Spotify scrobble and a browser extension can both record the same song, or a YouTube tab left open on the PC can keep playing while the phone plays Spotify. The scanner forgives these cases on purpose:

1. **Double scrobbles are removed, not scored.** Copies of one song within 60 s count as one play, up to 3 copies (one per scrobbler app). Metadata differences like "Song – Remastered 2011" vs "Song (Official Video)" still match. The copies are dropped from the adjusted count.
2. **Up to two players at once is fine.** Every other play gets the least time a player must spend on it before Last.fm accepts it: half the song, capped at 4 minutes, or 30 s when the length is unknown. The plays are then packed onto at most 2 players. Plays that fit only on the second player are accepted.
3. **Plays that need a third player are the core evidence.** A bot that scrobbles every 2–8 seconds needs dozens of players running at once. Its plays cannot fit, however generously each one is credited.

On top of that come the signals the admin panel already shows: impossible daily, monthly and lifetime volume, hours over 60 plays, bursts, same-song loops seconds apart, plays under 15 s apart (counted after duplicates are removed), different songs at the same second, fixed-interval spacing (scripts use one interval, while real song lengths vary), no-sleep days and no quiet hours. Signals are weighted by intent:

| kind | examples | weight |
|---|---|---|
| `fake` | needs a 3rd player, rapid loops, bursts, fixed interval, same-second batches | highest |
| `volume` | lifetime > 400/day, days > 600, months > 600/day, hours > 60 | high |
| `pattern` | loops within 10 min, 20+ hour days, no quiet hours | moderate (a forgotten autoplay tab can cause these) |
| `benign` | double scrobbles, second player | 0, listed so admins can see what was forgiven |

Score **0–34 clean**, **35–64 review**, **65+ suspect**. The leaderboard action is `keep`, `adjust` (clean, but rank by the adjusted count), `review` or `exclude`.

**Song lengths make the test sharper.** A script that submits a different song every 31 s gets past the 30 s floor, but not "half of a 3½-minute song". The scanner looks up lengths (`track.getInfo`) for the songs most often followed closely by another song. It does up to 300 lookups per scan, and it caches the lengths across all users.

## Speed

- **Parallel page downloads** (200 scrobbles per page) with one shared rate limiter, 5 req/s by default. Retries back off on Last.fm errors 8/11/16/29, HTTP 5xx and truncated responses. A page that still fails gets a second pass and is then reported as missing; it does not sink the whole scan.
- **Upper time bound pinned** at scan start, so page boundaries stay stable while new scrobbles arrive.
- **Local cache** (`data/users/<user>/history.bin.gz`, about 3–4 bytes per play). A rescan downloads only what is new, plus the last 72 h to catch offline plays that sync late.
- **Auto mode** does a full scan for accounts up to 300k scrobbles (about 5 min cold at 5 req/s, seconds when cached). Bigger accounts get a **quick** scan: the last 90 days (up to 150 pages) plus Last.fm's own monthly totals, at one cheap request per month, so the whole-history chart and the "months over the limit" check still cover everything.
- The analysis itself is O(n log n) and runs in memory. A 920k-play history takes about 0.1 s.

## Running

Requires Go 1.24+.

```sh
cp .env.example .env          # put your LASTFM_API_KEY in it
go build -o bin/lfscan ./cmd/lfscan

bin/lfscan demo               # example reports from generated accounts, no API needed
bin/lfscan scan HasanJWS -format json,html,pdf,png
bin/lfscan scan user1 user2 -mode quick
bin/lfscan render data/reports/hasanjws/latest.json -format pdf
bin/lfscan serve              # HTTP API on :8080
```

Or with Docker: `docker build -t lfscan . && docker run -p 8080:8080 --env-file .env -v $PWD/data:/data lfscan`.

### Configuration

| variable | default | |
|---|---|---|
| `LASTFM_API_KEY` | – | required; only read methods are used, so the shared secret is not needed |
| `LFSCAN_TOKEN` | – | bearer token for the HTTP API (set it!) |
| `LFSCAN_DATA_DIR` | `data` | cache, song lengths and saved reports |
| `LFSCAN_RPS` / `LFSCAN_BURST` | 5 / 10 | request rate shared by all scans |
| `LFSCAN_WORKERS` | 8 | requests in flight per scan |
| `LFSCAN_AUTO_FULL_MAX` | 300000 | auto mode does a full scan up to this many scrobbles |
| `LFSCAN_DURATION_LOOKUPS` | 300 | `track.getInfo` calls per scan (0 disables) |
| `LFSCAN_ADDR` | `:8080` | listen address |

## HTTP API

Every `/v1` route requires `Authorization: Bearer $LFSCAN_TOKEN`.

```http
POST /v1/scans            {"user": "HasanJWS", "mode": "auto|full|quick", "refresh": false}
→ 202 {"id": "…", "status": "queued", …}

GET  /v1/scans/{id}       → {"status": "running", "stage": "history", "done": 412, "total": 738}
                          → {"status": "done", "score": {…}, "leaderboard_action": "keep",
                             "reports": {"json": "/v1/scans/{id}/report.json", "pdf": …}}
                             (add ?report=1 to inline the full report)
GET  /v1/scans/{id}/report.{json|html|pdf|png|svg}[?download=1]
GET  /v1/users/{user}/report.{json|html|pdf|png|svg}    latest saved report
POST /v1/batch            {"users": ["a", "b", …], "mode": "auto"}   e.g. the whole leaderboard
GET  /v1/scans            jobs in memory
GET  /healthz
```

A scan already running for a user is returned instead of starting a second one. A failed job carries `error_code`: `not_found`, `private` (the user hides their recent listening) or `failed`.

## Report JSON

`analysis.Report` (`internal/analysis/report.go`) is the contract, and every other format is rendered from it. The main fields are `score`, `signals[]`, `leaderboard` (`action`, `raw_scrobbles`, `adjusted_scrobbles`, `estimated`), `integrity` (duplicates, second-device plays, excess plays, loops, bursts, …), `stats`, `charts` (months, days, week × hour, gaps, busiest days, densest bursts, most repeated songs), `measurement` and `params`.

## Layout

```
cmd/lfscan          CLI: scan, serve, render, demo
internal/lastfm     API client: rate limit, retries, parallel pages, monthly totals, song lengths
internal/scrobble   compact interned history + on-disk cache
internal/scanner    one scan end to end (cache, incremental fetch, lengths, analysis, saving)
internal/analysis   the detection logic and scoring
internal/render     JSON / HTML / PDF / PNG / SVG
internal/server     HTTP API with a job queue
internal/synth      generated listeners for tests and the demo
internal/mockfm     fake Last.fm API (incl. its JSON quirks and injected errors) for tests
```

## Limits

- All days and hours are UTC.
- An incremental rescan does not see edits older than 72 h (deleted scrobbles, very late offline syncs). Use `refresh` to download everything again.
- The PDF embeds the Go fonts (Latin incl. Turkish, Greek, Cyrillic). Song titles in other scripts show as boxes in the PDF and PNG, but stay intact in the HTML and JSON.
