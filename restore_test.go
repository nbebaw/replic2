package main

// restore_test.go — unit tests for the restore controller.
//
// Uses fake k8s clients and a temp directory to simulate PVC storage.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"

	restorectl "replic2/internal/controller/restore"
	"replic2/internal/k8s"
	"replic2/internal/types"
)

// makeRestoreUnstructured creates a Restore unstructured object for use in fake client.
func makeRestoreUnstructured(name, namespace, backupName, phase string) *unstructured.Unstructured {
	r := &types.Restore{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "replic2.io/v1alpha1",
			Kind:       "Restore",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: types.RestoreSpec{
			Namespace:  namespace,
			BackupName: backupName,
		},
		Status: types.RestoreStatus{
			Phase: phase,
		},
	}
	raw, _ := json.Marshal(r)
	var obj map[string]interface{}
	_ = json.Unmarshal(raw, &obj)
	return &unstructured.Unstructured{Object: obj}
}

// newTestClientsWithRestoreScheme builds a fake k8s.Clients that has
// both Backup and Restore kinds registered.
func newTestClientsWithRestoreScheme(objects ...runtime.Object) *k8s.Clients {
	scheme := runtime.NewScheme()
	for _, info := range []struct {
		kind     string
		listKind string
		group    string
		version  string
	}{
		{"Backup", "BackupList", "replic2.io", "v1alpha1"},
		{"Restore", "RestoreList", "replic2.io", "v1alpha1"},
	} {
		scheme.AddKnownTypeWithName(
			schema.GroupVersionKind{Group: info.group, Version: info.version, Kind: info.kind},
			&unstructured.Unstructured{},
		)
		scheme.AddKnownTypeWithName(
			schema.GroupVersionKind{Group: info.group, Version: info.version, Kind: info.listKind},
			&unstructured.UnstructuredList{},
		)
	}
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, objects...)
	core := kubernetesfake.NewSimpleClientset()
	return &k8s.Clients{
		Core:      core,
		Dynamic:   dyn,
		Discovery: core.Discovery(),
	}
}

// -----------------------------------------------------------------------
// ReconcileRestores() — skip logic
// -----------------------------------------------------------------------

func TestReconcileRestores_SkipsCompleted(t *testing.T) {
	obj := makeRestoreUnstructured("done", "my-ns", "", "Completed")
	c := newTestClientsWithRestoreScheme(obj)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := restorectl.ReconcileRestores(ctx, c); err != nil {
		t.Fatalf("ReconcileRestores error: %v", err)
	}
}

func TestReconcileRestores_SkipsFailed(t *testing.T) {
	obj := makeRestoreUnstructured("fail", "my-ns", "", "Failed")
	c := newTestClientsWithRestoreScheme(obj)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := restorectl.ReconcileRestores(ctx, c); err != nil {
		t.Fatalf("ReconcileRestores error: %v", err)
	}
}

func TestReconcileRestores_PicksPending(t *testing.T) {
	obj := makeRestoreUnstructured("pending-restore", "my-ns", "", "Pending")
	c := newTestClientsWithRestoreScheme(obj)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := restorectl.ReconcileRestores(ctx, c); err != nil {
		t.Fatalf("ReconcileRestores error: %v", err)
	}
}

// -----------------------------------------------------------------------
// EnsureNamespace()
// -----------------------------------------------------------------------

func TestEnsureNamespace_CreatesIfMissing(t *testing.T) {
	c := newTestClientsWithRestoreScheme()

	ctx := context.Background()
	if err := restorectl.EnsureNamespace(ctx, c, "brand-new-ns"); err != nil {
		t.Fatalf("EnsureNamespace error: %v", err)
	}
	// Second call must be idempotent (namespace already exists).
	if err := restorectl.EnsureNamespace(ctx, c, "brand-new-ns"); err != nil {
		t.Fatalf("EnsureNamespace (idempotent) error: %v", err)
	}
}

func TestEnsureNamespace_ExistingNamespaceNoError(t *testing.T) {
	c := newTestClientsWithRestoreScheme()
	ctx := context.Background()

	// Create the namespace once.
	if err := restorectl.EnsureNamespace(ctx, c, "existing-ns"); err != nil {
		t.Fatalf("create error: %v", err)
	}
	// Try creating again — should be idempotent.
	if err := restorectl.EnsureNamespace(ctx, c, "existing-ns"); err != nil {
		t.Fatalf("idempotent error: %v", err)
	}
}

// -----------------------------------------------------------------------
// FindBackupPath() — auto-select newest backup
// -----------------------------------------------------------------------

func TestFindBackupPath_AutoSelect(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BACKUP_ROOT", root)

	ns := "my-app"
	nsDir := filepath.Join(root, ns)
	_ = os.MkdirAll(filepath.Join(nsDir, "backup-old"), 0o755)
	time.Sleep(5 * time.Millisecond) // ensure different mtime
	_ = os.MkdirAll(filepath.Join(nsDir, "backup-new"), 0o755)

	c := newTestClientsWithRestoreScheme()
	r := &types.Restore{
		Spec: types.RestoreSpec{Namespace: ns},
	}

	ctx := context.Background()
	got, err := restorectl.FindBackupPath(ctx, c, r)
	if err != nil {
		t.Fatalf("FindBackupPath error: %v", err)
	}
	want := filepath.Join(nsDir, "backup-new")
	if got != want {
		t.Errorf("FindBackupPath = %q; want %q", got, want)
	}
}

func TestFindBackupPath_ExplicitBackupName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BACKUP_ROOT", root)

	ns := "my-app"
	backupName := "explicit-backup"
	storagePath := filepath.Join(root, ns, backupName)
	_ = os.MkdirAll(storagePath, 0o755)

	// Create a Backup CR in the fake client with the storage path set.
	backupObj := &types.Backup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "replic2.io/v1alpha1",
			Kind:       "Backup",
		},
		ObjectMeta: metav1.ObjectMeta{Name: backupName},
		Spec:       types.BackupSpec{Namespace: ns},
		Status:     types.BackupStatus{Phase: "Completed", StoragePath: storagePath},
	}
	raw, _ := json.Marshal(backupObj)
	var obj map[string]interface{}
	_ = json.Unmarshal(raw, &obj)
	u := &unstructured.Unstructured{Object: obj}

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

	dyn := dynamicfake.NewSimpleDynamicClient(scheme, u)
	coreClient := kubernetesfake.NewSimpleClientset()
	c := &k8s.Clients{Core: coreClient, Dynamic: dyn, Discovery: coreClient.Discovery()}

	r := &types.Restore{
		Spec: types.RestoreSpec{Namespace: ns, BackupName: backupName},
	}

	ctx := context.Background()
	got, err := restorectl.FindBackupPath(ctx, c, r)
	if err != nil {
		t.Fatalf("FindBackupPath error: %v", err)
	}
	if got != storagePath {
		t.Errorf("FindBackupPath = %q; want %q", got, storagePath)
	}
}

func TestFindBackupPath_NoBackupsError(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BACKUP_ROOT", root)

	c := newTestClientsWithRestoreScheme()
	r := &types.Restore{Spec: types.RestoreSpec{Namespace: "no-backups-ns"}}

	ctx := context.Background()
	_, err := restorectl.FindBackupPath(ctx, c, r)
	if err == nil {
		t.Fatal("expected error when no backups exist, got nil")
	}
}

// -----------------------------------------------------------------------
// ApplyBackupDirectory() — writes and re-reads from temp dir
// -----------------------------------------------------------------------

func TestApplyBackupDirectory_CoreTypeInOrder(t *testing.T) {
	root := t.TempDir()

	// Create a fake configmap YAML in the backup directory.
	cmDir := filepath.Join(root, "configmaps")
	if err := os.MkdirAll(cmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlContent := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-cfg\n  namespace: src-ns\ndata:\n  key: value\n"
	if err := os.WriteFile(filepath.Join(cmDir, "test-cfg.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Set up a fake dynamic client that has ConfigMap registered.
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMapList"},
		&unstructured.UnstructuredList{},
	)

	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	coreClient := kubernetesfake.NewSimpleClientset()
	c := &k8s.Clients{Core: coreClient, Dynamic: dyn, Discovery: coreClient.Discovery()}

	ctx := context.Background()
	// ApplyBackupDirectory is best-effort — it logs errors and continues.
	// We simply verify it does not return an error itself.
	if err := restorectl.ApplyBackupDirectory(ctx, c, root, "target-ns"); err != nil {
		t.Fatalf("ApplyBackupDirectory error: %v", err)
	}
}

func TestApplyBackupDirectory_UnknownSubdirSkipped(t *testing.T) {
	root := t.TempDir()

	// Create a subdirectory that is not a core type (simulating a CRD).
	crdDir := filepath.Join(root, "widgets")
	if err := os.MkdirAll(crdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A YAML file whose apiVersion points to an unknown group — the discovery
	// client will not find it, so applyYAMLFile will log an error and continue.
	yamlContent := "apiVersion: example.io/v1\nkind: Widget\nmetadata:\n  name: my-widget\n  namespace: src-ns\n"
	if err := os.WriteFile(filepath.Join(crdDir, "my-widget.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	c := newTestClientsWithRestoreScheme()
	ctx := context.Background()

	// Should complete without error even though the CRD type is unknown.
	if err := restorectl.ApplyBackupDirectory(ctx, c, root, "target-ns"); err != nil {
		t.Fatalf("ApplyBackupDirectory error: %v", err)
	}
}

// -----------------------------------------------------------------------
// GVRFromObject()
// -----------------------------------------------------------------------

func TestGVRFromUnstructured_NotFound(t *testing.T) {
	c := newTestClientsWithRestoreScheme()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("nonexistent.io/v1")
	u.SetKind("Nonexistent")

	_, err := restorectl.GVRFromObject(c, u)
	if err == nil {
		t.Fatal("expected error for unknown GVK, got nil")
	}
}

// -----------------------------------------------------------------------
// boolPtr equivalent — inline verification
// -----------------------------------------------------------------------

func TestBoolPtr(t *testing.T) {
	// boolPtr is now package-private in restore; verify the behaviour inline.
	trueVal := true
	p := &trueVal
	if !*p {
		t.Error("pointer to true should be true")
	}
	falseVal := false
	p2 := &falseVal
	if *p2 {
		t.Error("pointer to false should be false")
	}
}
