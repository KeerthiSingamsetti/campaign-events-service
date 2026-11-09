# Debugging exercise

`main.go` reads `events.jsonl` (20,000 lines, one JSON event per line) and prints per-campaign statistics. It compiles, runs, and produces plausible-looking output. It is wrong. There are **four bugs**. None is a syntax error.

## What the program is supposed to do

1. **Count each `event_id` at most once across the whole file.** Providers retry; the input contains duplicate lines. A duplicate must not change any number.
2. **`unique_opens` = the number of distinct contacts that opened, per campaign.** One contact opening five times in a campaign counts once. The same contact opening in two campaigns counts once *in each*.
3. **Daily `delivered` buckets use the event's UTC date.** The `timestamp` field is UTC.
4. **Output is identical on every run** for the same input.

## Ground truth

`expected_output.txt` is the exact stdout of a correct version. When all four bugs are fixed:

```bash
go run . events.jsonl > actual.txt
diff actual.txt expected_output.txt   # must be empty, run after run
```

## Rules

- Make **minimal fixes**. Do not rewrite or restructure the program. If your diff touches most of the file, you've replaced the code, not debugged it.
- Record each bug in `BUGS.md` at the repo root: what it is, why it happens, your fix, and how you verified the fix.
- At least one bug may not show up in the numbers at all on your machine. It is still a bug against the spec above. `go run -race . events.jsonl` is your friend.

Start by running the program and comparing its output with the expected output. The differences are not random; each has a cause.
