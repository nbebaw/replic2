package main

// backup.go — Backup controller.
//
// Watches for Backup CRs (replic2.io/v1alpha1/Backup) cluster-wide.
// When a new Backup CR is created with phase "" or "Pending", the controller:
//   1. Sets phase → InProgress.
//   2. Builds the full list of resource types to back up:
//      a. The hardcoded core/apps/networking types (resourceTypes).
//      b. Every namespace-scoped CRD discovered via the API discovery client
//         (third-party CRDs: cert-manager, Prometheus, Argo, Istio, etc.).
//   3. Serialises each resource to YAML and writes it to the PVC mount.
//      Path layout: <BACKUP_ROOT>/<namespace>/<backup-name>/<resource>/<name>.yaml
//   4. Sets phase → Completed (or Failed on error).

import (
	"bytes"
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
	"k8s.io/apimachinery/pkg/util/yaml"
	k8syaml "sigs.k8s.io/yaml"
)

// backupRoot is the mount path of the PVC inside the container.
// Override with the BACKUP_ROOT env var.
const defaultBackupRoot = "/data/backups"

func backupRoot() string {
	if v := os.Getenv("BACKUP_ROOT"); v != "" {
		return v
	}
	return defaultBackupRoot
}

// coreResourceTypes is the ordered list of well-known built-in resource types
// the backup always captures. Ordered so that dependencies (e.g. ConfigMaps
// before Deployments) are restored first.
//
// Third-party CRDs are discovered at runtime via discoverCRDTypes and appended
// after these entries.
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
	"replic2.io":                   true, // our own CRDs
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

// discoverCRDTypes uses the API discovery client to find all namespace-scoped
// CRD resource types installed in the cluster that are not in systemGroups and
// not already covered by coreResourceTypes.
//
// This automatically includes third-party CRDs such as:
//   - cert-manager:  certificates.cert-manager.io, issuers.cert-manager.io, …
//   - Prometheus:    servicemonitors.monitoring.coreos.com, prometheusrules.…, …
//   - Argo CD:       applications.argoproj.io, …
//   - Istio:         virtualservices.networking.istio.io, …
//   - Keda:          scaledobjects.keda.sh, …
//   - … and any other operator CRDs present on the cluster
func discoverCRDTypes(c *clients) ([]schema.GroupVersionResource, error) {
	// ServerPreferredNamespacedResources returns one entry per resource type
	// (preferred version only) filtered to namespace-scoped resources.
	lists, err := c.discovery.ServerPreferredNamespacedResources()
	if err != nil {
		// Partial errors are common (some API groups may be unavailable).
		// Log them but continue with whatever was returned.
		log.Printf("backup: discovery partial error (continuing): %v", err)
	}

	// Build a quick-lookup set of the core types so we don't double-back them.
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
		// Skip the built-in groups that coreResourceTypes already handles.
		if gv.Group == "" || gv.Group == "apps" || gv.Group == "networking.k8s.io" {
			continue
		}
		for _, r := range list.APIResources {
			// Skip sub-resources (they contain a "/").
			if strings.Contains(r.Name, "/") {
				continue
			}
			// Only include resources that support list (required for backup).
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

// verbSupported returns true if verb is in the verbs list.
func verbSupported(verbs metav1.Verbs, verb string) bool {
	for _, v := range verbs {
		if v == verb {
			return true
		}
	}
	return false
}

// runBackupController polls for Backup CRs in a loop.
// It blocks until ctx is cancelled.
func runBackupController(ctx context.Context, c *clients) {
	log.Println("backup controller: started")
	for {
		select {
		case <-ctx.Done():
			log.Println("backup controller: stopped")
			return
		case <-time.After(5 * time.Second):
			if err := reconcileBackups(ctx, c); err != nil {
				log.Printf("backup controller: reconcile error: %v", err)
			}
		}
	}
}

// reconcileBackups lists all Backup CRs, starts any that are pending, and
// enforces TTL expiry on completed ones.
func reconcileBackups(ctx context.Context, c *clients) error {
	gvr := schema.GroupVersionResource{
		Group:    "replic2.io",
		Version:  "v1alpha1",
		Resource: "backups",
	}

	list, err := c.dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list backups: %w", err)
	}

	for _, item := range list.Items {
		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("backup controller: marshal item: %v", err)
			continue
		}

		var b Backup
		if err := json.Unmarshal(raw, &b); err != nil {
			log.Printf("backup controller: decode backup: %v", err)
			continue
		}

		// Enforce TTL for completed backups before deciding whether to skip.
		if b.Status.Phase == "Completed" {
			if expired, err := backupExpired(&b); err != nil {
				log.Printf("backup controller: [%s] TTL parse error: %v", b.Name, err)
			} else if expired {
				go func(b Backup) {
					if err := deleteExpiredBackup(ctx, c, gvr, &b); err != nil {
						log.Printf("backup controller: [%s] TTL delete error: %v", b.Name, err)
					}
				}(b)
				continue
			}
		}

		// Only process backups that have not started yet.
		if b.Status.Phase != "" && b.Status.Phase != "Pending" {
			continue
		}

		go func(b Backup) {
			if err := processBackup(ctx, c, &b); err != nil {
				log.Printf("backup controller: [%s] failed: %v", b.Name, err)
			}
		}(b)
	}
	return nil
}

// backupExpired returns true when spec.ttl is set and the backup has been
// completed long enough ago that completedAt + ttl ≤ now.
// Returns (false, nil) when no TTL is set or the backup has not completed yet.
func backupExpired(b *Backup) (bool, error) {
	if b.Spec.TTL == "" {
		return false, nil
	}
	if b.Status.CompletedAt == nil {
		return false, nil
	}
	ttl, err := time.ParseDuration(b.Spec.TTL)
	if err != nil {
		return false, fmt.Errorf("parse TTL %q: %w", b.Spec.TTL, err)
	}
	expiry := b.Status.CompletedAt.Time.Add(ttl)
	return !time.Now().Before(expiry), nil
}

// deleteExpiredBackup removes the PVC data directory and the Backup CR itself.
func deleteExpiredBackup(ctx context.Context, c *clients, gvr schema.GroupVersionResource, b *Backup) error {
	log.Printf("backup controller: [%s] TTL expired — deleting backup", b.Name)

	// 1. Remove data from the PVC (best-effort: log and continue if missing).
	if b.Status.StoragePath != "" {
		if err := os.RemoveAll(b.Status.StoragePath); err != nil && !os.IsNotExist(err) {
			log.Printf("backup controller: [%s] remove storage path %q: %v", b.Name, b.Status.StoragePath, err)
		} else {
			log.Printf("backup controller: [%s] removed storage path %q", b.Name, b.Status.StoragePath)
		}
	}

	// 2. Delete the Backup CR.
	if err := c.dynamic.Resource(gvr).Delete(ctx, b.Name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete Backup CR %q: %w", b.Name, err)
	}
	log.Printf("backup controller: [%s] deleted (TTL expired)", b.Name)
	return nil
}

// processBackup performs the full backup workflow for a single Backup CR.
func processBackup(ctx context.Context, c *clients, b *Backup) error {
	gvr := schema.GroupVersionResource{
		Group:    "replic2.io",
		Version:  "v1alpha1",
		Resource: "backups",
	}

	ns := b.Spec.Namespace
	log.Printf("backup controller: [%s] backing up namespace %q", b.Name, ns)

	// ---- Phase: InProgress ----
	now := metav1.Now()
	b.Status.Phase = "InProgress"
	b.Status.StartedAt = &now
	b.Status.Message = fmt.Sprintf("backing up namespace %q", ns)
	if err := patchBackupStatus(ctx, c, gvr, b); err != nil {
		return fmt.Errorf("set InProgress: %w", err)
	}

	// ---- Perform the backup ----
	storagePath := filepath.Join(backupRoot(), ns, b.Name)
	if err := os.MkdirAll(storagePath, 0o755); err != nil {
		return markBackupFailed(ctx, c, gvr, b, fmt.Errorf("mkdir %q: %w", storagePath, err))
	}

	// Build the full list: hardcoded core types first, then any CRDs
	// discovered at runtime (third-party operators, etc.).
	allTypes := append([]schema.GroupVersionResource(nil), coreResourceTypes...)
	crdTypes, err := discoverCRDTypes(c)
	if err != nil {
		log.Printf("backup controller: [%s] CRD discovery error (continuing with core types only): %v", b.Name, err)
	} else if len(crdTypes) > 0 {
		log.Printf("backup controller: [%s] discovered %d additional CRD types", b.Name, len(crdTypes))
		allTypes = append(allTypes, crdTypes...)
	}

	for _, gvRes := range allTypes {
		if err := backupResourceType(ctx, c, ns, storagePath, gvRes); err != nil {
			// Log and continue — a missing API (e.g. Ingress on a cluster without
			// the networking API) should not abort the whole backup.
			log.Printf("backup controller: [%s] skip %s: %v", b.Name, gvRes.Resource, err)
		}
	}

	// ---- Phase: Completed ----
	done := metav1.Now()
	b.Status.Phase = "Completed"
	b.Status.CompletedAt = &done
	b.Status.StoragePath = storagePath
	b.Status.Message = fmt.Sprintf("backup complete — %d resource types captured", len(allTypes))
	if err := patchBackupStatus(ctx, c, gvr, b); err != nil {
		return fmt.Errorf("set Completed: %w", err)
	}

	log.Printf("backup controller: [%s] done — path: %s", b.Name, storagePath)
	return nil
}

// backupResourceType lists all resources of gvRes in ns and writes each to a
// YAML file under storagePath/<Resource>/<name>.yaml.
func backupResourceType(ctx context.Context, c *clients, ns, storagePath string, gvRes schema.GroupVersionResource) error {
	list, err := c.dynamic.Resource(gvRes).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	dir := filepath.Join(storagePath, gvRes.Resource)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}

	for _, item := range list.Items {
		// Strip managed fields, status, and runtime metadata that should not
		// be re-applied verbatim.
		item.SetManagedFields(nil)
		item.SetResourceVersion("")
		item.SetUID("")
		item.SetCreationTimestamp(metav1.Time{})
		item.SetGeneration(0)

		// Remove status sub-object so the restored resource starts fresh.
		delete(item.Object, "status")

		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("backup: marshal %s/%s: %v", gvRes.Resource, item.GetName(), err)
			continue
		}

		// Convert JSON → YAML for human-readability.
		yamlBytes, err := jsonToYAML(raw)
		if err != nil {
			log.Printf("backup: json→yaml %s/%s: %v", gvRes.Resource, item.GetName(), err)
			continue
		}

		filename := filepath.Join(dir, item.GetName()+".yaml")
		if err := os.WriteFile(filename, yamlBytes, 0o644); err != nil {
			log.Printf("backup: write %s: %v", filename, err)
		}
	}
	return nil
}

// patchBackupStatus writes the updated status back to the API server via the
// /status subresource using the dynamic client.
func patchBackupStatus(ctx context.Context, c *clients, gvr schema.GroupVersionResource, b *Backup) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("marshal backup: %w", err)
	}

	_, err = c.dynamic.Resource(gvr).
		Patch(ctx, b.Name, "application/merge-patch+json", raw, metav1.PatchOptions{}, "status")
	if err != nil {
		// Subresource patch may fail if the CRD has no /status sub-resource yet;
		// fall back to a full update.
		u := b.unstructured()
		_, err = c.dynamic.Resource(gvr).Update(ctx, &u, metav1.UpdateOptions{})
	}
	return err
}

// markBackupFailed sets phase → Failed and returns the original error.
func markBackupFailed(ctx context.Context, c *clients, gvr schema.GroupVersionResource, b *Backup, cause error) error {
	now := metav1.Now()
	b.Status.Phase = "Failed"
	b.Status.CompletedAt = &now
	b.Status.Message = cause.Error()
	_ = patchBackupStatus(ctx, c, gvr, b)
	return cause
}

// unstructured converts a typed Backup back to an Unstructured so it can be
// passed to the dynamic client's Update method.
func (b *Backup) unstructured() unstructuredObj {
	raw, _ := json.Marshal(b)
	var u unstructuredObj
	_ = json.Unmarshal(raw, &u.Object)
	return u
}

// jsonToYAML converts a JSON byte slice to YAML using the sigs.k8s.io/yaml
// library (which is already a transitive dep of client-go).
func jsonToYAML(j []byte) ([]byte, error) {
	return k8syaml.JSONToYAML(j)
}

// yamlReader is a helper used by the restore controller but defined here to
// keep YAML utilities together.
func decodeYAMLFile(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// sigs.k8s.io/yaml parses YAML into JSON-compatible maps.
	var obj map[string]interface{}
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	return obj, nil
}
