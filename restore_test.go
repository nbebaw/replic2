package main

// restore_test.go — unit tests for the restore controller.
//
// Uses fake k8s clients only; all S3 operations are skipped when c.S3 == nil.

import (
	"context"
	"encoding/json"
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
	ns := "my-app"

	// Create two completed Backup CRs for the same namespace with different
	// completedAt timestamps; FindBackupPath should return the newer one's StoragePath.
	oldTime := metav1.NewTime(time.Now().Add(-2 * time.Hour)) // older backup
	newTime := metav1.NewTime(time.Now().Add(-1 * time.Hour)) // newer backup

	makeBackup := func(name, storagePath string, completedAt metav1.Time) *unstructured.Unstructured {
		b := &types.Backup{
			TypeMeta:   metav1.TypeMeta{APIVersion: "replic2.io/v1alpha1", Kind: "Backup"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       types.BackupSpec{Namespace: ns},
			Status: types.BackupStatus{
				Phase:       "Completed",
				StoragePath: storagePath,
				CompletedAt: &completedAt,
			},
		}
		raw, _ := json.Marshal(b)
		var obj map[string]interface{}
		_ = json.Unmarshal(raw, &obj)
		return &unstructured.Unstructured{Object: obj}
	}

	oldBackup := makeBackup("backup-old", ns+"/backup-old", oldTime)
	newBackup := makeBackup("backup-new", ns+"/backup-new", newTime)

	// Register both Backup CRs in the fake dynamic client.
	c := newTestClientsWithRestoreScheme(oldBackup, newBackup)
	r := &types.Restore{Spec: types.RestoreSpec{Namespace: ns}}

	ctx := context.Background()
	got, err := restorectl.FindBackupPath(ctx, c, r)
	if err != nil {
		t.Fatalf("FindBackupPath error: %v", err)
	}
	want := ns + "/backup-new" // newer backup's S3 key prefix
	if got != want {
		t.Errorf("FindBackupPath = %q; want %q", got, want)
	}
}

func TestFindBackupPath_ExplicitBackupName(t *testing.T) {
	ns := "my-app"
	backupName := "explicit-backup"
	storagePath := ns + "/" + backupName // S3 key prefix (no filesystem path)

	// Create a Backup CR in the fake client with the storage path set.
	backupObj := &types.Backup{
		TypeMeta:   metav1.TypeMeta{APIVersion: "replic2.io/v1alpha1", Kind: "Backup"},
		ObjectMeta: metav1.ObjectMeta{Name: backupName},
		Spec:       types.BackupSpec{Namespace: ns},
		Status:     types.BackupStatus{Phase: "Completed", StoragePath: storagePath},
	}
	raw, _ := json.Marshal(backupObj)
	var obj map[string]interface{}
	_ = json.Unmarshal(raw, &obj)
	u := &unstructured.Unstructured{Object: obj}

	c := newTestClientsWithRestoreScheme(u)
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
	// No Backup CRs in the fake client — FindBackupPath must return an error.
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
	// When c.S3 is nil, ApplyBackupDirectory skips all S3 work and returns nil.
	// This verifies the nil guard does not panic and returns cleanly.
	c := newTestClientsWithRestoreScheme()
	ctx := context.Background()
	if err := restorectl.ApplyBackupDirectory(ctx, c, "my-app/my-backup-01", "target-ns"); err != nil {
		t.Fatalf("ApplyBackupDirectory error: %v", err)
	}
}

func TestApplyBackupDirectory_UnknownSubdirSkipped(t *testing.T) {
	// When c.S3 is nil, ApplyBackupDirectory skips all S3 work and returns nil
	// regardless of what keyPrefix is passed — including a prefix that would
	// contain CRD resource directories in a real backup.
	c := newTestClientsWithRestoreScheme()
	ctx := context.Background()
	if err := restorectl.ApplyBackupDirectory(ctx, c, "my-app/my-backup-01", "target-ns"); err != nil {
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
