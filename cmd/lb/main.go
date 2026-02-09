package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-load-balance/lib"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
)

func main() {
	app := &cli.Command{
		Name:      "lb",
		Usage:     "A simple load balancer",
		UsageText: "lb --backends <url1> [--backends <url2> ...] [--port <port>] [--timeout <duration>] [--health-check-interval <duration>] [--verbose]",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:     "backends",
				Usage:    "Backend URLs (required)",
				Required: true,
			},
			&cli.IntFlag{
				Name:  "port",
				Usage: "Port to listen on",
				Value: 8080,
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Request timeout duration (e.g. 500ms, 30s, 5m, 2h, 1h30m)",
				Value: 4 * time.Hour,
			},
			&cli.DurationFlag{
				Name:  "health-check-interval",
				Usage: "Health check interval (e.g. 500ms, 30s, 5m, 2h, 1h30m)",
				Value: 30 * time.Second,
			},
			&cli.BoolFlag{
				Name:  "verbose",
				Usage: "Enable verbose logging",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			backends := cmd.StringSlice("backends")
			// Remaining positional args are also backends (supports bash expansion:
			// lb --backends http://localhost:800{0..2})
			backends = append(backends, cmd.Args().Slice()...)

			port := cmd.Int("port")
			timeout := cmd.Duration("timeout")
			healthCheckInterval := cmd.Duration("health-check-interval")
			verbose := cmd.Bool("verbose")

			// Add http:// to backends without a scheme
			for i, b := range backends {
				if !strings.Contains(b, "://") {
					backends[i] = "http://" + b
				}
			}

			if port < 1 || port > 65535 {
				return fmt.Errorf("invalid port %d (must be 1-65535)", port)
			}

			if timeout < 0 {
				return fmt.Errorf("timeout cannot be negative")
			}

			// Print startup configuration
			log.Printf("Starting go-load-balance")
			log.Printf("Port: %d", port)
			log.Printf("Timeout: %v", timeout)
			log.Printf("Health check interval: %v", healthCheckInterval)
			log.Printf("Verbose: %v", verbose)
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
			healthChecker := lib.NewHealthChecker(pool, healthCheckInterval, timeout)
			go healthChecker.Start(ctx)

			// Start status logger
			statusLogger := lib.NewStatusLogger(pool, verbose)
			go statusLogger.Start(ctx)

			// Create mux with health endpoint
			mux := http.NewServeMux()
			mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
				totalActive, healthyCount, totalCount := pool.GetStatus()
				status := map[string]interface{}{
					"status":           "ok",
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
				Addr:         fmt.Sprintf(":%d", port),
				Handler:      mux,
				ReadTimeout:  timeout,
				WriteTimeout: timeout,
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
			log.Printf("Load balancer listening on :%d", port)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Server failed: %v", err)
			}

			log.Println("Server stopped")
			return nil
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
