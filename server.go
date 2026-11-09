package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

const (
	// maxBodyBytes caps one webhook request. 1 MiB is roughly 2,000 average
	// events, comfortably above the 1,000-event batch limit, so a valid batch
	// never hits the body cap first.
	maxBodyBytes = 1 << 20

	// maxBatchEvents caps array length. Providers should chunk; silently
	// processing the first 1,000 and dropping the rest would lose data.
	maxBatchEvents = 1000

	defaultPageSize = 50
	maxPageSize     = 200
)

type Server struct {
	store  *Store
	now    func() time.Time
	logger *log.Logger
}

// NewServer wires routes. `now` is injected so timestamp-validation and as_of
// are testable without sleeping or depending on the wall clock.
func NewServer(store *Store, now func() time.Time, logger *log.Logger) http.Handler {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{store: store, now: now, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /events", s.handlePostEvents)
	mux.HandleFunc("GET /campaigns/{campaignID}/stats", s.handleCampaignStats)
	mux.HandleFunc("GET /campaigns/{campaignID}/events", s.handleCampaignEvents)
	return mux
}

type batchResponse struct {
	Received   int          `json:"received"`
	Accepted   int          `json:"accepted"`
	Duplicates int          `json:"duplicates"`
	Rejected   int          `json:"rejected"`
	Errors     []eventError `json:"errors"`
}

type eventError struct {
	Index   int    `json:"index"`
	EventID string `json:"event_id"`
	Reason  string `json:"reason"`
}

// handlePostEvents ingests a JSON array and reports per-element outcomes.
//
// 200 even when some (or all) elements were rejected: an invalid event will
// never become valid by being retried, so a non-2xx would just make the
// provider retry the same poison batch forever. Only a body we cannot interpret
// as an array at all is a 4xx for the whole request.
func (s *Server) handlePostEvents(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var raw []json.RawMessage
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "body must be a JSON array of events: "+err.Error())
		return
	}
	// A second JSON value after the array means the body is not what we think
	// it is; accepting it would silently ignore the rest.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "body must contain exactly one JSON array")
		return
	}
	if raw == nil { // JSON "null" decodes to a nil slice without error
		writeError(w, http.StatusBadRequest, "body must be a JSON array of events")
		return
	}
	if len(raw) > maxBatchEvents {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("batch too large: %d events, maximum is %d", len(raw), maxBatchEvents))
		return
	}

	now := s.now()
	resp := batchResponse{Received: len(raw), Errors: make([]eventError, 0)}
	valid := make([]ValidEvent, 0, len(raw))
	for i, elem := range raw {
		ev, err := decodeAndValidate(elem, now)
		if err != nil {
			resp.Rejected++
			resp.Errors = append(resp.Errors, eventError{
				Index:   i,
				EventID: peekEventID(elem),
				Reason:  err.Error(),
			})
			continue
		}
		valid = append(valid, ev)
	}

	stored, err := s.store.InsertBatch(r.Context(), valid, now)
	if err != nil {
		// Nothing was committed (one transaction per batch), so a provider
		// retry is both safe and the correct action.
		s.logger.Printf("ingest: insert batch: %v", err)
		writeError(w, http.StatusInternalServerError, "could not store events, please retry")
		return
	}
	resp.Accepted = stored.Accepted
	resp.Duplicates = stored.Duplicates

	for _, c := range stored.Conflicts {
		s.logger.Printf("ingest: duplicate event_id %q with different payload; keeping the first write (stored type=%s, incoming type=%s)",
			c.EventID, c.StoredType, c.IncomingType)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCampaignStats(w http.ResponseWriter, r *http.Request) {
	campaignID := r.PathValue("campaignID")
	stats, err := s.store.Stats(r.Context(), campaignID, s.now())
	if errors.Is(err, ErrCampaignNotFound) {
		writeError(w, http.StatusNotFound, "campaign not found")
		return
	}
	if err != nil {
		s.logger.Printf("stats: %v", err)
		writeError(w, http.StatusInternalServerError, "could not compute stats")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

type eventsResponse struct {
	CampaignID string     `json:"campaign_id"`
	Events     []EventOut `json:"events"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

func (s *Server) handleCampaignEvents(w http.ResponseWriter, r *http.Request) {
	campaignID := r.PathValue("campaignID")

	limit := defaultPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("limit must be an integer between 1 and %d", maxPageSize))
			return
		}
		limit = n
	}
	cursor := r.URL.Query().Get("cursor")

	events, next, err := s.store.ListEvents(r.Context(), campaignID, limit, cursor)
	if err != nil {
		if errors.Is(err, errInvalidCursor) {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		s.logger.Printf("events: %v", err)
		writeError(w, http.StatusInternalServerError, "could not list events")
		return
	}
	// Distinguish "campaign does not exist" from "this page is empty".
	if len(events) == 0 && cursor == "" {
		writeError(w, http.StatusNotFound, "campaign not found")
		return
	}
	writeJSON(w, http.StatusOK, eventsResponse{
		CampaignID: campaignID,
		Events:     events,
		NextCursor: next,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // keep metadata URLs readable
	if err := enc.Encode(v); err != nil {
		// The status line is already sent; all we can do is record it.
		log.Printf("writeJSON: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
