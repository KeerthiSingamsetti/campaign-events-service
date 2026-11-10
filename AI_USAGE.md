# AI usage

Being direct: this submission was built with an AI coding agent, and the design decisions were reviewed and defended as I went. The assignment allows heavy AI use; the follow-up interview does not, so this file records what was AI-assisted and what I verified myself.

## Tools used

- **AntiGravity and Claude-based** inside the editor: most of the first draft of the Go service, tests, README, NOTES and BUGS.md, plus running the commands and iterating on failures.
- **`pdftotext`** to extract the text of `Engineering Assignment.pdf` (so the PDF and `ASSIGNMENT.md` could both be read in full).
- **Node.js one-off scripts** (already on the machine) to parse `seed/events.json` and count duplicates, invalid rows, unknown types and per-campaign expected numbers. No AI was needed for the arithmetic; it was faster than eyeballing 235 rows.
- **A local Go 1.27 toolchain** and a portable clang, installed under the gitignored `.tools/` because this machine had neither Go nor a C compiler. `-race` needs cgo, hence the clang.

No ChatGPT, Copilot or Cursor was used in this session.

## What the AI was used for

- Scaffolding the stdlib HTTP handlers, the SQLite store and the tests.
- Enumerating seed anomalies and suggesting which validation rules follow from them.
- Drafting the written sections from my decisions, then rewriting them into plain English.
- Running `gofmt`, `go vet`, `go test -race`, the 20-run diff loop, and the curl end-to-end check, and interpreting the output.

## One suggestion I rejected or changed

The agent's first storage suggestion was an **in-memory map guarded by `sync.RWMutex`**, justified by "the brief allows it and it removes a dependency". I rejected it: these events are the source of truth for numbers a marketer acts on, and an in-memory store loses everything on restart, cannot be inspected with SQL, and cannot deduplicate across processes. A webhook receiver whose data disappears on deploy is not a defensible design at any scale. I kept SQLite with `modernc.org/sqlite` (pure Go, so no cgo for normal runs).

Two smaller changes: the agent initially returned rates unclamped and only `counts` (no `total_events`). I changed that to nullable, clamped rates **plus** raw totals, because "opened: 19" and "27 open events" are different facts and the marketer scenario shows why conflating them causes support tickets. I also rejected a "process the first 1,000 events and report the rest as dropped" shortcut for oversized batches; silent loss is worse than a 400.

## One thing AI helped me understand

Why `RFC3339Nano` timestamps must not be stored as sortable text: trailing zeros are trimmed, so `...:04Z` and `...:04.5Z` compare the wrong way round (`'Z' > '.'`). Storing a **fixed-width** UTC layout (`2006-01-02T15:04:05.000000000Z`) makes lexicographic order equal chronological order, which is what makes both `ORDER BY ts, event_id` and keyset pagination correct. I had the intuition that "RFC3339 sorts fine"; the counter-example is what made it click.

## AI-generated things I had to debug

- **Pagination off-by-one.** The cursor was built from the lookahead row (`LIMIT n+1`), so every page skipped one event. The seed pagination test caught it: 112 events walked instead of 114. Fixed by seeding the cursor only from rows inside the page, and the test now pins the total.
- **A wrong test expectation.** I initially asserted that re-posting the seed yields 210 duplicates. It is 228: all 228 valid rows are duplicates on the second pass, including the 18 rows that lost the first time. The code was right and the assertion was wrong.
- **A `gofmt -l .` that swept the downloaded Go toolchain** in `.tools/` and printed hundreds of unrelated files (and `go vet` errors from Go's own testdata). Scoped to `*.go debugging/*.go`.
- **A truncated notes append** (shell heredoc limit) that cut off the close-out section; repaired by editing the file directly.

## What I verified rather than trusted

- The seed numbers in the README and tests come from parsing the file, not from the service's own output: 235 rows, 18 extra duplicate rows → 217 distinct ids; 7 of those are invalid → 210 accepted.
- `go test -race` passes; the race in the debugging exercise was reproduced by removing only the mutex, and then fixed.
- 20/20 runs of the debugging program match `expected_output.txt`; 5/5 under `-race`.
- The service was run for real and exercised with curl, including the seed, stats, pagination and a 400 body.

The honest risk: AI wrote most of the first draft, so the interview is where I have to prove I understand it. The parts I would rehearse are the dedup transaction, why the timestamp layout is fixed-width, why the single writer connection is safe, and the difference between `counts` and `total_events`.
