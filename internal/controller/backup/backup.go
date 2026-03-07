// Package backup implements the Backup controller.
//
// The controller watches Backup CRs (replic2.io/v1alpha1/Backup) cluster-wide.
// When a new CR appears with phase "" or "Pending" it:
//
//  1. Sets phase → InProgress.
//  2. Discovers all resource types to capture:
//     a. The hardcoded coreResourceTypes (dependency-ordered).
//     b. Every namespace-scoped CRD found via API discovery (third-party
//     operators: cert-manager, Prometheus, Argo CD, Istio, …).
//  3. Lists each resource type in the target namespace and writes a YAML file
//     per object under <BACKUP_ROOT>/<namespace>/<backup-name>/<resource>/.
//  4. Sets phase → Completed (or Failed on error).
//
// Completed backups with spec.ttl set are pruned once completedAt+TTL passes.
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"replic2/internal/k8s"
	"replic2/internal/store"
	apitypes "replic2/internal/types"
)

// GVR is the GroupVersionResource for the Backup CRD.
var GVR = schema.GroupVersionResource{
	Group:    "replic2.io",
	Version:  "v1alpha1",
	Resource: "backups",
}

// coreResourceTypes is the ordered list of built-in resource types the backup
// always captures.  Order matters for restore: dependencies come first.
var coreResourceTypes = []schema.GroupVersionResource{
	{Group: "", Version: "v1", Resource: "serviceaccounts"},
	{Group: "", Version: "v1", Resource: "configmaps"},
	{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
	{Group: "", Version: "v1", Resource: "services"},
	{Group: "apps", Version: "v1", Resource: "deployments"},
	{Group: "apps", Version: "v1", Resource: "statefulsets"},
	{Group: "apps", Version: "v1", Resource: "daemonsets"},
	{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
}

// systemGroups lists API groups that must never be included in a backup.
// These are either cluster-scoped infrastructure or replic2's own CRDs.
var systemGroups = map[string]bool{
	"replic2.io":                   true,
	"apiregistration.k8s.io":       true,
	"apiextensions.k8s.io":         true,
	"admissionregistration.k8s.io": true,
	"rbac.authorization.k8s.io":    true,
	"authorization.k8s.io":         true,
	"authentication.k8s.io":        true,
	"certificates.k8s.io":          true,
	"coordination.k8s.io":          true,
	"events.k8s.io":                true,
	"flowcontrol.apiserver.k8s.io": true,
	"node.k8s.io":                  true,
	"policy":                       true,
	"scheduling.k8s.io":            true,
	"storage.k8s.io":               true,
	"metrics.k8s.io":               true,
}

// Run polls for Backup CRs every 5 seconds until ctx is cancelled.
func Run(ctx context.Context, c *k8s.Clients) {
	log.Println("backup controller: started")
	for {
		select {
		case <-ctx.Done():
			log.Println("backup controller: stopped")
			return
		case <-time.After(5 * time.Second):
			if err := reconcile(ctx, c); err != nil {
				log.Printf("backup controller: reconcile error: %v", err)
			}
		}
	}
}

// reconcile lists all Backup CRs, starts any that are pending, and
// enforces TTL expiry on completed ones.
func reconcile(ctx context.Context, c *k8s.Clients) error {
	list, err := c.Dynamic.Resource(GVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list backups: %w", err)
	}

	for _, item := range list.Items {
		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("backup controller: marshal item: %v", err)
			continue
		}

		var b apitypes.Backup
		if err := json.Unmarshal(raw, &b); err != nil {
			log.Printf("backup controller: decode backup: %v", err)
			continue
		}

		// Enforce TTL for completed backups before deciding whether to skip.
		if b.Status.Phase == apitypes.PhaseCompleted {
			expired, err := isExpired(&b)
			if err != nil {
				log.Printf("backup controller: [%s] TTL parse error: %v", b.Name, err)
			} else if expired {
				go func(b apitypes.Backup) {
					if err := deleteExpired(ctx, c, &b); err != nil {
						log.Printf("backup controller: [%s] TTL delete error: %v", b.Name, err)
					}
				}(b)
				continue
			}
		}

		// Only process backups that have not started yet.
		if b.Status.Phase != "" && b.Status.Phase != apitypes.PhasePending {
			continue
		}

		go func(b apitypes.Backup) {
			if err := process(ctx, c, &b); err != nil {
				log.Printf("backup controller: [%s] failed: %v", b.Name, err)
			}
		}(b)
	}
	return nil
}

// isExpired returns true when spec.ttl is set and completedAt+ttl ≤ now.
func isExpired(b *apitypes.Backup) (bool, error) {
	if b.Spec.TTL == "" || b.Status.CompletedAt == nil {
		return false, nil
	}
	ttl, err := time.ParseDuration(b.Spec.TTL)
	if err != nil {
		return false, fmt.Errorf("parse TTL %q: %w", b.Spec.TTL, err)
	}
	expiry := b.Status.CompletedAt.Time.Add(ttl)
	return !time.Now().Before(expiry), nil
}

// IsExpired is the exported version used by the scheduled backup controller.
func IsExpired(b *apitypes.Backup) (bool, error) { return isExpired(b) }

// DeleteExpired removes the PVC data directory and the Backup CR.
// Exported so the scheduled backup controller can call it during keepLast pruning.
func DeleteExpired(ctx context.Context, c *k8s.Clients, b *apitypes.Backup) error {
	return deleteExpired(ctx, c, b)
}

func deleteExpired(ctx context.Context, c *k8s.Clients, b *apitypes.Backup) error {
	log.Printf("backup controller: [%s] TTL expired — deleting", b.Name)

	// 1. Remove data from the PVC (best-effort).
	if b.Status.StoragePath != "" {
		if err := os.RemoveAll(b.Status.StoragePath); err != nil && !os.IsNotExist(err) {
			log.Printf("backup controller: [%s] remove %q: %v", b.Name, b.Status.StoragePath, err)
		}
	}

	// 2. Delete the Backup CR.
	if err := c.Dynamic.Resource(GVR).Delete(ctx, b.Name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete Backup CR %q: %w", b.Name, err)
	}
	log.Printf("backup controller: [%s] deleted (TTL expired)", b.Name)
	return nil
}

// process runs the full backup workflow for one Backup CR.
func process(ctx context.Context, c *k8s.Clients, b *apitypes.Backup) error {
	ns := b.Spec.Namespace
	log.Printf("backup controller: [%s] backing up namespace %q", b.Name, ns)

	// Phase: InProgress
	now := metav1.Now()
	b.Status.Phase = apitypes.PhaseInProgress
	b.Status.StartedAt = &now
	b.Status.Message = fmt.Sprintf("backing up namespace %q", ns)
	if err := patchStatus(ctx, c, b); err != nil {
		return fmt.Errorf("set InProgress: %w", err)
	}

	storagePath := filepath.Join(store.BackupRoot(), ns, b.Name)
	if err := os.MkdirAll(storagePath, 0o755); err != nil {
		return markFailed(ctx, c, b, fmt.Errorf("mkdir %q: %w", storagePath, err))
	}

	// Build the full resource-type list: hardcoded core types first, then
	// any third-party CRDs discovered at runtime.
	allTypes := append([]schema.GroupVersionResource(nil), coreResourceTypes...)
	crdTypes, err := discoverCRDTypes(c)
	if err != nil {
		log.Printf("backup controller: [%s] CRD discovery error (continuing with core types only): %v", b.Name, err)
	} else if len(crdTypes) > 0 {
		log.Printf("backup controller: [%s] discovered %d additional CRD types", b.Name, len(crdTypes))
		allTypes = append(allTypes, crdTypes...)
	}

	for _, gvr := range allTypes {
		if err := backupResourceType(ctx, c, ns, storagePath, gvr); err != nil {
			// Log and continue — a missing API should not abort the whole backup.
			log.Printf("backup controller: [%s] skip %s: %v", b.Name, gvr.Resource, err)
		}
	}

	// Phase: Completed
	done := metav1.Now()
	b.Status.Phase = apitypes.PhaseCompleted
	b.Status.CompletedAt = &done
	b.Status.StoragePath = storagePath
	b.Status.Message = fmt.Sprintf("backup complete — %d resource types captured", len(allTypes))
	if err := patchStatus(ctx, c, b); err != nil {
		return fmt.Errorf("set Completed: %w", err)
	}

	log.Printf("backup controller: [%s] done — path: %s", b.Name, storagePath)
	return nil
}

// backupResourceType lists all resources of the given GVR in namespace ns
// and writes one YAML file per object under storagePath/<resource>/<name>.yaml.
func backupResourceType(ctx context.Context, c *k8s.Clients, ns, storagePath string, gvr schema.GroupVersionResource) error {
	list, err := c.Dynamic.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	dir := filepath.Join(storagePath, gvr.Resource)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}

	for _, item := range list.Items {
		// Strip fields that must not be re-applied verbatim.
		item.SetManagedFields(nil)
		item.SetResourceVersion("")
		item.SetUID("")
		item.SetCreationTimestamp(metav1.Time{})
		item.SetGeneration(0)
		delete(item.Object, "status")

		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("backup: marshal %s/%s: %v", gvr.Resource, item.GetName(), err)
			continue
		}

		yamlBytes, err := store.JSONToYAML(raw)
		if err != nil {
			log.Printf("backup: json→yaml %s/%s: %v", gvr.Resource, item.GetName(), err)
			continue
		}

		filename := filepath.Join(dir, item.GetName()+".yaml")
		if err := os.WriteFile(filename, yamlBytes, 0o644); err != nil {
			log.Printf("backup: write %s: %v", filename, err)
		}
	}
	return nil
}

// discoverCRDTypes uses the API discovery client to find all namespace-scoped
// CRD resource types that are not in systemGroups and not already in
// coreResourceTypes.  This automatically picks up third-party CRDs such as
// cert-manager, Prometheus operator, Argo CD, Istio, KEDA, and so on.
func discoverCRDTypes(c *k8s.Clients) ([]schema.GroupVersionResource, error) {
	lists, err := c.Discovery.ServerPreferredNamespacedResources()
	if err != nil {
		// Partial errors are common when some API groups are unavailable.
		log.Printf("backup: discovery partial error (continuing): %v", err)
	}

	coreSet := make(map[schema.GroupVersionResource]bool, len(coreResourceTypes))
	for _, gvr := range coreResourceTypes {
		coreSet[gvr] = true
	}

	var extra []schema.GroupVersionResource
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		if systemGroups[gv.Group] {
			continue
		}
		// Skip the built-in groups already handled by coreResourceTypes.
		if gv.Group == "" || gv.Group == "apps" || gv.Group == "networking.k8s.io" {
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") { // skip sub-resources
				continue
			}
			if !verbSupported(r.Verbs, "list") {
				continue
			}
			gvr := schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: r.Name,
			}
			if !coreSet[gvr] {
				extra = append(extra, gvr)
			}
		}
	}
	return extra, nil
}

// verbSupported returns true if the given verb appears in the verbs list.
func verbSupported(verbs metav1.Verbs, verb string) bool {
	for _, v := range verbs {
		if v == verb {
			return true
		}
	}
	return false
}

// VerbSupported is the exported version used in tests.
func VerbSupported(verbs metav1.Verbs, verb string) bool { return verbSupported(verbs, verb) }

// patchStatus writes only the status sub-object back to the API server.
//
// Strategy: merge-patch on the /status subresource (avoids resourceVersion
// conflicts).  Falls back to a full Update if the subresource is not available.
func patchStatus(ctx context.Context, c *k8s.Clients, b *apitypes.Backup) error {
	statusOnly, err := json.Marshal(map[string]interface{}{"status": b.Status})
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}

	_, err = c.Dynamic.Resource(GVR).
		Patch(ctx, b.Name, types.MergePatchType, statusOnly, metav1.PatchOptions{}, "status")
	if err == nil {
		return nil
	}

	// Fallback: re-fetch the latest version, overwrite status, then Update.
	latest, getErr := c.Dynamic.Resource(GVR).Get(ctx, b.Name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("patch status (subresource): %v; re-fetch: %w", err, getErr)
	}
	raw, _ := json.Marshal(b.Status)
	var statusMap map[string]interface{}
	_ = json.Unmarshal(raw, &statusMap)
	latest.Object["status"] = statusMap
	if _, updateErr := c.Dynamic.Resource(GVR).Update(ctx, latest, metav1.UpdateOptions{}); updateErr != nil {
		return fmt.Errorf("patch status (subresource): %v; update fallback: %w", err, updateErr)
	}
	return nil
}

// markFailed sets phase → Failed on the Backup CR and returns the original error.
func markFailed(ctx context.Context, c *k8s.Clients, b *apitypes.Backup, cause error) error {
	now := metav1.Now()
	b.Status.Phase = apitypes.PhaseFailed
	b.Status.CompletedAt = &now
	b.Status.Message = cause.Error()
	_ = patchStatus(ctx, c, b)
	return cause
}

// CoreResourceTypes exposes the ordered slice for the restore controller.
func CoreResourceTypes() []schema.GroupVersionResource {
	return coreResourceTypes
}

// SystemGroups exposes the exclusion map for tests.
func SystemGroups() map[string]bool {
	return systemGroups
}

// ReconcileBackups is the exported entry point for tests.
func ReconcileBackups(ctx context.Context, c *k8s.Clients) error { return reconcile(ctx, c) }

// BackupResourceType is the exported version used in tests.
func BackupResourceType(ctx context.Context, c *k8s.Clients, ns, storagePath string, gvr schema.GroupVersionResource) error {
	return backupResourceType(ctx, c, ns, storagePath, gvr)
}

// DiscoverCRDTypes is the exported version used in tests.
func DiscoverCRDTypes(c *k8s.Clients) ([]schema.GroupVersionResource, error) {
	return discoverCRDTypes(c)
}
