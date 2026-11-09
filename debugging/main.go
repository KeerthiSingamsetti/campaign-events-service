// Command eventstats reads a JSONL file of campaign events and prints
// per-campaign statistics.
//
// Requirements (see README.md):
//  1. Each event_id must be counted at most once across the whole file.
//  2. unique_opens = number of distinct contacts that opened, per campaign.
//  3. Daily delivered buckets use the event's UTC date.
//  4. Output must be identical across runs on the same input.
//
// Usage: go run . events.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

type Event struct {
	EventID    string    `json:"event_id"`
	CampaignID string    `json:"campaign_id"`
	ContactID  string    `json:"contact_id"`
	Type       string    `json:"type"`
	Timestamp  time.Time `json:"timestamp"`
}

type CampaignStats struct {
	Sent           int
	Delivered      int
	Opened         int
	Clicked        int
	UniqueOpens    int
	DailyDelivered map[string]int
}

// stats records are created before the workers run, so the map itself is
// never written concurrently.
var stats = map[string]*CampaignStats{}

// openedBy remembers which contacts have already opened, so repeat opens by
// the same contact do not inflate unique_opens. It is only touched from
// track(), which runs on the main goroutine.
var openedBy = map[string]bool{}

// seen remembers every event_id already processed. It must outlive a single
// batch: batches are an implementation detail, while de-duplication is defined
// across the whole file. processBatch runs on the main goroutine, so a plain
// map is enough.
var seen = map[string]bool{}

// mu protects the counters updated by the worker goroutines in apply().
var mu sync.Mutex

const (
	batchSize  = 200
	numWorkers = 8
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run . <events.jsonl>")
		os.Exit(1)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	batch := make([]Event, 0, batchSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			fmt.Fprintf(os.Stderr, "skipping bad line: %v\n", err)
			continue
		}
		batch = append(batch, ev)
		if len(batch) == batchSize {
			processBatch(batch)
			batch = make([]Event, 0, batchSize)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(batch) > 0 {
		processBatch(batch)
	}

	printReport()
}

// processBatch de-duplicates a batch, runs the map-based tracking that needs
// ordered access, then fans the counter updates out to a worker pool. The
// counters are plain ints, so the increments themselves are cheap.
func processBatch(events []Event) {
	unique := make([]Event, 0, len(events))
	for _, ev := range events {
		if seen[ev.EventID] {
			continue
		}
		seen[ev.EventID] = true
		if _, ok := stats[ev.CampaignID]; !ok {
			stats[ev.CampaignID] = &CampaignStats{DailyDelivered: map[string]int{}}
		}
		track(ev)
		unique = append(unique, ev)
	}

	jobs := make(chan Event)
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ev := range jobs {
				apply(ev)
			}
		}()
	}
	for _, ev := range unique {
		jobs <- ev
	}
	close(jobs)
	wg.Wait()
}

// track owns unique-open tracking and daily delivery buckets. Maps are not
// safe for concurrent use, so this runs before the workers are started.
func track(ev Event) {
	cs := stats[ev.CampaignID]
	switch ev.Type {
	case "opened":
		// Key by campaign AND contact: the same person opening two campaigns
		// counts once in each campaign, not once overall.
		key := ev.CampaignID + "\x00" + ev.ContactID
		if !openedBy[key] {
			openedBy[key] = true
			cs.UniqueOpens++
		}
	case "delivered":
		// The timestamp field is UTC, so bucket by UTC date. Local time would
		// move late-evening events into the next day on non-UTC machines.
		day := ev.Timestamp.UTC().Format("2006-01-02")
		cs.DailyDelivered[day]++
	}
}

// apply updates the counters for a single event. The stats record always
// exists at this point.
func apply(ev Event) {
	cs := stats[ev.CampaignID]
	mu.Lock()
	defer mu.Unlock()
	switch ev.Type {
	case "sent":
		cs.Sent++
	case "delivered":
		cs.Delivered++
	case "opened":
		cs.Opened++
	case "clicked":
		cs.Clicked++
	}
}

func printReport() {
	names := make([]string, 0, len(stats))
	for name := range stats {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cs := stats[name]
		fmt.Printf("campaign=%s sent=%d delivered=%d opened=%d clicked=%d unique_opens=%d\n",
			name, cs.Sent, cs.Delivered, cs.Opened, cs.Clicked, cs.UniqueOpens)
		days := make([]string, 0, len(cs.DailyDelivered))
		for day := range cs.DailyDelivered {
			days = append(days, day)
		}
		sort.Strings(days)
		for _, day := range days {
			fmt.Printf("  %s delivered=%d\n", day, cs.DailyDelivered[day])
		}
	}
}
