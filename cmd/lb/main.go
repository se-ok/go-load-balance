package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go-load-balance/lib"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// CLI flags
	var backends []string
	port := flag.Int("port", 8080, "Port to listen on")
	timeout := flag.Duration("timeout", 4*time.Hour, "Request timeout duration (e.g. 500ms, 30s, 5m, 2h, 1h30m)")
	healthCheckInterval := flag.Duration("health-check-interval", 30*time.Second, "Health check interval (e.g. 500ms, 30s, 5m, 2h, 1h30m)")
	verbose := flag.Bool("verbose", false, "Enable verbose logging")

	// Custom flag parsing for space-separated backends
	flag.Func("backends", "Space-separated backend URLs (required)", func(s string) error {
		backends = append(backends, s)
		return nil
	})

	flag.Parse()

	// Validate configuration
	if len(backends) == 0 {
		fmt.Fprintln(os.Stderr, "Error: at least one backend is required")
		fmt.Fprintln(os.Stderr, "Usage: go-load-balance --backends <url1> <url2> ...")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if *port < 1 || *port > 65535 {
		fmt.Fprintf(os.Stderr, "Error: invalid port %d (must be 1-65535)\n", *port)
		os.Exit(1)
	}

	if *timeout < 0 {
		fmt.Fprintf(os.Stderr, "Error: timeout cannot be negative\n")
		os.Exit(1)
	}

	// Print startup configuration
	log.Printf("Starting go-load-balance")
	log.Printf("Port: %d", *port)
	log.Printf("Timeout: %v", *timeout)
	log.Printf("Health check interval: %v", *healthCheckInterval)
	log.Printf("Verbose: %v", *verbose)
	log.Printf("Backends:")
	for _, backend := range backends {
		log.Printf("  - %s", backend)
	}

	// Create backend pool
	pool, err := lib.NewPool(backends)
	if err != nil {
		log.Fatalf("Failed to create backend pool: %v", err)
	}

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start health checker
	healthChecker := lib.NewHealthChecker(pool, *healthCheckInterval, *timeout)
	go healthChecker.Start(ctx)

	// Start status logger
	statusLogger := lib.NewStatusLogger(pool, *verbose)
	go statusLogger.Start(ctx)

	// Create mux with health endpoint
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		totalActive, healthyCount, totalCount := pool.GetStatus()
		status := map[string]interface{}{
			"status":          "ok",
			"healthy_backends": healthyCount,
			"total_backends":   totalCount,
			"active_conns":     totalActive,
		}
		if healthyCount == 0 {
			status["status"] = "degraded"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})
	mux.Handle("/", pool)

	// Create HTTP server
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", *port),
		Handler:      mux,
		ReadTimeout:  *timeout,
		WriteTimeout: *timeout,
	}

	// Handle graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan

		log.Println("Shutting down...")
		cancel()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}()

	// Start HTTP server
	log.Printf("Load balancer listening on :%d", *port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server failed: %v", err)
	}

	log.Println("Server stopped")
}
