# Campaign Events — notes

## Step 0: what is actually in `seed/events.json` (read before designing)

I parsed the file rather than eyeballing it. It is one JSON array, 235 rows, 33.5 KB. It parses as valid JSON as a whole, so the mess is *inside* the array, not around it.

| Mess | Count | Examples |
|---|---|---|
| Retried duplicate `event_id` | 16 ids, 18 extra rows (11 exact retries, 3 conflicting payloads, 2 ids retried twice more) | `evt_00126` appears 3x; `evt_00029`, `evt_00113`, `evt_00164` have the same id but a different `type` |
| Unknown event type | 2 | `spam_report`, `OPENED` (uppercase) |
| Missing required field | 1 | `evt_00213` has no `contact_id` |
| Empty string ids | 2 | `event_id: ""`, `contact_id: ""` |
| Unparseable timestamp | 1 | `"not-a-time"` |
| Non-RFC3339 timestamp | 1 | `"10-08-2026 09:00"` (DD-MM-YYYY, no timezone) |
| Out-of-order arrival | many | `opened` rows appear before the `sent`/`delivered` for the same contact/message |
| Same contact opening repeatedly | 14 (campaign, contact) pairs open more than once, max 4 times | only matters because "opened" must mean unique contacts |
| No future dates | 0 | I checked; all valid timestamps are on 2026-08-10. I still guard future timestamps because providers do send them. |
| `metadata` | 21 rows, all objects | no wrong types or nulls here, so metadata type-checking is defensive, not driven by the seed |

What this means for validation: the only things I *have* to decide are duplicates, the 4 allowed types, non-empty ids, and RFC3339 parsing. I reject rather than normalise (`OPENED` is not silently lowercased; `10-08-2026` is not guessed). Guessing provider data is how you silently corrupt a dashboard.

After "validate first, then dedup, first write wins", the seed yields exactly: **235 received, 210 accepted, 18 duplicates, 7 rejected**. I use those numbers in a test so the seed's mess is pinned down, not just survived.

## 1. My interpretation of the problem

- Relay is an *event sink* in front of third-party providers. Providers push facts (`sent`, `delivered`, `opened`, `clicked`); a dashboard reads aggregated numbers per campaign.
- The hard parts are not CRUD. They are: (a) the same fact can be delivered more than once, (b) facts arrive late and in any order, (c) one bad row must not poison the other rows in a batch, and (d) the numbers must be explainable when a marketer says they are wrong.
- So the service is: a tolerant-but-strict ingest endpoint (reject only what is genuinely unusable, return 2xx for duplicates so retries stop), a deduplicating durable store keyed by `event_id`, and a read endpoint that aggregates by the event's own timestamp.
- "How many opened" means **unique contacts who opened in that campaign**, not raw open events. One person opening five times is one opener. I expose raw event totals next to it so the difference is visible.

## 2. Assumptions

- `event_id` is globally unique across providers/campaigns (the brief says it identifies one event). If providers can reuse ids across tenants, the key becomes `(provider, event_id)`; the schema is one column away.
- A duplicate with a different payload is still a duplicate. First write wins; we log a warning and keep serving 2xx. We do not "correct" history from a retry, because there is no way to know which copy is right.
- Arrival order means nothing. Counts, `first_event_at`/`last_event_at`, and daily buckets use the provider `timestamp`.
- `timestamp` must be RFC3339 with an offset (`Z` counts). No timezone → reject. It is a contract field, not free text.
- A timestamp more than 24h in the future is a bug on the provider side, not "late data": reject it. Past timestamps are unbounded (late is normal); I don't reject old data.
- "Reject" means the event is not stored. The response tells the provider which index failed and why. We do not quarantine invalid events yet (documented under "skipped").
- At this size (~50 campaigns, a few thousand events/day) one process and one SQLite file are enough. Multi-instance deployment is out of scope.
- `metadata` is opaque: store it, never interpret it.

## 3. Ambiguities in the brief

- **What is "sent"?** Messages handed to the provider, or unique contacts reached? The brief says "how many were sent" and separately "opened". Once opened means unique contacts, sent/delivered must use the same definition or the rates mix units. I use unique contacts for all four, and expose `total_events` per type so a marketer who wants message volume can see it. This is a product decision, not a technical one.
- **What is a campaign?** There is no campaign table. A campaign "exists" because events reference it. So stats for an id with zero events is 404, which is honest but makes an empty campaign look missing.
- **Who owns validation failures?** We can 200 the batch and report rejects (chosen), or fail the whole request (bad: one bad row blocks 999 good ones, provider retries forever), or drop silently (worst: data disappears with no trace).
- **Duplicate definition.** Is it the same `event_id`, or the same `(campaign, contact, type, timestamp)`? I use `event_id` per the brief. Two providers reporting the same open with different ids would count twice; nothing in the brief lets me distinguish that from two real opens.
- **"Absurdly far in the future"** is undefined. I chose 24h.
- **Are events ever deleted?** GDPR/contact deletion is not mentioned. There is no delete path.
- **Does the dashboard need time ranges?** The brief only asks for campaign totals. A real dashboard will want `from`/`to`. I return whole-campaign stats and note it.
- **Retention.** Unbounded table growth is fine at this volume, undefined at scale.

## 4. Questions I would ask the PM

1. For "opened", do you want unique contacts or open events? (I default to unique contacts and show both; I need confirmation the dashboard labels match.)
2. When a contact receives the same campaign twice, are they still one "sent"? Today my unique definition says yes.
3. Do you need stats for a date range / timezone, or is "campaign lifetime" enough for v1?
4. What should the dashboard show for a campaign with no events yet — 404 or zeros? (Today: 404.)
5. If a provider sends the same `event_id` with a different type, is first-write-wins acceptable, or do we need a manual correction path?
6. How far in the future is "obviously wrong"? I guessed 24h.
7. Do rejected events need to be queryable later (dead-letter), or is the response enough?
8. Is `event_id` unique per provider or globally? This decides the dedup key long-term.
9. What is the acceptable staleness of the dashboard? It affects whether we can cache stats.
10. Do we ever need to delete a contact's events for privacy? That changes the storage design.

## 5. Priorities and order

1. Correct ingest semantics: per-element decoding, validation, partial success, duplicates as 2xx. This is the part that is expensive to get wrong later.
2. Durable, atomic dedup (SQLite primary key + one transaction per batch), safe under concurrent requests.
3. Stats with a clear definition and guarded rates; 404 for unknown campaigns.
4. Tests for the four behaviours I would be most embarrassed to break (dedup, partial batch, out-of-order, concurrent duplicates), plus the seed.
5. README with runnable curl examples.
6. Paginated event list (optional in the brief) — done because the query is small and "recent activity" is the obvious next dashboard panel.
7. Written parts: scale memo, angry marketer, debugging, AI usage.

## 6. Storage choice: SQLite (`modernc.org/sqlite`, pure Go), not in-memory

- Webhook events are the source of truth for money-adjacent marketing numbers. Losing them on restart is not acceptable, and an in-memory map cannot be inspected or repaired with SQL.
- `event_id PRIMARY KEY` + `INSERT OR IGNORE` makes dedup an atomic database operation, not application logic. Two concurrent requests cannot both win; no read-check-write race.
- One transaction per batch gives all-or-nothing persistence for the batch (validation is done before the transaction), and `RowsAffected` tells me accepted vs duplicate per row without a second query.
- Why not in-memory: it is simpler and has no dependency, but it fails the "provider retried, service restarted, did we keep the number?" test, and every scale step (durable queue, replicas, reconciliation) starts from persistence.
- Why `modernc.org/sqlite`: it is pure Go, so `go run .` works with no C toolchain, which matters for the reviewer's machine. `mattn/go-sqlite3` needs cgo.
- Concurrency: one `*sql.DB`, `MaxOpenConns(1)`, WAL mode, `busy_timeout=5000`. A single writer connection serialises writes (SQLite has one writer anyway), removes `SQLITE_BUSY` retry logic, and at thousands of events/day the read cost of sharing that connection is irrelevant. WAL and the busy timeout are belt-and-braces for when a second read connection is added later.
