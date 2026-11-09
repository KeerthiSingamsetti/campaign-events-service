// Command campaign-events receives provider webhook events and serves
// per-campaign statistics for the marketing dashboard.
//
// Run:  go run .
// Env:  ADDR=:8080 DB=events.db (flags override)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "HTTP listen address")
	dbPath := flag.String("db", envOr("DB", "events.db"), "SQLite database file")
	flag.Parse()

	store, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("open store %q: %v", *dbPath, err)
	}
	defer store.Close()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           NewServer(store, time.Now, log.Default()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Shut down on Ctrl-C so the final batch is not cut off mid-transaction.
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	fmt.Printf("campaign-events listening on %s (db %s)\n", *addr, *dbPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
