package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedNow keeps timestamp validation and as_of deterministic. The seed is from
// 2026-08-10, so a day later is a safe "now" for most tests.
var fixedNow = func() time.Time { return time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC) }

func newTestServer(t *testing.T, now func() time.Time) *httptest.Server {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srv := httptest.NewServer(NewServer(store, now, log.New(io.Discard, "", 0)))
	t.Cleanup(srv.Close)
	return srv
}

// post sends a raw body and returns the status plus the decoded summary.
func post(t *testing.T, srv *httptest.Server, body string) (int, batchResponse) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/events", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /events: %v", err)
	}
	defer resp.Body.Close()
	var summary batchResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
			t.Fatalf("decode summary: %v", err)
		}
	}
	return resp.StatusCode, summary
}

func getStats(t *testing.T, srv *httptest.Server, campaignID string) (int, Stats) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/campaigns/" + campaignID + "/stats")
	if err != nil {
		t.Fatalf("GET stats: %v", err)
	}
	defer resp.Body.Close()
	var stats Stats
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
	}
	return resp.StatusCode, stats
}

func event(id, campaign, contact, typ, ts string) string {
	return fmt.Sprintf(`{"event_id":%q,"campaign_id":%q,"contact_id":%q,"type":%q,"timestamp":%q}`,
		id, campaign, contact, typ, ts)
}

func array(events ...string) string { return "[" + strings.Join(events, ",") + "]" }

func TestBatchPartialSuccess(t *testing.T) {
	srv := newTestServer(t, fixedNow)
	body := array(
		event("evt_ok1", "cmp_x", "ct_1", "sent", "2026-08-10T09:00:00Z"),
		`{"event_id":"evt_nocontact","campaign_id":"cmp_x","type":"sent","timestamp":"2026-08-10T09:00:00Z"}`,
		event("evt_badtype", "cmp_x", "ct_1", "spam_report", "2026-08-10T09:00:00Z"),
		event("evt_badtime", "cmp_x", "ct_1", "sent", "not-a-time"),
		`{"event_id":"evt_badmeta","campaign_id":"cmp_x","contact_id":"ct_1","type":"sent","timestamp":"2026-08-10T09:00:00Z","metadata":"nope"}`,
		event("evt_ok2", "cmp_x", "ct_2", "sent", "2026-08-10T09:05:00Z"),
	)

	status, summary := post(t, srv, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if summary.Received != 6 || summary.Accepted != 2 || summary.Duplicates != 0 || summary.Rejected != 4 {
		t.Fatalf("summary = %+v, want received=6 accepted=2 duplicates=0 rejected=4", summary)
	}
	gotIndexes := make([]int, len(summary.Errors))
	for i, e := range summary.Errors {
		gotIndexes[i] = e.Index
		if e.Reason == "" {
			t.Errorf("error at index %d has no reason", e.Index)
		}
	}
	want := []int{1, 2, 3, 4}
	if fmt.Sprint(gotIndexes) != fmt.Sprint(want) {
		t.Fatalf("error indexes = %v, want %v", gotIndexes, want)
	}

	// The two good events must still be there.
	_, stats := getStats(t, srv, "cmp_x")
	if stats.TotalEvents.Sent != 2 || stats.Counts.Sent != 2 {
		t.Fatalf("stats = %+v, want 2 sent events from 2 contacts", stats)
	}
}

func TestDuplicateIsNotDoubleCounted(t *testing.T) {
	srv := newTestServer(t, fixedNow)
	body := array(
		event("evt_d1", "cmp_x", "ct_1", "sent", "2026-08-10T09:00:00Z"),
		event("evt_d2", "cmp_x", "ct_1", "opened", "2026-08-10T09:10:00Z"),
	)

	if status, first := post(t, srv, body); status != http.StatusOK || first.Accepted != 2 {
		t.Fatalf("first post: status=%d summary=%+v", status, first)
	}
	status, second := post(t, srv, body)
	if status != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 so the provider stops retrying", status)
	}
	if second.Accepted != 0 || second.Duplicates != 2 || second.Rejected != 0 {
		t.Fatalf("retry summary = %+v, want accepted=0 duplicates=2 rejected=0", second)
	}
	if len(second.Errors) != 0 {
		t.Fatalf("duplicates must not be reported as errors: %+v", second.Errors)
	}

	_, stats := getStats(t, srv, "cmp_x")
	if stats.TotalEvents.Sent != 1 || stats.TotalEvents.Opened != 1 {
		t.Fatalf("stats = %+v, want one sent and one opened event", stats.TotalEvents)
	}
}

func TestDuplicateWithDifferentPayloadKeepsFirstWrite(t *testing.T) {
	srv := newTestServer(t, fixedNow)
	if status, _ := post(t, srv, array(
		event("evt_sameid", "cmp_x", "ct_1", "sent", "2026-08-10T09:00:00Z"),
	)); status != http.StatusOK {
		t.Fatalf("first post status = %d", status)
	}
	status, summary := post(t, srv, array(
		event("evt_sameid", "cmp_x", "ct_1", "opened", "2026-08-10T09:00:00Z"),
	))
	if status != http.StatusOK || summary.Duplicates != 1 || summary.Accepted != 0 {
		t.Fatalf("conflicting retry: status=%d summary=%+v", status, summary)
	}
	_, stats := getStats(t, srv, "cmp_x")
	if stats.TotalEvents.Sent != 1 || stats.TotalEvents.Opened != 0 {
		t.Fatalf("first write must win, got %+v", stats.TotalEvents)
	}
}

func TestOpenedBeforeDeliveredCountsCorrectly(t *testing.T) {
	srv := newTestServer(t, fixedNow)
	// Opened is ingested first; delivered for the same message arrives later.
	if status, _ := post(t, srv, array(
		event("evt_o1", "cmp_y", "ct_1", "opened", "2026-08-10T09:10:00Z"),
	)); status != http.StatusOK {
		t.Fatalf("first post status = %d", status)
	}
	if status, _ := post(t, srv, array(
		event("evt_d1", "cmp_y", "ct_1", "delivered", "2026-08-10T09:05:00Z"),
	)); status != http.StatusOK {
		t.Fatalf("second post status = %d", status)
	}

	_, stats := getStats(t, srv, "cmp_y")
	if stats.Counts.Opened != 1 || stats.Counts.Delivered != 1 {
		t.Fatalf("counts = %+v, want one unique opener and one delivered contact", stats.Counts)
	}
	if stats.Rates.OpenedOfDelivered == nil || *stats.Rates.OpenedOfDelivered != 1 {
		t.Fatalf("opened_of_delivered = %v, want 1", stats.Rates.OpenedOfDelivered)
	}
	if stats.Rates.DeliveredOfSent != nil {
		t.Fatalf("delivered_of_sent = %v, want null (no sends recorded)", *stats.Rates.DeliveredOfSent)
	}
	// first_event_at must follow the event timestamp, not arrival order.
	if stats.FirstEventAt == nil || *stats.FirstEventAt != "2026-08-10T09:05:00Z" {
		t.Fatalf("first_event_at = %v, want the delivered timestamp", stats.FirstEventAt)
	}
}

func TestUniqueContactsAndRateClamping(t *testing.T) {
	srv := newTestServer(t, fixedNow)
	body := array(
		event("e1", "cmp_z", "ct_1", "sent", "2026-08-10T09:00:00Z"),
		event("e2", "cmp_z", "ct_2", "sent", "2026-08-10T09:00:00Z"),
		event("e3", "cmp_z", "ct_3", "sent", "2026-08-10T09:00:00Z"),
		event("e4", "cmp_z", "ct_1", "delivered", "2026-08-10T09:01:00Z"),
		event("e5", "cmp_z", "ct_2", "delivered", "2026-08-10T09:01:00Z"),
		// Three openers for two deliveries: >100% is possible with out-of-order
		// and independent provider reports, so the rate is clamped.
		event("e6", "cmp_z", "ct_1", "opened", "2026-08-10T09:02:00Z"),
		event("e7", "cmp_z", "ct_2", "opened", "2026-08-10T09:02:00Z"),
		event("e8", "cmp_z", "ct_3", "opened", "2026-08-10T09:02:00Z"),
		// ct_1 opens again: unique openers must stay at 3.
		event("e9", "cmp_z", "ct_1", "opened", "2026-08-10T09:03:00Z"),
	)
	if status, _ := post(t, srv, body); status != http.StatusOK {
		t.Fatalf("post status = %d", status)
	}

	_, stats := getStats(t, srv, "cmp_z")
	if stats.Counts.Sent != 3 || stats.Counts.Delivered != 2 || stats.Counts.Opened != 3 || stats.Counts.Clicked != 0 {
		t.Fatalf("unique counts = %+v", stats.Counts)
	}
	if stats.TotalEvents.Opened != 4 {
		t.Fatalf("opened total_events = %d, want 4 raw opens", stats.TotalEvents.Opened)
	}
	if got := *stats.Rates.DeliveredOfSent; got != 0.6667 {
		t.Fatalf("delivered_of_sent = %v, want 0.6667", got)
	}
	if got := *stats.Rates.OpenedOfDelivered; got != 1 {
		t.Fatalf("opened_of_delivered = %v, want clamped to 1", got)
	}
	if stats.Rates.ClickedOfOpened == nil || *stats.Rates.ClickedOfOpened != 0 {
		t.Fatalf("clicked_of_opened = %v, want 0", stats.Rates.ClickedOfOpened)
	}
}

func TestConcurrentDuplicatePostsCountedOnce(t *testing.T) {
	srv := newTestServer(t, fixedNow)
	body := array(event("evt_race", "cmp_r", "ct_1", "sent", "2026-08-10T09:00:00Z"))

	const workers = 20
	var wg sync.WaitGroup
	summaries := make([]batchResponse, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, summary := post(t, srv, body)
			if status != http.StatusOK {
				t.Errorf("worker %d: status = %d", i, status)
			}
			summaries[i] = summary
		}(i)
	}
	wg.Wait()

	accepted, duplicates := 0, 0
	for _, s := range summaries {
		accepted += s.Accepted
		duplicates += s.Duplicates
	}
	if accepted != 1 || duplicates != workers-1 {
		t.Fatalf("accepted=%d duplicates=%d, want 1 and %d", accepted, duplicates, workers-1)
	}
	_, stats := getStats(t, srv, "cmp_r")
	if stats.TotalEvents.Sent != 1 || stats.Counts.Sent != 1 {
		t.Fatalf("stats = %+v, want exactly one stored event", stats)
	}
}

func TestFutureTimestampRejected(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	srv := newTestServer(t, func() time.Time { return now })

	status, summary := post(t, srv, array(
		event("evt_future", "cmp_f", "ct_1", "sent", now.Add(25*time.Hour).Format(time.RFC3339)),
		event("evt_skew", "cmp_f", "ct_2", "sent", now.Add(time.Hour).Format(time.RFC3339)),
	))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if summary.Accepted != 1 || summary.Rejected != 1 || summary.Errors[0].EventID != "evt_future" {
		t.Fatalf("summary = %+v, want the 25h-ahead event rejected", summary)
	}
}

func TestMalformedBodies(t *testing.T) {
	srv := newTestServer(t, fixedNow)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty", "", http.StatusBadRequest},
		{"not json", "hello", http.StatusBadRequest},
		{"object not array", `{"event_id":"x"}`, http.StatusBadRequest},
		{"null not array", `null`, http.StatusBadRequest},
		{"truncated array", `[{"event_id":"a"}`, http.StatusBadRequest},
		{"two values", `[] []`, http.StatusBadRequest},
		{"too many events", "[" + strings.Repeat(`{"event_id":"x"},`, maxBatchEvents) + `{"event_id":"y"}]`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := post(t, srv, tc.body)
			if status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}
		})
	}
}

func TestBodyTooLarge(t *testing.T) {
	srv := newTestServer(t, fixedNow)
	huge := `["` + strings.Repeat("a", maxBodyBytes) + `"]`
	status, _ := post(t, srv, huge)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}
}

func TestUnknownCampaignReturns404(t *testing.T) {
	srv := newTestServer(t, fixedNow)
	if status, _ := getStats(t, srv, "cmp_nope"); status != http.StatusNotFound {
		t.Fatalf("stats status = %d, want 404", status)
	}
	resp, err := http.Get(srv.URL + "/campaigns/cmp_nope/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("events status = %d, want 404", resp.StatusCode)
	}
}

// TestSeedFileLoads pins down the actual mess in starter/seed/events.json:
// 235 rows, 7 invalid, 18 retries of 16 ids, so 210 unique events are stored.
func TestSeedFileLoads(t *testing.T) {
	seed, err := os.ReadFile(filepath.Join("seed", "events.json"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	srv := newTestServer(t, fixedNow)

	status, summary := post(t, srv, string(seed))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if summary.Received != 235 || summary.Accepted != 210 || summary.Duplicates != 18 || summary.Rejected != 7 {
		t.Fatalf("seed summary = %+v, want 235/210/18/7", summary)
	}
	for _, e := range summary.Errors {
		if e.Reason == "" {
			t.Errorf("rejected event %d has no reason", e.Index)
		}
	}

	_, stats := getStats(t, srv, "cmp_summer_sale")
	wantCounts := Counts{Sent: 40, Delivered: 35, Opened: 19, Clicked: 12}
	if stats.Counts != wantCounts {
		t.Fatalf("unique counts = %+v, want %+v", stats.Counts, wantCounts)
	}
	wantTotals := Counts{Sent: 40, Delivered: 35, Opened: 27, Clicked: 12}
	if stats.TotalEvents != wantTotals {
		t.Fatalf("total events = %+v, want %+v", stats.TotalEvents, wantTotals)
	}

	// Re-posting the whole seed changes nothing. Every one of the 228 valid
	// rows is now a duplicate, including the 18 rows that lost the first time.
	_, second := post(t, srv, string(seed))
	if second.Accepted != 0 || second.Duplicates != 228 || second.Rejected != 7 {
		t.Fatalf("seed retry summary = %+v, want 0 accepted / 228 duplicates / 7 rejected", second)
	}
	_, after := getStats(t, srv, "cmp_summer_sale")
	if after.Counts != wantCounts || after.TotalEvents != wantTotals {
		t.Fatalf("seed retry changed stats: %+v", after)
	}

	// Keyset pagination: walk every event in cmp_summer_sale exactly once.
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		url := srv.URL + "/campaigns/cmp_summer_sale/events?limit=50"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		var page eventsResponse
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decode page: %v", err)
		}
		for _, e := range page.Events {
			if seen[e.EventID] {
				t.Fatalf("event %s appeared on two pages", e.EventID)
			}
			seen[e.EventID] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	total := wantTotals.Sent + wantTotals.Delivered + wantTotals.Opened + wantTotals.Clicked
	if len(seen) != total {
		t.Fatalf("paginated over %d events, want %d", len(seen), total)
	}
	if pages != 3 { // 50 + 50 + 14
		t.Fatalf("pages = %d, want 3", pages)
	}
}
