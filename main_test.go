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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"

	"replic2/internal/k8s"
	"replic2/internal/server"
	"replic2/internal/server/handler"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newFakeClients(objects ...runtime.Object) *k8s.Clients {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "replic2.io", Version: "v1alpha1", Kind: "Backup"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "replic2.io", Version: "v1alpha1", Kind: "BackupList"},
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "replic2.io", Version: "v1alpha1", Kind: "Restore"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "replic2.io", Version: "v1alpha1", Kind: "RestoreList"},
		&unstructured.UnstructuredList{},
	)

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
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want %d", rr.Code, http.StatusOK)
	}
}

func TestHelloHandler_ContentTypeJSON(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}

func TestHelloHandler_BodyFields(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp handler.HelloResponse
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
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp handler.HelloResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Version != "9.9.9" {
		t.Errorf("Version = %q; want 9.9.9", resp.Version)
	}
}

func TestHelloHandler_NamespaceFromEnv(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "production")
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp handler.HelloResponse
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
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want %d", rr.Code, http.StatusOK)
	}
}

func TestHealthzHandler_ContentTypeJSON(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}

func TestHealthzHandler_BodyFields(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp handler.HealthResponse
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
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want %d", rr.Code, http.StatusOK)
	}
}

func TestReadyzHandler_ContentTypeJSON(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}

func TestReadyzHandler_BodyReady(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
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

// -----------------------------------------------------------------------
// GET /backup
// -----------------------------------------------------------------------

func makeBackup(name, phase, completedAt string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "replic2.io",
		Version: "v1alpha1",
		Kind:    "Backup",
	})
	obj.SetName(name)
	obj.SetNamespace("default")
	if phase != "" || completedAt != "" {
		status := map[string]interface{}{}
		if phase != "" {
			status["phase"] = phase
		}
		if completedAt != "" {
			status["completedAt"] = completedAt
		}
		obj.Object["status"] = status
	}
	return obj
}

func makeRestore(name, phase, completedAt string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "replic2.io",
		Version: "v1alpha1",
		Kind:    "Restore",
	})
	obj.SetName(name)
	obj.SetNamespace("default")
	if phase != "" || completedAt != "" {
		status := map[string]interface{}{}
		if phase != "" {
			status["phase"] = phase
		}
		if completedAt != "" {
			status["completedAt"] = completedAt
		}
		obj.Object["status"] = status
	}
	return obj
}

func TestBackupHandler_StatusOK(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/backup", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want %d", rr.Code, http.StatusOK)
	}
}

func TestBackupHandler_ContentTypeJSON(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/backup", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}

func TestBackupHandler_EmptyList(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/backup", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp []handler.Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("len = %d; want 0", len(resp))
	}
}

func TestBackupHandler_ListsBackups(t *testing.T) {
	b1 := makeBackup("backup-01", "Completed", "2024-01-01T00:00:00Z")
	b2 := makeBackup("backup-02", "InProgress", "")
	r := server.NewRouter(newFakeClients(b1, b2), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/backup", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rr.Code, http.StatusOK)
	}
	var resp []handler.Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("len = %d; want 2", len(resp))
	}
}

func TestBackupHandler_PendingPhaseDefault(t *testing.T) {
	b := makeBackup("backup-pending", "", "")
	r := server.NewRouter(newFakeClients(b), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/backup", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp []handler.Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("len = %d; want 1", len(resp))
	}
	if resp[0].Phase != "Pending" {
		t.Errorf("Phase = %q; want Pending", resp[0].Phase)
	}
}

// -----------------------------------------------------------------------
// GET /restore
// -----------------------------------------------------------------------

func TestRestoreHandler_StatusOK(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/restore", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want %d", rr.Code, http.StatusOK)
	}
}

func TestRestoreHandler_ContentTypeJSON(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/restore", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}

func TestRestoreHandler_EmptyList(t *testing.T) {
	r := server.NewRouter(newFakeClients(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/restore", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp []handler.Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("len = %d; want 0", len(resp))
	}
}

func TestRestoreHandler_ListsRestores(t *testing.T) {
	rs1 := makeRestore("restore-01", "Completed", "2024-01-01T00:00:00Z")
	rs2 := makeRestore("restore-02", "InProgress", "")
	r := server.NewRouter(newFakeClients(rs1, rs2), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/restore", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rr.Code, http.StatusOK)
	}
	var resp []handler.Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("len = %d; want 2", len(resp))
	}
}

func TestRestoreHandler_PendingPhaseDefault(t *testing.T) {
	rs := makeRestore("restore-pending", "", "")
	r := server.NewRouter(newFakeClients(rs), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/restore", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp []handler.Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("len = %d; want 1", len(resp))
	}
	if resp[0].Phase != "Pending" {
		t.Errorf("Phase = %q; want Pending", resp[0].Phase)
	}
}
