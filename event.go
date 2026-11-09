package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The four event types we accept. The comparison is case-sensitive on purpose:
// "OPENED" from a provider is not the same word as our contract, and guessing
// what it meant is how a dashboard silently gets wrong numbers.
const (
	TypeSent      = "sent"
	TypeDelivered = "delivered"
	TypeOpened    = "opened"
	TypeClicked   = "clicked"
)

var allowedTypes = map[string]bool{
	TypeSent:      true,
	TypeDelivered: true,
	TypeOpened:    true,
	TypeClicked:   true,
}

const (
	// maxIDLen stops a single event from using the whole request body budget on
	// one identifier. It is a sanity limit, not a business rule.
	maxIDLen = 200

	// maxFutureSkew is how far ahead of our clock an event timestamp may be
	// before we call it a provider bug instead of clock skew. Late events have no
	// lower bound: being hours or days late is normal in this business.
	maxFutureSkew = 24 * time.Hour

	// tsLayout is a fixed-width UTC timestamp layout. Storing fixed-width text
	// means SQLite's text comparison is chronological comparison, which keeps
	// ORDER BY / keyset pagination simple. RFC3339Nano drops trailing zeros and
	// is therefore *not* safe to sort as text.
	tsLayout = "2006-01-02T15:04:05.000000000Z"
)

// Event is the provider contract. Metadata is kept as raw JSON because we never
// interpret it; all we need is to store it faithfully.
type Event struct {
	EventID    string          `json:"event_id"`
	CampaignID string          `json:"campaign_id"`
	ContactID  string          `json:"contact_id"`
	Type       string          `json:"type"`
	Timestamp  string          `json:"timestamp"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// ValidEvent is a validated, normalised event ready for storage. It only exists
// if validation passed, so the store never has to re-check anything.
type ValidEvent struct {
	EventID    string
	CampaignID string
	ContactID  string
	Type       string
	TS         time.Time
	Metadata   string // compact JSON object, or "" when absent
}

// validate checks one decoded event and normalises it. Errors are written for a
// provider integration engineer, not for us: they say what to send instead.
func validate(ev *Event, now time.Time) (ValidEvent, error) {
	if err := requireID("event_id", ev.EventID); err != nil {
		return ValidEvent{}, err
	}
	if err := requireID("campaign_id", ev.CampaignID); err != nil {
		return ValidEvent{}, err
	}
	if err := requireID("contact_id", ev.ContactID); err != nil {
		return ValidEvent{}, err
	}
	if !allowedTypes[ev.Type] {
		return ValidEvent{}, fmt.Errorf("type %q is not one of sent, delivered, opened, clicked", ev.Type)
	}

	ts, err := time.Parse(time.RFC3339, ev.Timestamp)
	if err != nil {
		return ValidEvent{}, fmt.Errorf("timestamp %q is not RFC3339 (want e.g. 2026-08-10T09:15:04Z)", ev.Timestamp)
	}
	if ts.After(now.Add(maxFutureSkew)) {
		return ValidEvent{}, fmt.Errorf("timestamp %q is more than %s in the future", ev.Timestamp, maxFutureSkew)
	}

	meta, err := normaliseMetadata(ev.Metadata)
	if err != nil {
		return ValidEvent{}, err
	}

	return ValidEvent{
		EventID:    ev.EventID,
		CampaignID: ev.CampaignID,
		ContactID:  ev.ContactID,
		Type:       ev.Type,
		TS:         ts.UTC(),
		Metadata:   meta,
	}, nil
}

func requireID(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(value) > maxIDLen {
		return fmt.Errorf("%s is longer than %d bytes", field, maxIDLen)
	}
	return nil
}

// normaliseMetadata accepts an absent/null metadata and otherwise requires a
// JSON object. A string or array there is a contract violation; storing it as
// opaque bytes would let it fail later, at read time, in front of a marketer.
func normaliseMetadata(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	if trimmed[0] != '{' {
		return "", errors.New("metadata must be a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return "", errors.New("metadata is not valid JSON")
	}
	return buf.String(), nil
}

// peekEventID extracts the id for the error report without caring whether the
// rest of the event is valid. It must never fail loudly: reporting that an
// object has no usable id is the point.
func peekEventID(raw json.RawMessage) string {
	var probe struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.EventID
}

// decodeAndValidate handles exactly one element of the request array. Decoding
// per element is the whole reason a bad row cannot reject its neighbours.
func decodeAndValidate(raw json.RawMessage, now time.Time) (ValidEvent, error) {
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		// Distinguish "not an object" from "a field has the wrong type" so the
		// provider knows whether this is their serialiser or their mapping.
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return ValidEvent{}, fmt.Errorf("field %q has the wrong JSON type (want %s)", typeErr.Field, typeErr.Type)
		}
		return ValidEvent{}, errors.New("event must be a JSON object")
	}
	return validate(&ev, now)
}

// formatTS converts the fixed-width storage form back to normal RFC3339 for API
// responses, so callers see "2026-08-10T09:15:04Z" rather than nine zeros.
func formatTS(s string) string {
	t, err := time.Parse(tsLayout, s)
	if err != nil {
		return s // never expected; better to return what we stored than nothing
	}
	return t.Format(time.RFC3339Nano)
}
