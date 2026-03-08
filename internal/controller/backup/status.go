// status.go — Backup CR status management helpers.
//
// This file owns everything related to writing status back to the API server
// and to the lifecycle of a Backup CR (TTL expiry, failure marking).
//
// Functions:
//   - patchStatus   — write status sub-object back to the API server
//   - markFailed    — convenience: set phase=Failed and call patchStatus
//   - isExpired     — check whether spec.ttl has elapsed since completedAt
//   - deleteExpired — remove PVC data + the Backup CR itself
//
// The exported wrappers IsExpired and DeleteExpired are used by the scheduled
// backup controller and by the test suite.
package backup

import (
	"context"       // for cancellation / deadlines
	"encoding/json" // to marshal the status struct to JSON for the patch body
	"fmt"           // for error wrapping
	"log"           // for structured logging
	"os"            // for RemoveAll (delete PVC data on expiry)
	"time"          // for ParseDuration and time.Now (TTL check)

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // Now(), DeleteOptions, PatchOptions
	"k8s.io/apimachinery/pkg/types"               // MergePatchType

	"replic2/internal/k8s"            // Kubernetes client wrapper
	apitypes "replic2/internal/types" // Backup struct, phase constants
)

// patchStatus writes only the status sub-object back to the API server.
//
// Strategy:
//  1. Try a merge-patch on the /status subresource (preferred — avoids
//     resourceVersion conflicts from concurrent updates).
//  2. Fall back to a full Update if the subresource is unavailable (e.g. in
//     the fake client used by tests).
func patchStatus(ctx context.Context, c *k8s.Clients, b *apitypes.Backup) error {
	// Build the JSON merge-patch body: {"status": { … }}.
	statusOnly, err := json.Marshal(map[string]interface{}{"status": b.Status})
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}

	// Attempt subresource patch — the recommended way to update status.
	_, err = c.Dynamic.Resource(GVR).
		Patch(ctx, b.Name, types.MergePatchType, statusOnly, metav1.PatchOptions{}, "status")
	if err == nil {
		return nil // success — nothing more to do
	}

	// Fallback: re-fetch the latest object (to get the current resourceVersion),
	// overwrite its status map, and call Update.
	latest, getErr := c.Dynamic.Resource(GVR).Get(ctx, b.Name, metav1.GetOptions{})
	if getErr != nil {
		// We lost both strategies — surface both errors.
		return fmt.Errorf("patch status (subresource): %v; re-fetch: %w", err, getErr)
	}

	// Convert b.Status to a plain map[string]interface{} and inject it.
	raw, _ := json.Marshal(b.Status) // marshal our typed struct to JSON
	var statusMap map[string]interface{}
	_ = json.Unmarshal(raw, &statusMap) // decode into a generic map
	latest.Object["status"] = statusMap // overwrite the status field

	if _, updateErr := c.Dynamic.Resource(GVR).Update(ctx, latest, metav1.UpdateOptions{}); updateErr != nil {
		return fmt.Errorf("patch status (subresource): %v; update fallback: %w", err, updateErr)
	}
	return nil
}

// markFailed transitions the Backup CR to phase=Failed, records the error
// message, patches the status, and returns the original cause unchanged so
// callers can return it directly with "return markFailed(…)".
func markFailed(ctx context.Context, c *k8s.Clients, b *apitypes.Backup, cause error) error {
	now := metav1.Now() // record when the failure was detected
	b.Status.Phase = apitypes.PhaseFailed
	b.Status.CompletedAt = &now      // set completedAt so the TTL clock starts
	b.Status.Message = cause.Error() // human-readable failure reason
	_ = patchStatus(ctx, c, b)       // best-effort — ignore patch error on failure path
	return cause                     // propagate the original error to the caller
}

// isExpired returns true when spec.ttl is non-empty, completedAt is set, and
// completedAt + ttl ≤ now.  Returns false (not expired) when either field is
// missing so that backups without a TTL are kept indefinitely.
func isExpired(b *apitypes.Backup) (bool, error) {
	if b.Spec.TTL == "" || b.Status.CompletedAt == nil {
		return false, nil // no TTL configured, or backup not yet completed — keep it
	}
	ttl, err := time.ParseDuration(b.Spec.TTL) // parse "24h", "7d", etc.
	if err != nil {
		return false, fmt.Errorf("parse TTL %q: %w", b.Spec.TTL, err)
	}
	expiry := b.Status.CompletedAt.Time.Add(ttl) // absolute expiry time
	return !time.Now().Before(expiry), nil       // expired when now ≥ expiry
}

// IsExpired is the exported version used by the scheduled backup controller
// and by the test suite.
func IsExpired(b *apitypes.Backup) (bool, error) { return isExpired(b) }

// deleteExpired removes the PVC data directory and the Backup CR itself.
// It is called by reconcile() in a goroutine when isExpired returns true.
func deleteExpired(ctx context.Context, c *k8s.Clients, b *apitypes.Backup) error {
	log.Printf("backup controller: [%s] TTL expired — deleting", b.Name)

	// 1. Remove data from the backup PVC (best-effort).
	//    If the path does not exist (already cleaned up) that is fine.
	if b.Status.StoragePath != "" {
		if err := os.RemoveAll(b.Status.StoragePath); err != nil && !os.IsNotExist(err) {
			log.Printf("backup controller: [%s] remove %q: %v", b.Name, b.Status.StoragePath, err)
			// Non-fatal: continue to delete the CR even if filesystem removal fails.
		}
	}

	// 2. Delete the Backup CR from the cluster.
	if err := c.Dynamic.Resource(GVR).Delete(ctx, b.Name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete Backup CR %q: %w", b.Name, err)
	}
	log.Printf("backup controller: [%s] deleted (TTL expired)", b.Name)
	return nil
}

// DeleteExpired is the exported version used by the scheduled backup controller
// for keepLast pruning and by the test suite.
func DeleteExpired(ctx context.Context, c *k8s.Clients, b *apitypes.Backup) error {
	return deleteExpired(ctx, c, b)
}
