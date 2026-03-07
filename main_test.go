package main

// main_test.go — unit tests for the HTTP handlers.
//
// Uses net/http/httptest so no real server is started.
// Routes are exercised through server.NewRouter().

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"

	"replic2/internal/k8s"
	"replic2/internal/server"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newFakeClients(objects ...runtime.Object) *k8s.Clients {
	scheme := runtime.NewScheme()

	dyn := dynamicfake.NewSimpleDynamicClient(scheme, objects...)
	core := kubernetesfake.NewSimpleClientset()

	return &k8s.Clients{
		Core:      core,
		Dynamic:   dyn,
		Discovery: core.Discovery(),
	}
}

// -----------------------------------------------------------------------
// GET /
// -----------------------------------------------------------------------

func TestHelloHandler_StatusOK(t *testing.T) {
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want %d", rr.Code, http.StatusOK)
	}
}

func TestHelloHandler_ContentTypeJSON(t *testing.T) {
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}

func TestHelloHandler_BodyFields(t *testing.T) {
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp server.HelloResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.App != "replic2" {
		t.Errorf("App = %q; want replic2", resp.App)
	}
	if resp.Message == "" {
		t.Error("Message should not be empty")
	}
	if resp.Hostname == "" {
		t.Error("Hostname should not be empty")
	}
	if resp.Version == "" {
		t.Error("Version should not be empty")
	}
	if _, err := time.Parse(time.RFC3339, resp.Timestamp); err != nil {
		t.Errorf("Timestamp %q is not valid RFC3339: %v", resp.Timestamp, err)
	}
}

func TestHelloHandler_VersionFromEnv(t *testing.T) {
	t.Setenv("APP_VERSION", "9.9.9")
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp server.HelloResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Version != "9.9.9" {
		t.Errorf("Version = %q; want 9.9.9", resp.Version)
	}
}

func TestHelloHandler_NamespaceFromEnv(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "production")
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp server.HelloResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Namespace != "production" {
		t.Errorf("Namespace = %q; want production", resp.Namespace)
	}
}

// -----------------------------------------------------------------------
// GET /healthz
// -----------------------------------------------------------------------

func TestHealthzHandler_StatusOK(t *testing.T) {
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want %d", rr.Code, http.StatusOK)
	}
}

func TestHealthzHandler_ContentTypeJSON(t *testing.T) {
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}

func TestHealthzHandler_BodyFields(t *testing.T) {
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp server.HealthResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("Status = %q; want ok", resp.Status)
	}
	if resp.Hostname == "" {
		t.Error("Hostname should not be empty")
	}
	if resp.Uptime == "" {
		t.Error("Uptime should not be empty")
	}
}

// -----------------------------------------------------------------------
// GET /readyz
// -----------------------------------------------------------------------

func TestReadyzHandler_StatusOK(t *testing.T) {
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want %d", rr.Code, http.StatusOK)
	}
}

func TestReadyzHandler_ContentTypeJSON(t *testing.T) {
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}

func TestReadyzHandler_BodyReady(t *testing.T) {
	r := server.NewRouter(newFakeClients())
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "ready" {
		t.Errorf("status = %q; want ready", resp["status"])
	}
}
