package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver: no cgo, so `go run .` just works
)

// ErrCampaignNotFound means the campaign has no stored events. There is no
// campaign table in this service: a campaign exists because events mention it.
var ErrCampaignNotFound = errors.New("campaign not found")

var errInvalidCursor = errors.New("invalid cursor")

// Counts is one number per event type. Used twice: unique contacts and raw
// event volume. Keeping them in the same shape makes the JSON self-describing.
type Counts struct {
	Sent      int `json:"sent"`
	Delivered int `json:"delivered"`
	Opened    int `json:"opened"`
	Clicked   int `json:"clicked"`
}

// Rates is deliberately nullable: a nil pointer marshals to JSON null, which is
// the honest answer when the denominator is zero ("we cannot compute this yet").
type Rates struct {
	DeliveredOfSent   *float64 `json:"delivered_of_sent"`
	OpenedOfDelivered *float64 `json:"opened_of_delivered"`
	ClickedOfOpened   *float64 `json:"clicked_of_opened"`
}

type Stats struct {
	CampaignID   string  `json:"campaign_id"`
	Counts       Counts  `json:"counts"`       // unique contacts per type
	TotalEvents  Counts  `json:"total_events"` // raw events per type
	Rates        Rates   `json:"rates"`
	FirstEventAt *string `json:"first_event_at"` // by event timestamp, not arrival
	LastEventAt  *string `json:"last_event_at"`
	AsOf         string  `json:"as_of"`
}

type EventOut struct {
	EventID    string          `json:"event_id"`
	CampaignID string          `json:"campaign_id"`
	ContactID  string          `json:"contact_id"`
	Type       string          `json:"type"`
	Timestamp  string          `json:"timestamp"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	ReceivedAt string          `json:"received_at"`
}

// Conflict describes a retry whose payload does not match what we stored first.
// It is still a duplicate: we never overwrite, we only make the difference
// visible in the log.
type Conflict struct {
	EventID      string
	StoredType   string
	IncomingType string
}

type BatchResult struct {
	Accepted   int
	Duplicates int
	Conflicts  []Conflict
}

type Store struct {
	db *sql.DB
}

// OpenStore opens (creating if needed) the SQLite database and applies the
// schema. One writer connection on purpose: SQLite allows one writer at a time
// anyway, and a single connection removes SQLITE_BUSY retry logic entirely.
// WAL and a busy timeout are set as defence for the day a second read
// connection is added.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS events (
			event_id    TEXT PRIMARY KEY,
			campaign_id TEXT NOT NULL,
			contact_id  TEXT NOT NULL,
			type        TEXT NOT NULL,
			ts          TEXT NOT NULL,
			metadata    TEXT,
			received_at TEXT NOT NULL
		)`,
		// Required index: serves unique-contact counts and per-type filtering.
		`CREATE INDEX IF NOT EXISTS idx_events_campaign_type_contact
			ON events (campaign_id, type, contact_id)`,
		// Pagination index: ORDER BY ts, event_id is a straight index scan.
		`CREATE INDEX IF NOT EXISTS idx_events_campaign_ts
			ON events (campaign_id, ts, event_id)`,
	}
	for _, q := range statements {
		if _, err := db.ExecContext(ctx, q); err != nil {
			db.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func dsn(path string) string {
	if path == "" {
		path = "events.db"
	}
	if path == ":memory:" {
		return "file::memory:?_pragma=busy_timeout(5000)"
	}
	// filepath.ToSlash keeps the SQLite URI valid on Windows.
	return "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}

func (s *Store) Close() error { return s.db.Close() }

// InsertBatch writes every validated event in one transaction. `INSERT OR
// IGNORE` plus the PRIMARY KEY is what makes dedup atomic: two concurrent
// requests cannot both "check then insert" the same id.
//
// If the transaction fails we return an error and store nothing; the caller
// answers 5xx so the provider retries the whole batch. That retry is safe
// *because* writes are idempotent.
func (s *Store) InsertBatch(ctx context.Context, events []ValidEvent, receivedAt time.Time) (BatchResult, error) {
	var result BatchResult
	if len(events) == 0 {
		return result, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback() // no-op after Commit

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO events
			(event_id, campaign_id, contact_id, type, ts, metadata, received_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return result, err
	}
	defer stmt.Close()

	received := receivedAt.UTC().Format(tsLayout)
	for _, ev := range events {
		var metadata any
		if ev.Metadata != "" {
			metadata = ev.Metadata
		}
		res, err := stmt.ExecContext(ctx,
			ev.EventID, ev.CampaignID, ev.ContactID, ev.Type,
			ev.TS.UTC().Format(tsLayout), metadata, received)
		if err != nil {
			return BatchResult{}, fmt.Errorf("insert %s: %w", ev.EventID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return BatchResult{}, err
		}
		if n == 1 {
			result.Accepted++
			continue
		}

		// Ignored: the id already exists. Count it as a duplicate and check
		// whether the retry disagrees with the first write so we can warn.
		result.Duplicates++
		stored, err := storedPayload(ctx, tx, ev.EventID)
		if err != nil {
			return BatchResult{}, err
		}
		if stored.disagreesWith(ev) {
			result.Conflicts = append(result.Conflicts, Conflict{
				EventID:      ev.EventID,
				StoredType:   stored.typ,
				IncomingType: ev.Type,
			})
		}
	}

	if err := tx.Commit(); err != nil {
		return BatchResult{}, err
	}
	return result, nil
}

type payload struct {
	campaignID, contactID, typ, ts, metadata string
}

func (p payload) disagreesWith(ev ValidEvent) bool {
	return p.campaignID != ev.CampaignID ||
		p.contactID != ev.ContactID ||
		p.typ != ev.Type ||
		p.ts != ev.TS.UTC().Format(tsLayout) ||
		p.metadata != ev.Metadata
}

func storedPayload(ctx context.Context, tx *sql.Tx, eventID string) (payload, error) {
	var p payload
	err := tx.QueryRowContext(ctx,
		`SELECT campaign_id, contact_id, type, ts, COALESCE(metadata, '')
		   FROM events WHERE event_id = ?`, eventID).
		Scan(&p.campaignID, &p.contactID, &p.typ, &p.ts, &p.metadata)
	if err != nil {
		return payload{}, fmt.Errorf("read stored event %s: %w", eventID, err)
	}
	return p, nil
}

// Stats aggregates one campaign in a single query.
//
// "opened" is COUNT(DISTINCT contact_id), i.e. unique openers, not open events.
// It never requires a delivered row first: an open that arrived before its
// delivered is normal, and the two are independent facts about the message.
func (s *Store) Stats(ctx context.Context, campaignID string, asOf time.Time) (Stats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT type,
		       COUNT(DISTINCT contact_id),
		       COUNT(*),
		       MIN(ts),
		       MAX(ts)
		  FROM events
		 WHERE campaign_id = ?
		 GROUP BY type`, campaignID)
	if err != nil {
		return Stats{}, err
	}
	defer rows.Close()

	stats := Stats{
		CampaignID: campaignID,
		AsOf:       asOf.UTC().Format(time.RFC3339),
	}
	var first, last sql.NullString
	found := false
	for rows.Next() {
		found = true
		var typ string
		var unique, total int
		var minTS, maxTS string
		if err := rows.Scan(&typ, &unique, &total, &minTS, &maxTS); err != nil {
			return Stats{}, err
		}
		setCount(&stats.Counts, typ, unique)
		setCount(&stats.TotalEvents, typ, total)
		if !first.Valid || minTS < first.String {
			first = sql.NullString{String: minTS, Valid: true}
		}
		if !last.Valid || maxTS > last.String {
			last = sql.NullString{String: maxTS, Valid: true}
		}
	}
	if err := rows.Err(); err != nil {
		return Stats{}, err
	}
	if !found {
		return Stats{}, ErrCampaignNotFound
	}

	firstText, lastText := formatTS(first.String), formatTS(last.String)
	stats.FirstEventAt = &firstText
	stats.LastEventAt = &lastText
	stats.Rates = Rates{
		DeliveredOfSent:   ratio(stats.Counts.Delivered, stats.Counts.Sent),
		OpenedOfDelivered: ratio(stats.Counts.Opened, stats.Counts.Delivered),
		ClickedOfOpened:   ratio(stats.Counts.Clicked, stats.Counts.Opened),
	}
	return stats, nil
}

func setCount(c *Counts, typ string, n int) {
	switch typ {
	case TypeSent:
		c.Sent = n
	case TypeDelivered:
		c.Delivered = n
	case TypeOpened:
		c.Opened = n
	case TypeClicked:
		c.Clicked = n
	}
}

// ratio returns null for a zero denominator, and clamps at 1.0.
//
// Clamping matters because the counts are independent: an open can be stored
// before its delivered, or a provider can send opens we never saw a delivery
// for, so opened/delivered can legitimately exceed 100% for a while. A
// dashboard showing "143% opened" reads as broken; the raw counts are still in
// the response, so anyone diagnosing the anomaly can see it there.
func ratio(numerator, denominator int) *float64 {
	if denominator <= 0 {
		return nil
	}
	r := float64(numerator) / float64(denominator)
	if r > 1 {
		r = 1
	}
	if r < 0 {
		r = 0
	}
	return ptrFloat(math.Round(r*10000) / 10000)
}

func ptrFloat(v float64) *float64 { return &v }

// ListEvents returns one page ordered by (ts, event_id) and an opaque cursor
// for the next page. Keyset pagination is used instead of OFFSET: it stays
// correct while new events are being inserted at the same time, which is the
// normal state of this table.
func (s *Store) ListEvents(ctx context.Context, campaignID string, limit int, cursor string) ([]EventOut, string, error) {
	query := `SELECT event_id, campaign_id, contact_id, type, ts,
	                 COALESCE(metadata, ''), received_at
	            FROM events WHERE campaign_id = ?`
	args := []any{campaignID}

	if cursor != "" {
		ts, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		query += ` AND (ts > ? OR (ts = ? AND event_id > ?))`
		args = append(args, ts, ts, id)
	}
	query += ` ORDER BY ts ASC, event_id ASC LIMIT ?`
	args = append(args, limit+1) // +1 tells us whether another page exists

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	events := make([]EventOut, 0, limit)
	var lastTS, lastID string
	for rows.Next() {
		var e EventOut
		var ts string
		var metadata string
		if err := rows.Scan(&e.EventID, &e.CampaignID, &e.ContactID, &e.Type,
			&ts, &metadata, &e.ReceivedAt); err != nil {
			return nil, "", err
		}
		e.Timestamp = formatTS(ts)
		e.ReceivedAt = formatTS(e.ReceivedAt)
		if metadata != "" {
			e.Metadata = json.RawMessage(metadata)
		}
		// Only rows that belong to this page may seed the next cursor. The
		// extra row exists purely to answer "is there another page?".
		if len(events) < limit {
			lastTS, lastID = ts, e.EventID
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	next := ""
	if len(events) > limit {
		events = events[:limit]
		next = encodeCursor(lastTS, lastID)
	}
	return events, next, nil
}

// The cursor is base64url(JSON [ts, event_id]) so callers cannot construct one
// by hand and accidentally skip or repeat rows. It is not signed: it only
// controls reading within a campaign the caller already names.
func encodeCursor(ts, eventID string) string {
	raw, _ := json.Marshal([2]string{ts, eventID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(cursor string) (string, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", errInvalidCursor
	}
	var parts [2]string
	if err := json.Unmarshal(raw, &parts); err != nil || parts[0] == "" || parts[1] == "" {
		return "", "", errInvalidCursor
	}
	if _, err := time.Parse(tsLayout, parts[0]); err != nil {
		return "", "", errInvalidCursor
	}
	return parts[0], parts[1], nil
}
