package main

// scheduled_backup.go — ScheduledBackup controller.
//
// Watches ScheduledBackup CRs (replic2.io/v1alpha1/ScheduledBackup).
// On each 5-second poll it:
//   1. Parses spec.schedule (standard 5-field cron, UTC).
//   2. If the next scheduled time has passed since the last run (or this is
//      the first run), creates a new Backup CR owned by this schedule.
//   3. Enforces spec.keepLast: deletes the oldest Backup CRs (and their PVC
//      data) once more than keepLast completed backups exist for this schedule.
//   4. Patches status.lastScheduleTime, status.lastBackupName, and
//      status.activeBackups back onto the ScheduledBackup CR.
//
// Generated Backup CRs are named:  <scheduledbackup-name>-<unix-timestamp>
// An owner label is stamped on each:  replic2.io/scheduled-by: <name>

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const scheduledByLabel = "replic2.io/scheduled-by"

var scheduledBackupGVR = schema.GroupVersionResource{
	Group:    "replic2.io",
	Version:  "v1alpha1",
	Resource: "scheduledbackups",
}

// runScheduledBackupController polls for ScheduledBackup CRs in a loop.
// It blocks until ctx is cancelled.
func runScheduledBackupController(ctx context.Context, c *clients) {
	log.Println("scheduled backup controller: started")
	for {
		select {
		case <-ctx.Done():
			log.Println("scheduled backup controller: stopped")
			return
		case <-time.After(5 * time.Second):
			if err := reconcileScheduledBackups(ctx, c); err != nil {
				log.Printf("scheduled backup controller: reconcile error: %v", err)
			}
		}
	}
}

// reconcileScheduledBackups iterates every ScheduledBackup CR and fires
// a new Backup when the cron schedule says it is due.
func reconcileScheduledBackups(ctx context.Context, c *clients) error {
	list, err := c.dynamic.Resource(scheduledBackupGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list scheduledbackups: %w", err)
	}

	for _, item := range list.Items {
		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("scheduled backup controller: marshal item: %v", err)
			continue
		}

		var sb ScheduledBackup
		if err := json.Unmarshal(raw, &sb); err != nil {
			log.Printf("scheduled backup controller: decode: %v", err)
			continue
		}

		if err := reconcileScheduledBackup(ctx, c, &sb); err != nil {
			log.Printf("scheduled backup controller: [%s] error: %v", sb.Name, err)
		}
	}
	return nil
}

// reconcileScheduledBackup handles a single ScheduledBackup CR.
func reconcileScheduledBackup(ctx context.Context, c *clients, sb *ScheduledBackup) error {
	// Parse the cron schedule (standard 5-field, interpreted as UTC).
	sched, err := cron.ParseStandard(sb.Spec.Schedule)
	if err != nil {
		return fmt.Errorf("parse schedule %q: %w", sb.Spec.Schedule, err)
	}

	now := time.Now().UTC()

	// Determine when the last backup was triggered.
	var lastRun time.Time
	if sb.Status.LastScheduleTime != nil {
		lastRun = sb.Status.LastScheduleTime.UTC()
	}

	// Calculate the next scheduled time after the last run.
	// If we have never run, use a basis far enough in the past (25 hours) so
	// that Next() is guaranteed to have already elapsed, firing immediately.
	basis := lastRun
	if basis.IsZero() {
		basis = now.Add(-25 * time.Hour)
	}
	next := sched.Next(basis)

	if now.Before(next) {
		// Not yet due — nothing to do.
		return nil
	}

	// ---- Fire a new Backup CR ----
	backupName := fmt.Sprintf("%s-%d", sb.Name, now.Unix())
	log.Printf("scheduled backup controller: [%s] firing backup %q", sb.Name, backupName)

	backupObj := map[string]interface{}{
		"apiVersion": "replic2.io/v1alpha1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name": backupName,
			"labels": map[string]interface{}{
				scheduledByLabel: sb.Name,
			},
		},
		"spec": map[string]interface{}{
			"namespace": sb.Spec.Namespace,
			"ttl":       sb.Spec.TTL,
		},
	}

	backupGVRes := schema.GroupVersionResource{
		Group:    "replic2.io",
		Version:  "v1alpha1",
		Resource: "backups",
	}

	raw, err := json.Marshal(backupObj)
	if err != nil {
		return fmt.Errorf("marshal backup: %w", err)
	}

	created, err := c.dynamic.Resource(backupGVRes).Create(ctx,
		mustUnstructured(raw), metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create backup CR: %w", err)
	}
	log.Printf("scheduled backup controller: [%s] created backup %q", sb.Name, created.GetName())

	// ---- Update status ----
	t := metav1.NewTime(now)
	sb.Status.LastScheduleTime = &t
	sb.Status.LastBackupName = backupName

	// Count active (non-expired) backups owned by this schedule.
	owned, err := listOwnedBackups(ctx, c, backupGVRes, sb.Name)
	if err != nil {
		log.Printf("scheduled backup controller: [%s] list owned: %v", sb.Name, err)
	}
	sb.Status.ActiveBackups = len(owned)
	sb.Status.Message = fmt.Sprintf("last backup: %s", backupName)

	if err := patchScheduledBackupStatus(ctx, c, sb); err != nil {
		log.Printf("scheduled backup controller: [%s] patch status: %v", sb.Name, err)
	}

	// ---- Enforce keepLast retention ----
	if sb.Spec.KeepLast > 0 {
		if err := enforceKeepLast(ctx, c, backupGVRes, sb, owned); err != nil {
			log.Printf("scheduled backup controller: [%s] keepLast: %v", sb.Name, err)
		}
	}

	return nil
}

// listOwnedBackups returns all Backup CRs that carry the scheduledByLabel
// for this ScheduledBackup, sorted oldest-first by creation timestamp.
func listOwnedBackups(ctx context.Context, c *clients, gvr schema.GroupVersionResource, sbName string) ([]Backup, error) {
	list, err := c.dynamic.Resource(gvr).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", scheduledByLabel, sbName),
	})
	if err != nil {
		return nil, err
	}

	var backups []Backup
	for _, item := range list.Items {
		raw, _ := item.MarshalJSON()
		var b Backup
		if err := json.Unmarshal(raw, &b); err != nil {
			continue
		}
		backups = append(backups, b)
	}

	// Sort oldest-first by creation timestamp.
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreationTimestamp.Before(&backups[j].CreationTimestamp)
	})
	return backups, nil
}

// enforceKeepLast deletes the oldest Backup CRs (and their PVC data) when
// the total count exceeds spec.keepLast.
func enforceKeepLast(ctx context.Context, c *clients, gvr schema.GroupVersionResource, sb *ScheduledBackup, owned []Backup) error {
	if len(owned) <= sb.Spec.KeepLast {
		return nil
	}

	toDelete := owned[:len(owned)-sb.Spec.KeepLast]
	for _, b := range toDelete {
		// Only delete completed or failed backups — leave in-progress ones alone.
		if b.Status.Phase == "InProgress" || b.Status.Phase == "Pending" || b.Status.Phase == "" {
			continue
		}
		log.Printf("scheduled backup controller: [%s] keepLast=%d — pruning %q", sb.Name, sb.Spec.KeepLast, b.Name)
		if err := deleteExpiredBackup(ctx, c, gvr, &b); err != nil {
			log.Printf("scheduled backup controller: [%s] prune %q: %v", sb.Name, b.Name, err)
		}
	}
	return nil
}

// patchScheduledBackupStatus writes only the status sub-object back to the
// API server, mirroring the strategy used by patchBackupStatus.
func patchScheduledBackupStatus(ctx context.Context, c *clients, sb *ScheduledBackup) error {
	statusOnly, err := json.Marshal(map[string]interface{}{"status": sb.Status})
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}

	_, err = c.dynamic.Resource(scheduledBackupGVR).
		Patch(ctx, sb.Name, "application/merge-patch+json", statusOnly, metav1.PatchOptions{}, "status")
	if err == nil {
		return nil
	}

	// Fallback: re-fetch, overwrite status, and Update.
	latest, getErr := c.dynamic.Resource(scheduledBackupGVR).Get(ctx, sb.Name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("patch status (subresource): %v; re-fetch: %w", err, getErr)
	}
	raw, _ := json.Marshal(sb.Status)
	var statusMap map[string]interface{}
	_ = json.Unmarshal(raw, &statusMap)
	latest.Object["status"] = statusMap
	_, updateErr := c.dynamic.Resource(scheduledBackupGVR).Update(ctx, latest, metav1.UpdateOptions{})
	if updateErr != nil {
		return fmt.Errorf("patch status (subresource): %v; update fallback: %w", err, updateErr)
	}
	return nil
}

// mustUnstructured deserialises raw JSON into an Unstructured object.
// Panics only on programmer error (bad JSON literal in this file).
func mustUnstructured(raw []byte) *unstructuredObj {
	u := &unstructuredObj{}
	if err := json.Unmarshal(raw, &u.Object); err != nil {
		panic(fmt.Sprintf("mustUnstructured: %v", err))
	}
	return u
}
