// Package backup implements the Backup controller.
//
// The controller watches Backup CRs (replic2.io/v1alpha1/Backup) cluster-wide.
// When a new CR appears with phase "" or "Pending" it:
//
//  1. Sets phase → InProgress.
//  2. Determines backup type (Full or Incremental):
//     - Full if no prior completed backup exists for the namespace, or if
//     spec.type is explicitly "Full".
//     - Incremental otherwise: only manifests/files changed since the previous
//     backup are written.
//  3. Backs up Kubernetes manifests:
//     a. The hardcoded coreResourceTypes (dependency-ordered).
//     b. Every namespace-scoped CRD found via API discovery (third-party
//     operators: cert-manager, Prometheus, Argo CD, Istio, …).
//  4. If spec.includePVCData is true, backs up raw PVC data by spawning a
//     temporary agent pod that tars the PVC contents and streams them to the
//     backup PVC (which is already mounted by replic2 at store.BackupRoot()).
//     Changed files are detected via mtime for incremental runs.
//  5. Sets phase → Completed (or Failed on error).
//
// Completed backups with spec.ttl set are pruned once completedAt+TTL passes.
//
// Source is split across:
//   - backup.go   — this file; package-level constants, Run, reconcile
//   - process.go  — the main per-CR workflow (process, findLatestCompletedBackup)
//   - manifests.go — manifest serialisation (backupResourceType, discoverCRDTypes, verbSupported)
//   - pvcdata.go  — agent-pod PVC copy (backupPVCData, backupSinglePVC)
//   - status.go   — CR status helpers (patchStatus, markFailed, isExpired, deleteExpired)
package backup

import (
	"context"       // for cancellation / deadlines
	"encoding/json" // to decode raw CR bytes from the dynamic client
	"fmt"           // for error wrapping
	"log"           // for structured logging
	"time"          // for the poll ticker

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // ListOptions, DeleteOptions, …
	"k8s.io/apimachinery/pkg/runtime/schema"      // GroupVersionResource

	"replic2/internal/k8s"            // Kubernetes client wrapper
	apitypes "replic2/internal/types" // Backup, Restore, phase constants, …
)

// GVR is the GroupVersionResource for the Backup CRD.
// Used by the dynamic client to list / patch / delete Backup objects.
var GVR = schema.GroupVersionResource{
	Group:    "replic2.io",
	Version:  "v1alpha1",
	Resource: "backups",
}

// coreResourceTypes is the ordered list of built-in Kubernetes resource types
// that every backup always captures.  Order matters for restore: dependencies
// (ServiceAccounts, ConfigMaps, PVCs) come before workloads (Deployments, …).
var coreResourceTypes = []schema.GroupVersionResource{
	{Group: "", Version: "v1", Resource: "serviceaccounts"},            // service identity
	{Group: "", Version: "v1", Resource: "configmaps"},                 // app configuration
	{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},     // storage claims
	{Group: "", Version: "v1", Resource: "services"},                   // network endpoints
	{Group: "apps", Version: "v1", Resource: "deployments"},            // stateless workloads
	{Group: "apps", Version: "v1", Resource: "statefulsets"},           // stateful workloads
	{Group: "apps", Version: "v1", Resource: "daemonsets"},             // per-node workloads
	{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, // HTTP routing
}

// systemGroups lists API groups that must never be included in a backup.
// These are either cluster-scoped infrastructure groups or replic2's own CRDs.
var systemGroups = map[string]bool{
	"replic2.io":                   true, // our own operator CRDs — never back up
	"apiregistration.k8s.io":       true, // API aggregation layer
	"apiextensions.k8s.io":         true, // CRD definitions themselves
	"admissionregistration.k8s.io": true, // webhooks
	"rbac.authorization.k8s.io":    true, // cluster RBAC
	"authorization.k8s.io":         true, // subject access reviews
	"authentication.k8s.io":        true, // token reviews
	"certificates.k8s.io":          true, // cert signing requests
	"coordination.k8s.io":          true, // leases (leader election)
	"events.k8s.io":                true, // ephemeral event records
	"flowcontrol.apiserver.k8s.io": true, // API priority & fairness
	"node.k8s.io":                  true, // runtime classes
	"policy":                       true, // pod disruption budgets (deprecated)
	"scheduling.k8s.io":            true, // priority classes
	"storage.k8s.io":               true, // storage classes / CSI
	"metrics.k8s.io":               true, // live metrics (not state)
}

// agentPodTimeout is the maximum time to wait for the data-copy agent pod to
// complete before we give up and mark the backup Failed.
const agentPodTimeout = 10 * time.Minute

// Run polls for Backup CRs every 5 seconds until the context is cancelled.
// It is the long-running goroutine started by main.go when this pod is leader.
func Run(ctx context.Context, c *k8s.Clients) {
	log.Println("backup controller: started") // confirm the controller is alive
	for {
		select {
		case <-ctx.Done(): // leader lost, shutting down
			log.Println("backup controller: stopped")
			return
		case <-time.After(5 * time.Second): // wait before each reconcile pass
			if err := reconcile(ctx, c); err != nil {
				// Non-fatal: log and try again next tick.
				log.Printf("backup controller: reconcile error: %v", err)
			}
		}
	}
}

// reconcile lists every Backup CR, starts any that are pending, and enforces
// TTL expiry on completed ones.  Each CR is processed in its own goroutine so
// a slow backup does not block the poll loop.
func reconcile(ctx context.Context, c *k8s.Clients) error {
	// Fetch all Backup CRs across all namespaces.
	list, err := c.Dynamic.Resource(GVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list backups: %w", err) // surface to caller for logging
	}

	for _, item := range list.Items {
		// The dynamic client returns unstructured objects; we marshal to JSON
		// first and then decode into our typed struct.
		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("backup controller: marshal item: %v", err)
			continue // skip this item and move on
		}

		var b apitypes.Backup
		if err := json.Unmarshal(raw, &b); err != nil {
			log.Printf("backup controller: decode backup: %v", err)
			continue // skip malformed CRs
		}

		// Enforce TTL for completed backups before deciding whether to skip.
		if b.Status.Phase == apitypes.PhaseCompleted {
			expired, err := isExpired(&b)
			if err != nil {
				// Bad TTL string in the spec — log but don't delete.
				log.Printf("backup controller: [%s] TTL parse error: %v", b.Name, err)
			} else if expired {
				// Delete in background; do not block the reconcile loop.
				go func(b apitypes.Backup) {
					if err := deleteExpired(ctx, c, &b); err != nil {
						log.Printf("backup controller: [%s] TTL delete error: %v", b.Name, err)
					}
				}(b)
				continue // skip further processing for this CR
			}
		}

		// Only process backups that have not started yet.
		// Phase "" (brand new) and "Pending" are both eligible.
		if b.Status.Phase != "" && b.Status.Phase != apitypes.PhasePending {
			continue // already InProgress, Completed, or Failed — leave it alone
		}

		// Spawn a goroutine per CR so each backup runs concurrently.
		go func(b apitypes.Backup) {
			if err := process(ctx, c, &b); err != nil {
				log.Printf("backup controller: [%s] failed: %v", b.Name, err)
			}
		}(b)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Exported wrappers — these thin shims expose internal functions to the test
// suite and to the restore / scheduled-backup controllers without changing the
// internal API.
// ---------------------------------------------------------------------------

// CoreResourceTypes exposes the ordered slice for the restore controller.
func CoreResourceTypes() []schema.GroupVersionResource {
	return coreResourceTypes
}

// SystemGroups exposes the exclusion map for tests.
func SystemGroups() map[string]bool {
	return systemGroups
}

// ReconcileBackups is the exported entry point used by tests.
func ReconcileBackups(ctx context.Context, c *k8s.Clients) error { return reconcile(ctx, c) }

// BackupResourceType is the exported version used in tests.
func BackupResourceType(ctx context.Context, c *k8s.Clients, ns, storagePath string, gvr schema.GroupVersionResource) error {
	return backupResourceType(ctx, c, ns, storagePath, gvr)
}

// DiscoverCRDTypes is the exported version used in tests.
func DiscoverCRDTypes(c *k8s.Clients) ([]schema.GroupVersionResource, error) {
	return discoverCRDTypes(c)
}

// FindLatestCompletedBackup is the exported version used in tests.
func FindLatestCompletedBackup(ctx context.Context, c *k8s.Clients, namespace string) (*apitypes.Backup, error) {
	return findLatestCompletedBackup(ctx, c, namespace)
}
