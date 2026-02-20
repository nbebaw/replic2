package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

var startTime = time.Now()

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

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", helloHandler)
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/readyz", readyzHandler)

	addr := fmt.Sprintf(":%s", port)
	log.Printf("replic2 starting on %s (pod: %s, namespace: %s)", addr, hostname(), namespace())

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
