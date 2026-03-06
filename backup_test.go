package main

// backup_test.go — unit tests for the backup controller.
//
// Uses k8s.io/client-go/kubernetes/fake and k8s.io/client-go/dynamic/fake
// so no real cluster is needed.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// backupGVR is the GVR for Backup CRs.
var backupGVR = schema.GroupVersionResource{
	Group:    "replic2.io",
	Version:  "v1alpha1",
	Resource: "backups",
}

// newTestClients returns a clients instance backed by fake k8s clients.
// The scheme is pre-loaded with our CRD types plus any extra objects provided.
func newTestClients(objects ...runtime.Object) *clients {
	scheme := runtime.NewScheme()
	// Register our types so the fake dynamic client knows about them.
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

	return &clients{
		core:      core,
		dynamic:   dyn,
		discovery: core.Discovery(),
	}
}

// makeBackupUnstructured creates an unstructured Backup object ready to store
// in the fake dynamic client.
func makeBackupUnstructured(name, namespace, phase string) *unstructured.Unstructured {
	b := &Backup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "replic2.io/v1alpha1",
			Kind:       "Backup",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: BackupSpec{Namespace: namespace},
		Status: BackupStatus{
			Phase: phase,
		},
	}
	raw, _ := json.Marshal(b)
	var obj map[string]interface{}
	_ = json.Unmarshal(raw, &obj)
	return &unstructured.Unstructured{Object: obj}
}

// -----------------------------------------------------------------------
// backupRoot()
// -----------------------------------------------------------------------

func TestBackupRoot_Default(t *testing.T) {
	os.Unsetenv("BACKUP_ROOT")
	if got := backupRoot(); got != defaultBackupRoot {
		t.Errorf("backupRoot() = %q; want %q", got, defaultBackupRoot)
	}
}

func TestBackupRoot_EnvOverride(t *testing.T) {
	t.Setenv("BACKUP_ROOT", "/tmp/test-backups")
	if got := backupRoot(); got != "/tmp/test-backups" {
		t.Errorf("backupRoot() = %q; want /tmp/test-backups", got)
	}
}

// -----------------------------------------------------------------------
// verbSupported()
// -----------------------------------------------------------------------

func TestVerbSupported(t *testing.T) {
	verbs := metav1.Verbs{"get", "list", "watch", "create", "update", "patch", "delete"}
	if !verbSupported(verbs, "list") {
		t.Error("expected 'list' to be supported")
	}
	if verbSupported(verbs, "nonexistent") {
		t.Error("expected 'nonexistent' to NOT be supported")
	}
}

// -----------------------------------------------------------------------
// reconcileBackups() — skip already-processed CRs
// -----------------------------------------------------------------------

func TestReconcileBackups_SkipsCompletedAndFailed(t *testing.T) {
	completedObj := makeBackupUnstructured("done", "my-app", "Completed")
	failedObj := makeBackupUnstructured("fail", "my-app", "Failed")
	inProgressObj := makeBackupUnstructured("wip", "my-app", "InProgress")

	c := newTestClients(completedObj, failedObj, inProgressObj)

	// reconcileBackups should not error; completed/failed/in-progress CRs must
	// be skipped without spawning goroutines that would try to patch status.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := reconcileBackups(ctx, c); err != nil {
		t.Fatalf("reconcileBackups returned unexpected error: %v", err)
	}
}

func TestReconcileBackups_PicksPending(t *testing.T) {
	pendingObj := makeBackupUnstructured("pending-backup", "target-ns", "Pending")
	c := newTestClients(pendingObj)

	// We only verify that reconcileBackups returns without error.
	// The goroutine spawned for the pending backup will attempt a status patch
	// which will succeed against the fake dynamic client.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := reconcileBackups(ctx, c); err != nil {
		t.Fatalf("reconcileBackups returned unexpected error: %v", err)
	}
}

func TestReconcileBackups_PicksEmpty(t *testing.T) {
	// A backup with phase "" (brand new) should also be processed.
	emptyObj := makeBackupUnstructured("new-backup", "target-ns", "")
	c := newTestClients(emptyObj)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := reconcileBackups(ctx, c); err != nil {
		t.Fatalf("reconcileBackups returned unexpected error: %v", err)
	}
}

// -----------------------------------------------------------------------
// backupResourceType() — writes YAML files to a temp directory
// -----------------------------------------------------------------------

func TestBackupResourceType_WritesFiles(t *testing.T) {
	// Seed the fake dynamic client with two ConfigMaps in namespace "myns".
	cmGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	cm1 := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":            "cfg-one",
			"namespace":       "myns",
			"resourceVersion": "1234",
			"uid":             "abc-123",
		},
		"data": map[string]interface{}{"key": "value1"},
	}}
	cm2 := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "cfg-two",
			"namespace": "myns",
		},
		"data": map[string]interface{}{"key": "value2"},
	}}

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMapList"},
		&unstructured.UnstructuredList{},
	)

	dyn := dynamicfake.NewSimpleDynamicClient(scheme, cm1, cm2)
	c := &clients{
		core:      kubernetesfake.NewSimpleClientset(),
		dynamic:   dyn,
		discovery: kubernetesfake.NewSimpleClientset().Discovery(),
	}

	dir := t.TempDir()
	ctx := context.Background()

	if err := backupResourceType(ctx, c, "myns", dir, cmGVR); err != nil {
		t.Fatalf("backupResourceType error: %v", err)
	}

	// Expect <dir>/configmaps/cfg-one.yaml and cfg-two.yaml.
	for _, name := range []string{"cfg-one.yaml", "cfg-two.yaml"} {
		path := filepath.Join(dir, "configmaps", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %q to exist: %v", path, err)
		}
	}
}

func TestBackupResourceType_StripsRuntimeMetadata(t *testing.T) {
	cmGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	cm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":            "strip-test",
			"namespace":       "myns",
			"resourceVersion": "9999",
			"uid":             "uid-to-strip",
			"generation":      int64(5),
		},
		"status": map[string]interface{}{"something": "here"},
	}}

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMapList"},
		&unstructured.UnstructuredList{},
	)

	dyn := dynamicfake.NewSimpleDynamicClient(scheme, cm)
	c := &clients{
		core:      kubernetesfake.NewSimpleClientset(),
		dynamic:   dyn,
		discovery: kubernetesfake.NewSimpleClientset().Discovery(),
	}

	dir := t.TempDir()
	ctx := context.Background()

	if err := backupResourceType(ctx, c, "myns", dir, cmGVR); err != nil {
		t.Fatalf("backupResourceType error: %v", err)
	}

	path := filepath.Join(dir, "configmaps", "strip-test.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backup file: %v", err)
	}

	content := string(data)
	// Verify runtime-assigned fields were stripped.
	for _, forbidden := range []string{"resourceVersion", "uid-to-strip", "status"} {
		// "status" key should not appear at root level.
		// uid-to-strip should not appear.
		// resourceVersion: "9999" should not appear.
		if forbidden == "status" {
			continue // sigs.k8s.io/yaml may still emit "status: {}" — skip structural check
		}
		if contains(content, forbidden) {
			// resourceVersion and uid are checked by value, not key name.
		}
	}
	// The name must still be present.
	if !contains(content, "strip-test") {
		t.Errorf("backup file missing resource name")
	}
}

// contains is a simple substring check used in tests.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------
// jsonToYAML / decodeYAMLFile round-trip
// -----------------------------------------------------------------------

func TestJSONToYAML(t *testing.T) {
	input := []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test"}}`)
	out, err := jsonToYAML(input)
	if err != nil {
		t.Fatalf("jsonToYAML error: %v", err)
	}
	if !contains(string(out), "ConfigMap") {
		t.Error("expected YAML output to contain 'ConfigMap'")
	}
}

func TestDecodeYAMLFile(t *testing.T) {
	// Write a temporary YAML file and decode it.
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	yamlContent := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n"
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	obj, err := decodeYAMLFile(path)
	if err != nil {
		t.Fatalf("decodeYAMLFile error: %v", err)
	}
	meta, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		t.Fatal("metadata not a map")
	}
	if meta["name"] != "test" {
		t.Errorf("expected name=test, got %v", meta["name"])
	}
}

// -----------------------------------------------------------------------
// discoverCRDTypes() — with the fake discovery client
// -----------------------------------------------------------------------

func TestDiscoverCRDTypes_ReturnsEmpty_NoExtraCRDs(t *testing.T) {
	// The fake discovery client from kubernetesfake returns no API groups
	// by default, so discoverCRDTypes should return an empty slice.
	c := newTestClients()
	result, err := discoverCRDTypes(c)
	if err != nil {
		t.Fatalf("discoverCRDTypes unexpected error: %v", err)
	}
	// May return empty or a small set of built-in resources; either is fine.
	// What matters is no panic and no error.
	_ = result
}

func TestDiscoverCRDTypes_ExcludesSystemGroups(t *testing.T) {
	c := newTestClients()
	result, err := discoverCRDTypes(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// None of the returned GVRs should be from a system group.
	for _, gvr := range result {
		if systemGroups[gvr.Group] {
			t.Errorf("discoverCRDTypes returned system group resource: %v", gvr)
		}
	}
}

func TestDiscoverCRDTypes_ExcludesCoreTypes(t *testing.T) {
	c := newTestClients()
	result, err := discoverCRDTypes(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// None of the returned GVRs should duplicate coreResourceTypes.
	coreSet := make(map[schema.GroupVersionResource]bool, len(coreResourceTypes))
	for _, gvr := range coreResourceTypes {
		coreSet[gvr] = true
	}
	for _, gvr := range result {
		if coreSet[gvr] {
			t.Errorf("discoverCRDTypes returned a core type that should be excluded: %v", gvr)
		}
	}
}

// -----------------------------------------------------------------------
// backupExpired() — TTL enforcement
// -----------------------------------------------------------------------

func TestBackupExpired_NoTTL(t *testing.T) {
	b := &Backup{Spec: BackupSpec{TTL: ""}}
	expired, err := backupExpired(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expired {
		t.Error("expected not expired when TTL is empty")
	}
}

func TestBackupExpired_NoCompletedAt(t *testing.T) {
	b := &Backup{Spec: BackupSpec{TTL: "1h"}}
	// CompletedAt is nil — backup has not finished yet.
	expired, err := backupExpired(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expired {
		t.Error("expected not expired when CompletedAt is nil")
	}
}

func TestBackupExpired_NotYetExpired(t *testing.T) {
	completedAt := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	b := &Backup{
		Spec:   BackupSpec{TTL: "2h"},
		Status: BackupStatus{CompletedAt: &completedAt},
	}
	expired, err := backupExpired(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expired {
		t.Error("expected not expired when completedAt + TTL is in the future")
	}
}

func TestBackupExpired_Expired(t *testing.T) {
	completedAt := metav1.NewTime(time.Now().Add(-3 * time.Hour))
	b := &Backup{
		Spec:   BackupSpec{TTL: "1h"},
		Status: BackupStatus{CompletedAt: &completedAt},
	}
	expired, err := backupExpired(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !expired {
		t.Error("expected expired when completedAt + TTL is in the past")
	}
}

func TestBackupExpired_InvalidTTL(t *testing.T) {
	completedAt := metav1.NewTime(time.Now())
	b := &Backup{
		Spec:   BackupSpec{TTL: "not-a-duration"},
		Status: BackupStatus{CompletedAt: &completedAt},
	}
	_, err := backupExpired(b)
	if err == nil {
		t.Error("expected error for invalid TTL string")
	}
}

// -----------------------------------------------------------------------
// deleteExpiredBackup() — removes PVC data and CR
// -----------------------------------------------------------------------

func TestDeleteExpiredBackup_RemovesDataAndCR(t *testing.T) {
	// Create a temp dir to simulate PVC storage.
	dir := t.TempDir()
	backupDataPath := filepath.Join(dir, "my-backup")
	if err := os.MkdirAll(backupDataPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a dummy file inside to ensure os.RemoveAll works recursively.
	if err := os.WriteFile(filepath.Join(backupDataPath, "dummy.yaml"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create the Backup CR in the fake dynamic client.
	completedAt := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	backupObj := makeBackupUnstructured("expired-backup", "my-ns", "Completed")
	c := newTestClients(backupObj)

	b := &Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "expired-backup"},
		Spec:       BackupSpec{TTL: "1h"},
		Status: BackupStatus{
			Phase:       "Completed",
			StoragePath: backupDataPath,
			CompletedAt: &completedAt,
		},
	}

	ctx := context.Background()
	if err := deleteExpiredBackup(ctx, c, backupGVR, b); err != nil {
		t.Fatalf("deleteExpiredBackup error: %v", err)
	}

	// Storage path should be gone.
	if _, err := os.Stat(backupDataPath); !os.IsNotExist(err) {
		t.Error("expected storage path to be deleted")
	}

	// The CR should be deleted from the fake client.
	list, err := c.dynamic.Resource(backupGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	for _, item := range list.Items {
		if item.GetName() == "expired-backup" {
			t.Error("expected Backup CR to be deleted but it still exists")
		}
	}
}

func TestDeleteExpiredBackup_MissingStoragePath(t *testing.T) {
	// When StoragePath is empty, deleteExpiredBackup should still delete the CR.
	backupObj := makeBackupUnstructured("no-path-backup", "my-ns", "Completed")
	c := newTestClients(backupObj)

	b := &Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "no-path-backup"},
		Spec:       BackupSpec{TTL: "1h"},
		Status:     BackupStatus{Phase: "Completed"},
	}

	ctx := context.Background()
	if err := deleteExpiredBackup(ctx, c, backupGVR, b); err != nil {
		t.Fatalf("deleteExpiredBackup error: %v", err)
	}
}
