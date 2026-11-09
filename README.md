# Campaign Events service

Receives delivery-provider webhook events for email/SMS/push campaigns and serves per-campaign statistics for the marketing dashboard.

The service is at the repository root. The provided `starter/` skeleton was replaced; its seed file now lives at [seed/events.json](seed/events.json). The debugging exercise is a separate module in [debugging/](debugging/).

## Requirements

- Go 1.22 or newer (tested with Go 1.27). The service itself is pure Go — no cgo, no database server, no external services.
- `go test -race` additionally needs a C toolchain, because the race detector uses cgo. On a normal Go installation `gcc`/`clang` is enough; without one, run `go test` (the plain tests still cover the behaviour).

## Run

```bash
go run .
```

That listens on `:8080` and stores data in `./events.db` (created on first run). Flags or environment variables change both:

```bash
go run . -addr 127.0.0.1:9000 -db /tmp/relay.db
ADDR=:9000 DB=/tmp/relay.db go run .
```

Optional: inspect the database with `sqlite3 events.db '.schema'` (any SQLite client works).

## API

### `POST /events` — ingest a batch

The body must be a JSON array. A single event is just an array of one.

```bash
curl -s -X POST localhost:8080/events \
  -H 'Content-Type: application/json' \
  -d '[
    {"event_id":"evt_1","campaign_id":"cmp_summer_sale","contact_id":"ct_001","type":"sent","timestamp":"2026-08-10T06:15:00Z"},
    {"event_id":"evt_2","campaign_id":"cmp_summer_sale","contact_id":"ct_001","type":"delivered","timestamp":"2026-08-10T06:16:00Z"}
  ]'
```

Response (`200 OK`):

```json
{"received":2,"accepted":2,"duplicates":0,"rejected":0,"errors":[]}
```

Loading the day of real seed traffic:

```bash
curl -s -X POST localhost:8080/events \
  -H 'Content-Type: application/json' \
  --data-binary @seed/events.json
```

Expected result: `{"received":235,"accepted":210,"duplicates":18,"rejected":7,"errors":[...]}` — 7 errors with `index`, `event_id`, `reason`.

Status codes and what they mean:

| Case | Status | Why |
|---|---|---|
| Array with good and/or bad elements | `200` | Per-element results; one bad event never rejects its neighbours. |
| Duplicate `event_id` | `200`, counted in `duplicates` | Providers must get a 2xx so they stop retrying. Duplicates are not errors. |
| All elements invalid | `200`, `accepted:0`, `rejected:N` | Retrying invalid data cannot fix it. Fix the payload instead. |
| Body is not JSON / not an array / trailing data | `400` | Nothing can be interpreted. |
| More than 1,000 events | `400` | Chunk the batch; silently dropping the tail would lose data. |
| Body over 1 MiB | `413` | Chunk the batch. |
| Storage failure | `500` | Nothing was committed (one transaction per batch), so a retry is safe. |

Each batch is validated first, then written in **one transaction**. A `500` therefore stores nothing.

### `GET /campaigns/{campaign_id}/stats`

```bash
curl -s localhost:8080/campaigns/cmp_summer_sale/stats
```

```json
{
  "campaign_id": "cmp_summer_sale",
  "counts":       {"sent": 40, "delivered": 35, "opened": 19, "clicked": 12},
  "total_events": {"sent": 40, "delivered": 35, "opened": 27, "clicked": 12},
  "rates": {
    "delivered_of_sent": 0.875,
    "opened_of_delivered": 0.5429,
    "clicked_of_opened": 0.6316
  },
  "first_event_at": "2026-08-10T06:01:00Z",
  "last_event_at": "2026-08-10T18:28:00Z",
  "as_of": "2026-11-09T10:33:02Z"
}
```

- `counts` = **unique contacts** per type (`COUNT(DISTINCT contact_id)`). "Opened: 19" means 19 people opened, even though the campaign had 27 open events.
- `total_events` = raw stored events per type, so the dashboard can show volume and the difference is visible.
- `rates` = ratios of the unique-contact counts, rounded to 4 decimals. `null` when the denominator is zero, and clamped at `1.0`. Clamping is deliberate: opens can legitimately be stored before deliveries, and a dashboard showing 143% looks broken; the raw counts remain available.
- `first_event_at` / `last_event_at` use the **event's** timestamp, not arrival time.
- Unknown campaign (no stored events) → `404 {"error":"campaign not found"}`. There is no campaign table: a campaign exists because events reference it.

### `GET /campaigns/{campaign_id}/events` — optional recent-activity view

Keyset pagination, ordered by `(timestamp, event_id)`:

```bash
curl -s "localhost:8080/campaigns/cmp_summer_sale/events?limit=50"
curl -s "localhost:8080/campaigns/cmp_summer_sale/events?limit=50&cursor=<next_cursor>"
```

```json
{"campaign_id":"cmp_summer_sale","events":[{"event_id":"evt_00001","campaign_id":"cmp_summer_sale","contact_id":"ct_001","type":"sent","timestamp":"2026-08-10T06:01:00Z","received_at":"2026-11-09T10:33:02.840489Z"}],"next_cursor":"..."}
```

`limit` defaults to 50, maximum 200. `next_cursor` is omitted on the last page. An invalid cursor is `400`; an unknown campaign on the first page is `404`.

### Rejecting an event

An event is rejected when:

- `event_id`, `campaign_id` or `contact_id` is missing/blank/over 200 bytes,
- `type` is not exactly `sent`, `delivered`, `opened` or `clicked` (`OPENED` is rejected, not lowercased),
- `timestamp` is not RFC3339 with an offset, or is more than 24 hours in the future,
- `metadata` is present but is not a JSON object.

Old timestamps are accepted: late data is normal. `metadata` is stored opaquely, never interpreted.

## Design notes (short version)

- **Dedup key is `event_id`.** The primary key plus `INSERT OR IGNORE` makes dedup atomic in the database, so two simultaneous requests cannot both insert the same event. First write wins; a retry with a different payload is still a duplicate, counted in `duplicates`, and logged as a warning with both types.
- **SQLite, not in-memory.** Events are the source of truth for marketing numbers; losing them on restart is not acceptable, and SQL lets us recompute/reconcile. `modernc.org/sqlite` is pure Go, so `go run .` works without a C compiler.
- **One writer connection** (`MaxOpenConns(1)`), WAL and `busy_timeout=5000`. SQLite has one writer anyway; serialising removes `SQLITE_BUSY` retry code and the read cost is irrelevant at thousands of events/day.
- **Timestamps are stored as fixed-width UTC text** (`2006-01-02T15:04:05.000000000Z`), so text comparison is chronological comparison. `RFC3339Nano` drops trailing zeros, which would make text sorting wrong.
- **Per-element decoding.** The body is decoded into `[]json.RawMessage`, then each element into an `Event`. A malformed element is reported by index instead of failing the array.

Longer reasoning, including ambiguities and the questions I would ask the PM, is in [NOTES.md](NOTES.md).

## Tests

```bash
go test ./...            # behaviour
go test -race ./...      # same tests under the race detector (needs cgo)
go vet ./...
gofmt -l *.go            # should print nothing
```

The suite covers: partial-batch success, duplicates (including conflicting payloads and 20 concurrent identical POSTs), opened-before-delivered, unique-contact counting and rate clamping, malformed/oversized bodies, 404s, and loading + fully paginating the seed file. The seed test pins the exact expected numbers (235/210/18/7 and `cmp_summer_sale` counts).

The debugging exercise is its own module:

```bash
cd debugging
go run . events.jsonl | diff - expected_output.txt   # empty
go run -race . events.jsonl                          # no race report
```

## Layout

```
main.go        flags, HTTP server, graceful shutdown
server.go      handlers, request caps, JSON responses
event.go       Event type, per-element validation, timestamp normalisation
store.go       SQLite schema, atomic batch insert, stats, keyset pagination
server_test.go tests
seed/          the provided day of provider traffic
debugging/     Part 3 module (fixed) + BUGS.md at the root
NOTES.md       Part 1, scale memo, angry marketer, close-out
```
