# Debugging `debugging/` — four bugs

All fixes are minimal and in [debugging/main.go](debugging/main.go). Verification for every bug ends with the same command, run 20 times:

```bash
cd debugging
go run . events.jsonl > actual.txt
diff actual.txt expected_output.txt   # empty, every run
```

Baseline before any fix: `sent` was too high everywhere (`cmp_A` 2025 vs 1987), `unique_opens` far too low (`cmp_A` 549 vs 942), and there were spurious `2026-08-08` delivered buckets.

## Bug 1 — de-duplication only worked inside one batch

**What.** Duplicate `event_id`s were removed, but only if both copies happened to fall in the same 200-line batch. Every counter was inflated because retries spread across batches were counted again.

**Why.** `seen := make(map[string]bool)` was created inside `processBatch`, so it was thrown away after each batch. The required scope is the whole file. The comment above `openedBy` is a hint: it is deliberately package-level; `seen` should have been too.

**Fix.** Moved `seen` to package level next to `openedBy`, and deleted the local declaration. `processBatch` runs on the main goroutine (workers are only started after de-duplication), so no lock is needed.

**Verified.** `cmp_A sent` went from 2025 to 1987, matching expected; 20/20 runs diff clean.

## Bug 2 — `unique_opens` keyed only by contact, not campaign

**What.** A contact who opened campaigns A and B counted as one unique opener in total, instead of once in each.

**Why.** `openedBy` was `map[string]bool` indexed by `ev.ContactID`. The spec says "distinct contacts that opened, per campaign". `CampaignStats` is already per campaign, so the key has to carry the campaign too.

**Fix.** `key := ev.CampaignID + "\x00" + ev.ContactID` before the map lookup. The NUL separator stops `("ab","c")` and `("a","bc")` colliding.

**Verified.** `cmp_A unique_opens` went from 549 to 942, `cmp_B` 441 → 833, all matching expected; 20/20 runs diff clean.

## Bug 3 — daily buckets used local time instead of UTC

**What.** `delivered` was bucketed by the machine's local date. On a non-UTC machine, a delivery at 23:30 UTC lands on the next day locally, creating an extra day and shifting counts out of the real day.

**Why.** `ev.Timestamp.Local().Format("2006-01-02")`. The README says "Daily `delivered` buckets use the event's UTC date. The `timestamp` field is UTC."

**Fix.** `ev.Timestamp.UTC().Format("2006-01-02")`.

**Verified.** The bogus `2026-08-08` buckets disappeared and per-day numbers match expected. This bug can be invisible on a UTC machine (CI often is), so it is worth running with `TZ=Asia/Kolkata` too.

## Bug 4 — data race on the counters

**What.** Eight worker goroutines call `apply`, which does `cs.Sent++` (and the other three counters) on shared `int` fields. Increments are read-modify-write, not atomic. The race detector reports concurrent writes on the same address, and in principle the program can lose counts.

**Why.** The worker pool was added for speed, but the counters are plain ints with no synchronisation. The comment "the increments themselves are cheap" is true but irrelevant; cheap is not safe. `track()` is safe because it runs before the workers start, which is why only `apply` needs the lock.

**Fix.** Added `var mu sync.Mutex` and `mu.Lock(); defer mu.Unlock()` around the switch in `apply`. This is the minimal fix; `atomic.Int64` would also work but changes four field types and the report code.

**Verified.** With only those two lines removed, `go run -race . events.jsonl` prints `WARNING: DATA RACE` with `main.apply` on both stacks. With the fix, five consecutive `go run -race` runs produced expected output and an empty stderr. Plain runs: 20/20 identical.
