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

## Scale memo: 100M events/day (~1,150/s average, much higher at peak)

**What breaks first.** Not the HTTP layer. The single SQLite writer connection: every batch is a serialised transaction on one connection, so ingest throughput has a hard ceiling and one slow batch delays every other provider. Worse, `GET .../stats` runs `COUNT(DISTINCT contact_id) ... GROUP BY type` over the *raw* events on every dashboard poll, on that same connection. Cost grows with history, so the dashboard gets slower every day and starts blocking ingest. Blocked ingest means provider timeouts, and provider retries amplify load exactly when we are weakest. After that: one disk, one machine, WAL/backup growth, and no way to run two instances.

**What I would change, in order.**
1. Measure first: ingest p99, stats p99, queue/lock waits, duplicate rate. Do not guess which wall I hit first.
2. Put a durable queue in front of the webhook; acknowledge quickly and process asynchronously (details below).
3. Bigger multi-row inserts and config tuning buy time, but SQLite is a stopgap at this volume.
4. Pre-aggregate. Keep raw events for audit, and maintain rollups per `(campaign, type, UTC day)`: event totals plus a unique-contact membership table keyed `(campaign, type, contact_id)`. Stats then reads a handful of rows instead of scanning history. The unique table is also the dedup source of truth for "opened".
5. Move storage to Postgres if the team wants transactions + familiar operations, or ClickHouse if analytics/ad-hoc slicing dominates. I would start with Postgres, because counters and reconciliation want transactions, and move heavy analytical reads to ClickHouse later if needed.
6. Partition by time (monthly) and optionally campaign; retention on raw events, keep rollups forever.
7. Cache stats for 30–60s and label freshness with `as_of`. Marketing does not need sub-second numbers; it needs numbers it can trust.

**API contract.** Mostly unchanged: same endpoints, same field names. `POST /events` may return `202` with a batch id instead of immediate counts once ingest is async (or keep 200 with "queued"). Stats should gain a time range and a per-day series, and `as_of` becomes load-bearing. Storage and internals change completely; callers mostly do not.

**Queue: yes, but it is not free.** Kafka/SQS between webhook and processor absorbs spikes, survives restarts, and decouples ingest from aggregation. New problems: lag (dashboard is stale; expose it), no ordering guarantee (we already never depend on order), consumer retries and rebalances (at-least-once, so dedup must be idempotent at the consumer), poison messages (dead-letter queue + alert + replay tool), and more operational surface. Exactly-once delivery is not something I would claim; idempotent writes give the same outcome for our purposes.

**New duplicate sources.** Consumer retries after a timeout or rebalance, producer retries when the ack is lost, replays/backfills of a queue or a day's file, multi-region producers writing to one store, and manual corrections. Mitigation is the same everywhere: one unique key at the final store (`event_id`), an idempotent upsert, and a dedup membership table for unique contacts. Dedup cannot live only in the webhook anymore, because the webhook is no longer the only entry point.

**Keeping counters trustworthy.** Raw events stay append-only and keyed by `event_id`. Rollups are derived, never independently invented. Update rollups in the same transaction as the raw insert where possible; also run a periodic reconciliation that recomputes a trailing window (say yesterday and today) from raw events and compares it with the rollups, alerting on drift and repairing. Version the metric definition (unique vs total opens, bot filtering) and store that version with the rollup, so a definition change is explainable rather than a mystery drop.

**What I would monitor.** Ingest lag (`now - event.timestamp` and `now - received_at`), queue depth and consumer lag, 5xx rate, rejection rate split by reason, duplicate rate, p99 ingest and stats latency, DB write latency and lock waits, counter drift from reconciliation, late-event rate, DLQ size, cache staleness, disk.

**What I would deliberately not solve yet.** End-to-end exactly-once (idempotency is enough), multi-region active-active, sub-second real-time dashboards, per-contact timelines at this volume, and automated GDPR deletion. They are real, but none of them is the first thing that breaks.

## The angry marketer: Delivered 970k → 975k, Opened 250k → 248k

First: **delivered rising is not suspicious.** Late deliveries are the normal case the brief describes; 5k more deliveries in 30 minutes is exactly what a running campaign looks like. **Opened falling is the thing that needs an explanation.** Clicked stayed flat at 20k, which is a useful clue: a genuine loss of engagement data would usually move clicks too.

Plausible explanations, before assuming a bug:
- **Late delivered events** arriving in the window (expected, not a bug).
- **Provider corrections/reclassification**: opens retracted after bot filtering, or Apple Mail Privacy Protection prefetch opens removed; some providers revise events after sending them.
- **Dedup or a backfill/reprocessing job**: duplicate opens removed, or a job recomputed the aggregate.
- **Definition change**: `opened` switched from total events to unique contacts, or started excluding machine opens; a deploy can change semantics without changing the API.
- **Time window / timezone**: the view changed from "campaign to date" to "last 30 minutes", or a timezone/DST boundary moved events out of the window.
- **Different campaign or filter**: a variant/segment filter, or the dashboard is showing a different campaign than the marketer thinks.
- **Cache or read-replica lag**: one poll hit a stale replica or a cached response, the other hit fresh data.
- **Deletion**: bounces, unsubscribes or a GDPR deletion removed contacts/events (unlikely to be exactly 2k, but plausible).
- **An actual bug**: double counting was fixed (opens were inflated before), events were dropped or overwritten, or a pagination/aggregation bug undercounts.

What I would check first, in this order:
1. Query raw events for that campaign with `received_at BETWEEN 10:00 AND 10:30` (arrival time), grouped by `type`. Are opens being *deleted or changed*, and are new delivered events simply arriving?
2. Compare the same aggregate before and after, and compare `counts` vs `total_events`: if unique opens fell but total open events did not, the definition changed; if both fell, data changed.
3. Look at deploys, feature flags, backfill/dedup jobs and cache flushes in that window. A change with no new events is almost always one of these.
4. Check whether the drop is concentrated (one provider, one domain, machine opens) or uniform. Concentration points to a provider correction or a filter; uniform points to a definition/deploy.
5. Query the primary directly and bypass the cache/replica. If the number is 250k there, the pipeline is fine and the read path is stale.

How I would decide bug vs expected:
- New delivered raw events received in the window and opens unchanged in raw → **expected**; explain late delivery.
- Raw opens still present and the same query now returns the old number → **read-path bug** (cache/replica/aggregation); fix and add a freshness indicator.
- Raw opens deleted or mutated → **pipeline bug or deliberate job**; identify the actor from logs, restore if wrong, and add an audit trail.
- Opens unchanged in raw but definition/version changed → **expected product change**; say so, show both numbers, and version the metric.
- No new events, no deploy, no job, number still differs between two identical queries → **bug**, and the first thing to fix is reproducibility, not the number.

## What I completed / what I intentionally skipped

Completed:
- Read the PDF/assignment and audited the seed by parsing it, not by sampling.
- `NOTES.md` Part 1 (interpretation, assumptions, ambiguities, PM questions, priorities, storage choice).
- The service: per-element validation, partial-batch success, atomic `event_id` dedup, durable SQLite storage, stats with unique contacts + raw totals + guarded rates, and optional cursor-paginated events.
- Tests for the behaviours I would be most embarrassed to break, including `-race` concurrency and the full seed, plus an end-to-end curl run.
- The debugging exercise: four minimal fixes, 20/20 deterministic runs, clean `-race`, and `BUGS.md`.
- `README.md` with run commands and curl examples.

Intentionally skipped:
- Auth, Docker, deployment, UI (explicitly out of scope).
- A campaign table/catalog, campaign creation, and validation of campaign ids against it.
- Time-range/daily-series stats, which a real dashboard will want next.
- Retention, GDPR/contact deletion, and deleting events.
- Persisting rejected events in a dead-letter table and a replay tool (the response reports them; that is all).
- Schema migrations/versioning, multi-instance operation, load testing at 100M/day, fuzz tests, metrics/tracing endpoints.

## If I had another day

1. Time-window stats (`from`/`to`, per-day series) and an explicit `definition_version` in the response.
2. A dead-letter table for rejected events plus a replay command, so bad provider data is inspectable instead of only reported.
3. Reconciliation job: recompute rollups from raw events and alert on drift — the precondition for trusting counters at scale.
4. Property/fuzz tests for validation and pagination, and a load test at the projected peak.
5. Prometheus metrics for the signals listed in the scale memo, with `as_of`/lag exposed to the dashboard.
6. Schema migrations, a retention job, and a small campaign catalog so an empty campaign is distinguishable from a typo.
