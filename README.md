# lastfm-integrity-scanner

An admin backend that reads a Last.fm account's scrobble history, works out whether the plays could come from a real person, and writes a report as **JSON, HTML, PDF, PNG or SVG**. Use it to keep faked scrobbles off a leaderboard without punishing people whose setup is just messy.

```
demo-honest              score   0 Clean     leaderboard: keep    27,628 → 27,628
demo-two-devices         score   0 Clean     leaderboard: keep    38,835 → 38,830
demo-double-scrobbler    score   0 Clean     leaderboard: adjust  48,134 → 27,884
demo-scripted            score  75 Suspect   leaderboard: exclude 110,760
demo-faker               score 100 Suspect   leaderboard: exclude 919,755
```

## Pre-import check (under 10 seconds, any account size)

`lfscan check <user>` and `GET /v1/check/{user}` decide whether an account may be imported: **pass**, **review**, **block**, or **unknown** (Last.fm did not answer enough in time, so retry). The check never downloads the whole history. It reads Last.fm's aggregate answers and scattered samples, so a 4.6-million-scrobble bot takes about as long as a new account.

| round | requests | what it gets |
|---|---|---|
| 1 | `user.getInfo`, `user.getTopTracks period=overall` | lifetime playcount, sign-up date, per-song lifetime totals with song lengths, number of distinct songs |
| 2 | one `user.getRecentTracks` per period (the account's life cut into ~24 periods, `from`/`to`, `limit=1000`) | **exact** play count of each period (`@attr.total`), plus the newest ≤1,000 consecutive plays of each period as evidence |
| 3 | the 2 densest periods split into months | exact monthly counts where it matters, plus more windows there |
| 4 | a page from the middle of the 2 densest months | evidence from inside the hottest stretch, not only its end |

That is typically 20–40 requests in 4 parallel rounds.

Every sampled window goes through the same analysis as a full scan: double scrobbles are forgiven, a second player is accepted, and plays that need a third player are counted. On top of that, the lifetime top songs give **proof that does not need the history**. Every scrobble needs some listening time (half the song, or 30 s when its length is unknown), so *"the top 200 songs alone need 148,784 hours; the account has existed for 43,824"* cannot be explained away.

Measured against the built-in fake Last.fm (120–150 ms per request, the production rate limit):

```
demo-bot-4m            4,566,128 scrobbles  BLOCK   score 100  0.9 s  31 requests
demo-veteran             276,374 scrobbles  PASS    score   0  1.0 s  37 requests   (10-year account)
demo-two-devices         117,235 scrobbles  PASS    score   0                       (phone + forgotten tab)
demo-double-scrobbler     94,133 scrobbles  PASS    score   0                       (adjusted count 55k)
demo-scripted            111,456 scrobbles  REVIEW  score  41                       (a song every 31 s)
```

Run `lfscan demo-check` to reproduce this without an API key.

**Many sign-ups at once.** Last.fm allows 5 requests per second per IP, *averaged over 5 minutes*. The client runs at 4.5/s with a burst of 60, which stays under 1,500 requests in any 5-minute window. Checks run at most 3 at a time, and the rest queue. When the shared budget runs low, each check reads fewer periods (never fewer than 8), so the queue keeps moving. Full-history downloads are held to 3 requests/s, so they never starve checks. In a test with 8 sign-ups arriving together, the first 3 finished in under 1 s and the 8th in 13 s. Sustained, one IP handles roughly 15–25 checks a minute. A result is cached for 6 hours (`force` skips the cache).

### What the Last.fm API docs changed here

Sources: [lastfm-docs/api-docs](https://github.com/lastfm-docs/api-docs) and the official [API terms](https://www.last.fm/api/tos).

- `user.getRecentTracks` accepts **`limit` up to 1000** (the official page says 200). Full scans now need 5× fewer requests: 4.7M scrobbles is about 4,700 pages instead of 23,500.
- A range query (`from`/`to`) returns the exact count in that range as `@attr.total`. The check is built on this.
- `user.getTopTracks` carries each song's `duration` and lifetime `playcount`, and `@attr.total` is the number of distinct songs. `user.getInfo` has no such count.
- **Known API bug:** Last.fm occasionally returns another user's data. Every answer's `@attr.user` is compared with the requested user and re-requested on mismatch, so one account's plays never end up in another's report.
- Errors come with HTTP 403/404 and a JSON body (`6` user not found, `17` hidden recent listening, `29` rate limit). All of them are handled.

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

- **Parallel page downloads** (1,000 scrobbles per page) with one shared rate limiter (4.5 req/s, burst 60; bulk downloads capped at 3 req/s). Retries back off on Last.fm errors 8/11/16/29, HTTP 5xx and truncated responses. A page that still fails gets a second pass and is then reported as missing; it does not sink the whole scan.
- **Upper time bound pinned** at scan start, so page boundaries stay stable while new scrobbles arrive.
- **Local cache** (`data/users/<user>/history.bin.gz`, about 3–4 bytes per play). A rescan downloads only what is new, plus the last 72 h to catch offline plays that sync late.
- **Auto mode** does a full scan for accounts up to 300k scrobbles (about 300 pages, under 2 minutes cold, seconds when cached). Bigger accounts get a **quick** scan: the last 90 days (up to 150 pages) plus Last.fm's own monthly totals, at one cheap request per month, so the whole-history chart and the "months over the limit" check still cover everything.
- The analysis itself is O(n log n) and runs in memory. A 920k-play history takes about 0.1 s.

## Running

Requires Go 1.24+.

```sh
cp .env.example .env          # put your LASTFM_API_KEY in it
go build -o bin/lfscan ./cmd/lfscan

bin/lfscan demo               # example reports from generated accounts, no API needed
bin/lfscan demo-check         # pre-import checks against a built-in fake Last.fm, no API needed
bin/lfscan check HasanJWS ChAelitaNicole   # pre-import check, a few seconds each
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
| `LFSCAN_RPS` / `LFSCAN_BURST` | 4.5 / 60 | request rate shared by everything (Last.fm: 5/s averaged over 5 min) |
| `LFSCAN_WORKERS` | 8 | requests in flight per scan |
| `LFSCAN_AUTO_FULL_MAX` | 300000 | auto mode does a full scan up to this many scrobbles |
| `LFSCAN_DURATION_LOOKUPS` | 300 | `track.getInfo` calls per scan (0 disables) |
| `LFSCAN_ADDR` | `:8080` | listen address |

## HTTP API

Every `/v1` route requires `Authorization: Bearer $LFSCAN_TOKEN`.

```http
GET  /v1/check/{user}[?force=1]      pre-import check, synchronous, a few seconds
POST /v1/check            {"user": "HasanJWS", "force": false}
→ 200 {"decision": "pass|review|block|unknown", "reason": "…", "score": {…},
       "signals": [...], "gate": {…evidence…},
       "reports": {"html": "/v1/users/HasanJWS/check.html", "png": …, "pdf": …, "json": …}}
→ 404 not_found · 422 private (recent listening hidden) · 503 busy (queue full, retry)
GET  /v1/users/{user}/check.{json|html|pdf|png|svg}

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
