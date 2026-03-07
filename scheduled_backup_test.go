package main

// scheduled_backup_test.go — unit tests for the ScheduledBackup controller.

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
)

// newTestClientsWithScheduleScheme registers Backup and ScheduledBackup kinds
// in the fake dynamic client scheme.
func newTestClientsWithScheduleScheme(objects ...runtime.Object) *clients {
	scheme := runtime.NewScheme()
	for _, info := range []struct{ kind, list, group, version string }{
		{"Backup", "BackupList", "replic2.io", "v1alpha1"},
		{"ScheduledBackup", "ScheduledBackupList", "replic2.io", "v1alpha1"},
	} {
		scheme.AddKnownTypeWithName(
			schema.GroupVersionKind{Group: info.group, Version: info.version, Kind: info.kind},
			&unstructured.Unstructured{},
		)
		scheme.AddKnownTypeWithName(
			schema.GroupVersionKind{Group: info.group, Version: info.version, Kind: info.list},
			&unstructured.UnstructuredList{},
		)
	}
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, objects...)
	core := kubernetesfake.NewSimpleClientset()
	return &clients{core: core, dynamic: dyn, discovery: core.Discovery()}
}

// makeScheduledBackupUnstructured builds a ScheduledBackup unstructured object.
func makeScheduledBackupUnstructured(name, namespace, schedule string, keepLast int, lastRunTime *time.Time) *unstructured.Unstructured {
	sb := &ScheduledBackup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "replic2.io/v1alpha1",
			Kind:       "ScheduledBackup",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ScheduledBackupSpec{
			Namespace: namespace,
			Schedule:  schedule,
			KeepLast:  keepLast,
		},
	}
	if lastRunTime != nil {
		t := metav1.NewTime(*lastRunTime)
		sb.Status.LastScheduleTime = &t
	}
	raw, _ := json.Marshal(sb)
	var obj map[string]interface{}
	_ = json.Unmarshal(raw, &obj)
	return &unstructured.Unstructured{Object: obj}
}

// -----------------------------------------------------------------------
// reconcileScheduledBackup() — due / not due
// -----------------------------------------------------------------------

func TestReconcileScheduledBackup_FiresWhenDue(t *testing.T) {
	// Use a schedule that was last run 2 minutes ago with a 1-minute interval.
	// "* * * * *" = every minute, so it is overdue.
	lastRun := time.Now().UTC().Add(-2 * time.Minute)
	sbObj := makeScheduledBackupUnstructured("every-minute", "demo-app", "* * * * *", 0, &lastRun)
	c := newTestClientsWithScheduleScheme(sbObj)

	sb := &ScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "every-minute"},
		Spec: ScheduledBackupSpec{
			Namespace: "demo-app",
			Schedule:  "* * * * *",
		},
		Status: ScheduledBackupStatus{
			LastScheduleTime: func() *metav1.Time { t := metav1.NewTime(lastRun); return &t }(),
		},
	}

	ctx := context.Background()
	if err := reconcileScheduledBackup(ctx, c, sb); err != nil {
		t.Fatalf("reconcileScheduledBackup error: %v", err)
	}

	// A Backup CR should have been created.
	list, err := c.dynamic.Resource(schema.GroupVersionResource{
		Group: "replic2.io", Version: "v1alpha1", Resource: "backups",
	}).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(list.Items) == 0 {
		t.Error("expected a Backup CR to be created, got none")
	}
	// Verify the label is set.
	labels := list.Items[0].GetLabels()
	if labels[scheduledByLabel] != "every-minute" {
		t.Errorf("scheduled-by label = %q; want every-minute", labels[scheduledByLabel])
	}
}

func TestReconcileScheduledBackup_SkipsWhenNotDue(t *testing.T) {
	// Last ran 10 seconds ago; schedule is every hour — not due.
	lastRun := time.Now().UTC().Add(-10 * time.Second)
	c := newTestClientsWithScheduleScheme()

	sb := &ScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "hourly"},
		Spec: ScheduledBackupSpec{
			Namespace: "demo-app",
			Schedule:  "0 * * * *", // every hour
		},
		Status: ScheduledBackupStatus{
			LastScheduleTime: func() *metav1.Time { t := metav1.NewTime(lastRun); return &t }(),
		},
	}

	ctx := context.Background()
	if err := reconcileScheduledBackup(ctx, c, sb); err != nil {
		t.Fatalf("reconcileScheduledBackup error: %v", err)
	}

	// No Backup CR should have been created.
	list, err := c.dynamic.Resource(schema.GroupVersionResource{
		Group: "replic2.io", Version: "v1alpha1", Resource: "backups",
	}).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected no Backup CRs, got %d", len(list.Items))
	}
}

func TestReconcileScheduledBackup_FirstRunFiresImmediately(t *testing.T) {
	// No lastScheduleTime — should fire on first reconcile.
	c := newTestClientsWithScheduleScheme()

	sb := &ScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "first-run"},
		Spec: ScheduledBackupSpec{
			Namespace: "demo-app",
			Schedule:  "0 2 * * *", // daily at 02:00 — irrelevant, first run always fires
		},
	}

	ctx := context.Background()
	if err := reconcileScheduledBackup(ctx, c, sb); err != nil {
		t.Fatalf("reconcileScheduledBackup error: %v", err)
	}

	list, err := c.dynamic.Resource(schema.GroupVersionResource{
		Group: "replic2.io", Version: "v1alpha1", Resource: "backups",
	}).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(list.Items) == 0 {
		t.Error("expected a Backup CR on first run, got none")
	}
}

// -----------------------------------------------------------------------
// reconcileScheduledBackups() — list iteration
// -----------------------------------------------------------------------

func TestReconcileScheduledBackups_InvalidScheduleLogsAndContinues(t *testing.T) {
	sbObj := makeScheduledBackupUnstructured("bad-cron", "demo-app", "not-a-cron", 0, nil)
	c := newTestClientsWithScheduleScheme(sbObj)

	ctx := context.Background()
	// Should not return an error — bad schedule is logged per-CR, not fatal.
	if err := reconcileScheduledBackups(ctx, c); err != nil {
		t.Fatalf("reconcileScheduledBackups error: %v", err)
	}
}

// -----------------------------------------------------------------------
// listOwnedBackups() — label selector
// -----------------------------------------------------------------------

func TestListOwnedBackups_ReturnsOnlyOwned(t *testing.T) {
	makeBackup := func(name, owner string, completedAt time.Time) *unstructured.Unstructured {
		b := &Backup{
			TypeMeta:   metav1.TypeMeta{APIVersion: "replic2.io/v1alpha1", Kind: "Backup"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{scheduledByLabel: owner}},
			Status:     BackupStatus{Phase: "Completed", CompletedAt: func() *metav1.Time { t := metav1.NewTime(completedAt); return &t }()},
		}
		raw, _ := json.Marshal(b)
		var obj map[string]interface{}
		_ = json.Unmarshal(raw, &obj)
		return &unstructured.Unstructured{Object: obj}
	}

	b1 := makeBackup("sched-a-1", "sched-a", time.Now().Add(-10*time.Minute))
	b2 := makeBackup("sched-a-2", "sched-a", time.Now().Add(-5*time.Minute))
	b3 := makeBackup("sched-b-1", "sched-b", time.Now().Add(-3*time.Minute))

	c := newTestClientsWithScheduleScheme(b1, b2, b3)
	gvr := schema.GroupVersionResource{Group: "replic2.io", Version: "v1alpha1", Resource: "backups"}

	ctx := context.Background()
	owned, err := listOwnedBackups(ctx, c, gvr, "sched-a")
	if err != nil {
		t.Fatalf("listOwnedBackups error: %v", err)
	}
	if len(owned) != 2 {
		t.Errorf("expected 2 owned backups for sched-a, got %d", len(owned))
	}
}

// -----------------------------------------------------------------------
// enforceKeepLast()
// -----------------------------------------------------------------------

func TestEnforceKeepLast_DeletesOldest(t *testing.T) {
	now := time.Now()

	makeCompletedBackup := func(name string, age time.Duration) Backup {
		completedAt := metav1.NewTime(now.Add(-age))
		createdAt := metav1.NewTime(now.Add(-age))
		return Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				CreationTimestamp: createdAt,
			},
			Status: BackupStatus{Phase: "Completed", CompletedAt: &completedAt},
		}
	}

	// 3 completed backups; keepLast=2 should delete the oldest one.
	oldest := makeCompletedBackup("backup-old", 30*time.Minute)
	middle := makeCompletedBackup("backup-mid", 20*time.Minute)
	newest := makeCompletedBackup("backup-new", 10*time.Minute)

	// Register all three in the fake dynamic client.
	toUnstructured := func(b Backup) *unstructured.Unstructured {
		b.TypeMeta = metav1.TypeMeta{APIVersion: "replic2.io/v1alpha1", Kind: "Backup"}
		raw, _ := json.Marshal(b)
		var obj map[string]interface{}
		_ = json.Unmarshal(raw, &obj)
		return &unstructured.Unstructured{Object: obj}
	}

	c := newTestClientsWithScheduleScheme(
		toUnstructured(oldest),
		toUnstructured(middle),
		toUnstructured(newest),
	)

	sb := &ScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "my-schedule"},
		Spec:       ScheduledBackupSpec{KeepLast: 2},
	}
	owned := []Backup{oldest, middle, newest} // already sorted oldest-first
	gvr := schema.GroupVersionResource{Group: "replic2.io", Version: "v1alpha1", Resource: "backups"}

	ctx := context.Background()
	if err := enforceKeepLast(ctx, c, gvr, sb, owned); err != nil {
		t.Fatalf("enforceKeepLast error: %v", err)
	}

	// backup-old should be deleted; backup-mid and backup-new should remain.
	list, err := c.dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	remaining := map[string]bool{}
	for _, item := range list.Items {
		remaining[item.GetName()] = true
	}
	if remaining["backup-old"] {
		t.Error("expected backup-old to be deleted by keepLast=2")
	}
	if !remaining["backup-mid"] {
		t.Error("expected backup-mid to be retained")
	}
	if !remaining["backup-new"] {
		t.Error("expected backup-new to be retained")
	}
}

func TestEnforceKeepLast_SkipsInProgress(t *testing.T) {
	now := time.Now()
	inProgress := Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "in-progress-backup",
			CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Hour)),
		},
		Status: BackupStatus{Phase: "InProgress"},
	}

	inProgressU := func() *unstructured.Unstructured {
		inProgress.TypeMeta = metav1.TypeMeta{APIVersion: "replic2.io/v1alpha1", Kind: "Backup"}
		raw, _ := json.Marshal(inProgress)
		var obj map[string]interface{}
		_ = json.Unmarshal(raw, &obj)
		return &unstructured.Unstructured{Object: obj}
	}()

	c := newTestClientsWithScheduleScheme(inProgressU)
	sb := &ScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "my-schedule"},
		Spec:       ScheduledBackupSpec{KeepLast: 0}, // keepLast=0 means keep all anyway
	}

	gvr := schema.GroupVersionResource{Group: "replic2.io", Version: "v1alpha1", Resource: "backups"}
	ctx := context.Background()

	// enforceKeepLast should not delete an in-progress backup.
	if err := enforceKeepLast(ctx, c, gvr, sb, []Backup{inProgress}); err != nil {
		t.Fatalf("enforceKeepLast error: %v", err)
	}

	list, _ := c.dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	if len(list.Items) == 0 {
		t.Error("expected in-progress backup to be preserved")
	}
}

// -----------------------------------------------------------------------
// mustUnstructured()
// -----------------------------------------------------------------------

func TestMustUnstructured_ValidJSON(t *testing.T) {
	raw := []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test"}}`)
	u := mustUnstructured(raw)
	if u.GetName() != "test" {
		t.Errorf("name = %q; want test", u.GetName())
	}
}
