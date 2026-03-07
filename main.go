package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var startTime = time.Now()

// -----------------------------------------------------------------------
// Existing HTTP handler types (unchanged)
// -----------------------------------------------------------------------

type HealthResponse struct {
	Status   string `json:"status"`
	Uptime   string `json:"uptime"`
	Hostname string `json:"hostname"`
}

type HelloResponse struct {
	Message   string `json:"message"`
	App       string `json:"app"`
	Version   string `json:"version"`
	Hostname  string `json:"hostname"`
	Namespace string `json:"namespace"`
	Timestamp string `json:"timestamp"`
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func namespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

func version() string {
	if v := os.Getenv("APP_VERSION"); v != "" {
		return v
	}
	return "0.1.0"
}

// -----------------------------------------------------------------------
// HTTP handlers
// -----------------------------------------------------------------------

func helloHandler(w http.ResponseWriter, r *http.Request) {
	resp := HelloResponse{
		Message:   "Hello from replic2!",
		App:       "replic2",
		Version:   version(),
		Hostname:  hostname(),
		Namespace: namespace(),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{
		Status:   "ok",
		Uptime:   time.Since(startTime).Round(time.Second).String(),
		Hostname: hostname(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func readyzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

// -----------------------------------------------------------------------
// main
// -----------------------------------------------------------------------

func main() {
	// Root context — cancelled on SIGTERM / SIGINT for graceful shutdown.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// ---- Kubernetes clients ----
	c, err := newClients()
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}

	// ---- HTTP server ----
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", helloHandler)
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/readyz", readyzHandler)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", port),
		Handler: mux,
	}

	go func() {
		log.Printf("replic2 starting on :%s (pod: %s, namespace: %s)", port, hostname(), namespace())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	// ---- Controllers (leader-elected) ----
	// runWithLeaderElection blocks until ctx is cancelled.
	// Only the elected leader actually runs the backup/restore controllers;
	// standby pods keep the HTTP server alive.
	go runWithLeaderElection(ctx, c, func(leaderCtx context.Context) {
		go runBackupController(leaderCtx, c)
		go runRestoreController(leaderCtx, c)
		go runScheduledBackupController(leaderCtx, c)
		<-leaderCtx.Done()
	})

	// ---- Graceful shutdown ----
	<-ctx.Done()
	log.Println("replic2: shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
}
