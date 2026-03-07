// Package server provides the HTTP router and handlers for replic2.
//
// Endpoints:
//
//	GET /        — application metadata (version, hostname, namespace, timestamp)
//	GET /healthz — liveness probe
//	GET /readyz  — readiness probe
//
// NewRouter returns a configured *gin.Engine so tests can exercise the
// handlers without starting a real TCP listener.
package server

import (
	"net/http"
	"os"
	"replic2/internal/k8s"
	"replic2/internal/server/handler"
	"time"

	"github.com/gin-gonic/gin"
)

// NewRouter builds and returns a configured gin.Engine.
// Extracted so tests can create the router without starting a real server.
func NewRouter(clients *k8s.Clients, startTime time.Time) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/", helloHandler)
	r.GET("/healthz", func(c *gin.Context) {
		handler.HealthzHandler(c, startTime)
	})
	r.GET("/readyz", handler.ReadyzHandler)
	r.GET("/backup", func(c *gin.Context) {
		handler.BackupHandler(c, clients)
	})
	r.GET("/restore", func(c *gin.Context) {
		handler.RestoreHandler(c, clients)
	})
	return r
}

// helloHandler handles GET / — returns app metadata.
func helloHandler(c *gin.Context) {
	c.JSON(http.StatusOK, handler.HelloResponse{
		Message:   "Hello from replic2!",
		App:       "replic2",
		Version:   appVersion(),
		Hostname:  handler.Hostname(),
		Namespace: podNamespace(),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------------------
// Helpers — read from environment with sensible defaults.
// ---------------------------------------------------------------------------

func podNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

func appVersion() string {
	if v := os.Getenv("APP_VERSION"); v != "" {
		return v
	}
	return "0.1.0"
}
