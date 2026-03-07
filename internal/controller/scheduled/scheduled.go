// Package scheduled implements the ScheduledBackup controller.
//
// The controller watches ScheduledBackup CRs (replic2.io/v1alpha1/ScheduledBackup).
// On each 5-second poll it:
//
//  1. Parses spec.schedule (standard 5-field cron expression, UTC).
//  2. If the next scheduled time has elapsed since the last run (or this is
//     the first run), creates a new Backup CR owned by this schedule.
//  3. Enforces spec.keepLast: deletes the oldest Backup CRs once more than
//     keepLast completed backups exist for this schedule.
//  4. Patches status.lastScheduleTime, lastBackupName, and activeBackups.
//
// Generated Backup CRs are named: <scheduledbackup-name>-<unix-timestamp>
// An owner label is stamped on each: replic2.io/scheduled-by: <name>
package scheduled

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"replic2/internal/controller/backup"
	"replic2/internal/k8s"
	apitypes "replic2/internal/types"
)

// ScheduledByLabel is the label key stamped on every Backup CR created by a
// ScheduledBackup controller so they can be listed and pruned by owner.
const ScheduledByLabel = "replic2.io/scheduled-by"

var gvr = schema.GroupVersionResource{
	Group:    "replic2.io",
	Version:  "v1alpha1",
	Resource: "scheduledbackups",
}

// Run polls for ScheduledBackup CRs every 5 seconds until ctx is cancelled.
func Run(ctx context.Context, c *k8s.Clients) {
	log.Println("scheduled backup controller: started")
	for {
		select {
		case <-ctx.Done():
			log.Println("scheduled backup controller: stopped")
			return
		case <-time.After(5 * time.Second):
			if err := reconcileAll(ctx, c); err != nil {
				log.Printf("scheduled backup controller: reconcile error: %v", err)
			}
		}
	}
}

// ReconcileAll is the exported entry point for tests.
func ReconcileAll(ctx context.Context, c *k8s.Clients) error { return reconcileAll(ctx, c) }

// reconcileAll iterates every ScheduledBackup CR and fires a new Backup when
// the cron schedule says it is due.
func reconcileAll(ctx context.Context, c *k8s.Clients) error {
	list, err := c.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list scheduledbackups: %w", err)
	}

	for _, item := range list.Items {
		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("scheduled backup controller: marshal item: %v", err)
			continue
		}

		var sb apitypes.ScheduledBackup
		if err := json.Unmarshal(raw, &sb); err != nil {
			log.Printf("scheduled backup controller: decode: %v", err)
			continue
		}

		if err := reconcileOne(ctx, c, &sb); err != nil {
			log.Printf("scheduled backup controller: [%s] error: %v", sb.Name, err)
		}
	}
	return nil
}

// ReconcileOne is the exported entry point for tests.
func ReconcileOne(ctx context.Context, c *k8s.Clients, sb *apitypes.ScheduledBackup) error {
	return reconcileOne(ctx, c, sb)
}

// reconcileOne handles a single ScheduledBackup CR.
func reconcileOne(ctx context.Context, c *k8s.Clients, sb *apitypes.ScheduledBackup) error {
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

	// If we have never run, use a basis 25 h in the past so that Next() has
	// already elapsed, triggering an immediate first run.
	basis := lastRun
	if basis.IsZero() {
		basis = now.Add(-25 * time.Hour)
	}

	if now.Before(sched.Next(basis)) {
		return nil // not yet due
	}

	// ---- Fire a new Backup CR ----
	backupName := fmt.Sprintf("%s-%d", sb.Name, now.Unix())
	log.Printf("scheduled backup controller: [%s] firing backup %q", sb.Name, backupName)

	backupPayload := map[string]interface{}{
		"apiVersion": "replic2.io/v1alpha1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name":   backupName,
			"labels": map[string]interface{}{ScheduledByLabel: sb.Name},
		},
		"spec": map[string]interface{}{
			"namespace": sb.Spec.Namespace,
			"ttl":       sb.Spec.TTL,
		},
	}

	raw, err := json.Marshal(backupPayload)
	if err != nil {
		return fmt.Errorf("marshal backup payload: %w", err)
	}

	created, err := c.Dynamic.Resource(backup.GVR).Create(ctx, mustUnstructured(raw), metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create backup CR: %w", err)
	}
	log.Printf("scheduled backup controller: [%s] created backup %q", sb.Name, created.GetName())

	// ---- Update ScheduledBackup status ----
	t := metav1.NewTime(now)
	sb.Status.LastScheduleTime = &t
	sb.Status.LastBackupName = backupName

	owned, err := ListOwnedBackups(ctx, c, sb.Name)
	if err != nil {
		log.Printf("scheduled backup controller: [%s] list owned: %v", sb.Name, err)
	}
	sb.Status.ActiveBackups = len(owned)
	sb.Status.Message = fmt.Sprintf("last backup: %s", backupName)

	if err := patchStatus(ctx, c, sb); err != nil {
		log.Printf("scheduled backup controller: [%s] patch status: %v", sb.Name, err)
	}

	// ---- Enforce keepLast retention ----
	if sb.Spec.KeepLast > 0 {
		if err := EnforceKeepLast(ctx, c, sb, owned); err != nil {
			log.Printf("scheduled backup controller: [%s] keepLast: %v", sb.Name, err)
		}
	}

	return nil
}

// ListOwnedBackups returns all Backup CRs labelled with the owner's name,
// sorted oldest-first by creation timestamp.
func ListOwnedBackups(ctx context.Context, c *k8s.Clients, ownerName string) ([]apitypes.Backup, error) {
	list, err := c.Dynamic.Resource(backup.GVR).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", ScheduledByLabel, ownerName),
	})
	if err != nil {
		return nil, err
	}

	var backups []apitypes.Backup
	for _, item := range list.Items {
		raw, _ := item.MarshalJSON()
		var b apitypes.Backup
		if err := json.Unmarshal(raw, &b); err != nil {
			continue
		}
		backups = append(backups, b)
	}

	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreationTimestamp.Before(&backups[j].CreationTimestamp)
	})
	return backups, nil
}

// EnforceKeepLast deletes the oldest Backup CRs when the total count exceeds
// spec.keepLast.  In-progress and pending backups are left untouched.
func EnforceKeepLast(ctx context.Context, c *k8s.Clients, sb *apitypes.ScheduledBackup, owned []apitypes.Backup) error {
	if len(owned) <= sb.Spec.KeepLast {
		return nil
	}

	toDelete := owned[:len(owned)-sb.Spec.KeepLast]
	for _, b := range toDelete {
		if b.Status.Phase == apitypes.PhaseInProgress ||
			b.Status.Phase == apitypes.PhasePending ||
			b.Status.Phase == "" {
			continue // never delete a running backup
		}
		log.Printf("scheduled backup controller: [%s] keepLast=%d — pruning %q", sb.Name, sb.Spec.KeepLast, b.Name)
		if err := backup.DeleteExpired(ctx, c, &b); err != nil {
			log.Printf("scheduled backup controller: [%s] prune %q: %v", sb.Name, b.Name, err)
		}
	}
	return nil
}

// patchStatus writes only the status sub-object back to the API server.
func patchStatus(ctx context.Context, c *k8s.Clients, sb *apitypes.ScheduledBackup) error {
	statusOnly, err := json.Marshal(map[string]interface{}{"status": sb.Status})
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}

	_, err = c.Dynamic.Resource(gvr).
		Patch(ctx, sb.Name, types.MergePatchType, statusOnly, metav1.PatchOptions{}, "status")
	if err == nil {
		return nil
	}

	// Fallback: re-fetch, overwrite status, Update.
	latest, getErr := c.Dynamic.Resource(gvr).Get(ctx, sb.Name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("patch status (subresource): %v; re-fetch: %w", err, getErr)
	}
	raw, _ := json.Marshal(sb.Status)
	var statusMap map[string]interface{}
	_ = json.Unmarshal(raw, &statusMap)
	latest.Object["status"] = statusMap
	if _, updateErr := c.Dynamic.Resource(gvr).Update(ctx, latest, metav1.UpdateOptions{}); updateErr != nil {
		return fmt.Errorf("patch status (subresource): %v; update fallback: %w", err, updateErr)
	}
	return nil
}

// mustUnstructured deserialises raw JSON into an Unstructured object.
// Panics only on programmer error (invalid JSON literal in this file).
func mustUnstructured(raw []byte) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, &u.Object); err != nil {
		panic(fmt.Sprintf("mustUnstructured: %v", err))
	}
	return u
}

// MustUnstructured is the exported version used in tests.
func MustUnstructured(raw []byte) *unstructured.Unstructured { return mustUnstructured(raw) }
