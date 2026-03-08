// process.go — the main per-Backup-CR workflow.
//
// process() is called once per CR that is in phase "" or "Pending".
// It orchestrates every step: status transitions, manifest backup, optional
// PVC data backup, and final status update.
//
// findLatestCompletedBackup() is a helper used by process() to decide whether
// to run a Full or Incremental backup.
package backup

import (
	"context"       // for cancellation / deadlines
	"encoding/json" // to decode unstructured CR bytes from the dynamic client
	"fmt"           // for error wrapping
	"log"           // for structured logging
	"time"          // for sinceTime (incremental cut-off)

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // Now(), ListOptions
	"k8s.io/apimachinery/pkg/runtime/schema"      // GroupVersionResource (allTypes)

	"replic2/internal/k8s"            // Kubernetes client wrapper
	apitypes "replic2/internal/types" // Backup struct, phase constants, type constants
)

// process runs the full backup workflow for a single Backup CR.
// It is called in its own goroutine by reconcile().
func process(ctx context.Context, c *k8s.Clients, b *apitypes.Backup) error {
	ns := b.Spec.Namespace // the namespace we are backing up
	log.Printf("backup controller: [%s] backing up namespace %q", b.Name, ns)

	// -----------------------------------------------------------------------
	// 1. Transition to InProgress so we know the backup has started.
	// -----------------------------------------------------------------------
	now := metav1.Now() // capture the start timestamp
	b.Status.Phase = apitypes.PhaseInProgress
	b.Status.StartedAt = &now
	b.Status.Message = fmt.Sprintf("backing up namespace %q", ns)
	if err := patchStatus(ctx, c, b); err != nil {
		return fmt.Errorf("set InProgress: %w", err)
	}

	// -----------------------------------------------------------------------
	// 2. Decide Full vs Incremental.
	//    findLatestCompletedBackup returns nil, nil when no prior backup exists
	//    — that is normal and means we should do a Full backup.
	// -----------------------------------------------------------------------
	prev, err := findLatestCompletedBackup(ctx, c, ns)
	if err != nil {
		return markFailed(ctx, c, b, fmt.Errorf("lookup previous backup: %w", err))
	}

	backupType := b.Spec.Type // use whatever the user explicitly requested
	switch backupType {
	case apitypes.BackupTypeFull, apitypes.BackupTypeIncremental:
		// Explicit type — honour the user's choice as-is.
	default:
		// Auto-select: Full on first run, Incremental when a prior backup exists.
		if prev == nil {
			backupType = apitypes.BackupTypeFull // no history → must be full
		} else {
			backupType = apitypes.BackupTypeIncremental // history found → incremental
		}
	}

	// Record what we decided in the status for observability.
	b.Status.BackupType = backupType
	if prev != nil && backupType == apitypes.BackupTypeIncremental {
		b.Status.BasedOn = prev.Name // link this incremental to its base
		log.Printf("backup controller: [%s] incremental backup based on %q", b.Name, prev.Name)
	} else {
		log.Printf("backup controller: [%s] full backup", b.Name)
	}

	// -----------------------------------------------------------------------
	// 3. Build the S3 key prefix for this backup.
	//    Format: <namespace>/<backup-name>
	//    Example: my-app/my-backup-01
	//    All manifests and PVC archives live under this prefix in the bucket.
	// -----------------------------------------------------------------------
	keyPrefix := ns + "/" + b.Name // S3 key prefix (replaces the old filesystem path)

	// -----------------------------------------------------------------------
	// 4. Manifest backup — serialise every resource to YAML and upload to S3.
	//    Start with the ordered coreResourceTypes, then append any CRDs found
	//    via API discovery (cert-manager, Prometheus, Argo CD, Istio, …).
	// -----------------------------------------------------------------------
	allTypes := append([]schema.GroupVersionResource(nil), coreResourceTypes...) // defensive copy
	crdTypes, err := discoverCRDTypes(c)
	if err != nil {
		// Discovery errors are non-fatal: we continue with core types only.
		log.Printf("backup controller: [%s] CRD discovery error (continuing with core types only): %v", b.Name, err)
	} else if len(crdTypes) > 0 {
		log.Printf("backup controller: [%s] discovered %d additional CRD types", b.Name, len(crdTypes))
		allTypes = append(allTypes, crdTypes...) // add discovered CRDs to the list
	}

	for _, gvr := range allTypes {
		if err := backupResourceType(ctx, c, ns, keyPrefix, gvr); err != nil {
			// A missing or unavailable API group should not abort the whole backup.
			log.Printf("backup controller: [%s] skip %s: %v", b.Name, gvr.Resource, err)
		}
	}

	// -----------------------------------------------------------------------
	// 5. PVC data backup — opt-in via spec.includePVCData.
	//    For incremental runs we pass the previous backup's completedAt as the
	//    mtime cut-off so only changed files are archived.
	// -----------------------------------------------------------------------
	if b.Spec.IncludePVCData {
		var sinceTime time.Time // zero value → full backup (all files)
		if backupType == apitypes.BackupTypeIncremental && prev != nil && prev.Status.CompletedAt != nil {
			sinceTime = prev.Status.CompletedAt.Time // use previous backup's completion time
		}
		if err := backupPVCData(ctx, c, b, ns, keyPrefix, sinceTime); err != nil {
			return markFailed(ctx, c, b, fmt.Errorf("PVC data backup: %w", err))
		}
	}

	// -----------------------------------------------------------------------
	// 6. Transition to Completed.
	// -----------------------------------------------------------------------
	done := metav1.Now() // capture the end timestamp
	b.Status.Phase = apitypes.PhaseCompleted
	b.Status.CompletedAt = &done
	b.Status.StoragePath = keyPrefix // S3 key prefix where all data lives
	b.Status.Message = fmt.Sprintf("%s backup complete — %d resource types captured", backupType, len(allTypes))
	if err := patchStatus(ctx, c, b); err != nil {
		return fmt.Errorf("set Completed: %w", err)
	}

	log.Printf("backup controller: [%s] done — s3 key prefix: %s", b.Name, keyPrefix)
	return nil
}

// findLatestCompletedBackup scans all Backup CRs and returns the most recently
// completed one for the given namespace.
//
// Returns nil, nil — not an error — when no completed backup exists.  The
// caller interprets nil as "run a Full backup".
func findLatestCompletedBackup(ctx context.Context, c *k8s.Clients, namespace string) (*apitypes.Backup, error) {
	// List every Backup CR cluster-wide (we filter by namespace below).
	list, err := c.Dynamic.Resource(GVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}

	var latest *apitypes.Backup // track the most recently completed backup
	for _, item := range list.Items {
		// Decode the unstructured object into our typed Backup struct.
		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("backup controller: marshal item: %v", err)
			continue // skip items we can't read
		}

		var b apitypes.Backup
		if err := json.Unmarshal(raw, &b); err != nil {
			log.Printf("backup controller: decode backup: %v", err)
			continue // skip malformed CRs
		}

		// Only consider completed backups in the target namespace.
		if b.Spec.Namespace != namespace || b.Status.Phase != apitypes.PhaseCompleted {
			continue
		}
		// Guard: a completed backup without a CompletedAt timestamp is invalid.
		if b.Status.CompletedAt == nil {
			continue
		}

		bCopy := b // capture a copy to avoid the loop-variable aliasing trap
		if latest == nil || bCopy.Status.CompletedAt.After(latest.Status.CompletedAt.Time) {
			latest = &bCopy // found a more recent completed backup
		}
	}

	return latest, nil // nil means "no prior backup found" — not an error
}
