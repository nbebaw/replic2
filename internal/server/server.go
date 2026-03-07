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
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var startTime = time.Now()

// HealthResponse is the JSON body returned by GET /healthz.
type HealthResponse struct {
	Status   string `json:"status"`
	Uptime   string `json:"uptime"`
	Hostname string `json:"hostname"`
}

// HelloResponse is the JSON body returned by GET /.
type HelloResponse struct {
	Message   string `json:"message"`
	App       string `json:"app"`
	Version   string `json:"version"`
	Hostname  string `json:"hostname"`
	Namespace string `json:"namespace"`
	Timestamp string `json:"timestamp"`
}

type backupResponse struct {
	Name        string `json:"name"`
	Phase       string `json:"phase"`
	CompletedAt string `json:"completedAt,omitempty"`
}

// GVR is the GroupVersionResource for the Backup CRD.
var BackupGVR = schema.GroupVersionResource{
	Group:    "replic2.io",
	Version:  "v1alpha1",
	Resource: "backups",
}

// NewRouter builds and returns a configured gin.Engine.
// Extracted so tests can create the router without starting a real server.
func NewRouter(clients *k8s.Clients) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/", helloHandler)
	r.GET("/healthz", healthzHandler)
	r.GET("/readyz", readyzHandler)
	r.GET("/backup", func(c *gin.Context) {
		backupHandler(c, clients)
	})
	return r
}

func backupHandler(c *gin.Context, clients *k8s.Clients) {
	list, err := clients.Dynamic.Resource(BackupGVR).List(c, metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for _, item := range list.Items {
		// Strip fields that must not be re-applied verbatim.
		c.JSON(http.StatusOK, backupResponse{
			Name:        item.Object["metadata"].(map[string]interface{})["name"].(string),
			Phase:       item.Object["status"].(map[string]interface{})["phase"].(string),
			CompletedAt: item.Object["status"].(map[string]interface{})["completedAt"].(string),
		})

	}
	c.JSON(http.StatusOK, list)
}

// helloHandler handles GET / — returns app metadata.
func helloHandler(c *gin.Context) {
	c.JSON(http.StatusOK, HelloResponse{
		Message:   "Hello from replic2!",
		App:       "replic2",
		Version:   appVersion(),
		Hostname:  hostname(),
		Namespace: podNamespace(),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// healthzHandler handles GET /healthz — liveness probe.
func healthzHandler(c *gin.Context) {
	c.JSON(http.StatusOK, HealthResponse{
		Status:   "ok",
		Uptime:   time.Since(startTime).Round(time.Second).String(),
		Hostname: hostname(),
	})
}

// readyzHandler handles GET /readyz — readiness probe.
func readyzHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

// ---------------------------------------------------------------------------
// Helpers — read from environment with sensible defaults.
// ---------------------------------------------------------------------------

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

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
