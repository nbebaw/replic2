package main

// restore.go — Restore controller.
//
// Watches for Restore CRs (replic2.io/v1alpha1/Restore) cluster-wide.
// When a new Restore CR appears with phase "" or "Pending", the controller:
//   1. Sets phase → InProgress.
//   2. Finds the backup on the PVC:
//      a. If spec.backupName is set, use <BACKUP_ROOT>/<namespace>/<backupName>/
//      b. Otherwise, pick the most recent sub-directory under
//         <BACKUP_ROOT>/<namespace>/ (by directory mtime).
//   3. Ensures the target namespace exists (creates it if deleted).
//   4. Walks the backup directory tree and applies every .yaml file using the
//      dynamic client (Server-Side Apply via Patch, then falls back to Create).
//   5. Sets phase → Completed (or Failed on error).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// runRestoreController polls for Restore CRs in a loop.
// It blocks until ctx is cancelled.
func runRestoreController(ctx context.Context, c *clients) {
	log.Println("restore controller: started")
	for {
		select {
		case <-ctx.Done():
			log.Println("restore controller: stopped")
			return
		case <-time.After(5 * time.Second):
			if err := reconcileRestores(ctx, c); err != nil {
				log.Printf("restore controller: reconcile error: %v", err)
			}
		}
	}
}

// reconcileRestores lists all Restore CRs and processes any that are pending.
func reconcileRestores(ctx context.Context, c *clients) error {
	gvr := schema.GroupVersionResource{
		Group:    "replic2.io",
		Version:  "v1alpha1",
		Resource: "restores",
	}

	list, err := c.dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list restores: %w", err)
	}

	for _, item := range list.Items {
		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("restore controller: marshal item: %v", err)
			continue
		}

		var r Restore
		if err := json.Unmarshal(raw, &r); err != nil {
			log.Printf("restore controller: decode restore: %v", err)
			continue
		}

		if r.Status.Phase != "" && r.Status.Phase != "Pending" {
			continue
		}

		go func(r Restore) {
			if err := processRestore(ctx, c, &r); err != nil {
				log.Printf("restore controller: [%s] failed: %v", r.Name, err)
			}
		}(r)
	}
	return nil
}

// processRestore performs the full restore workflow for a single Restore CR.
func processRestore(ctx context.Context, c *clients, r *Restore) error {
	gvr := schema.GroupVersionResource{
		Group:    "replic2.io",
		Version:  "v1alpha1",
		Resource: "restores",
	}

	ns := r.Spec.Namespace
	log.Printf("restore controller: [%s] restoring namespace %q", r.Name, ns)

	// ---- Phase: InProgress ----
	now := metav1.Now()
	r.Status.Phase = "InProgress"
	r.Status.StartedAt = &now
	r.Status.Message = fmt.Sprintf("restoring namespace %q", ns)
	if err := patchRestoreStatus(ctx, c, gvr, r); err != nil {
		return fmt.Errorf("set InProgress: %w", err)
	}

	// ---- Locate the backup on the PVC ----
	backupPath, err := findBackupPath(ctx, c, r)
	if err != nil {
		return markRestoreFailed(ctx, c, gvr, r, fmt.Errorf("locate backup: %w", err))
	}

	// ---- Ensure the namespace exists ----
	if err := ensureNamespace(ctx, c, ns); err != nil {
		return markRestoreFailed(ctx, c, gvr, r, fmt.Errorf("ensure namespace: %w", err))
	}

	// ---- Apply all YAML files from the backup directory ----
	if err := applyBackupDirectory(ctx, c, backupPath, ns); err != nil {
		return markRestoreFailed(ctx, c, gvr, r, fmt.Errorf("apply resources: %w", err))
	}

	// ---- Phase: Completed ----
	done := metav1.Now()
	r.Status.Phase = "Completed"
	r.Status.CompletedAt = &done
	r.Status.RestoredFrom = backupPath
	r.Status.Message = "restore complete"
	if err := patchRestoreStatus(ctx, c, gvr, r); err != nil {
		return fmt.Errorf("set Completed: %w", err)
	}

	log.Printf("restore controller: [%s] done — restored from: %s", r.Name, backupPath)
	return nil
}

// findBackupPath returns the directory on the PVC to restore from.
// If spec.backupName is set it uses that specific backup.
// Otherwise it picks the most recently modified backup directory for the
// target namespace.
func findBackupPath(ctx context.Context, c *clients, r *Restore) (string, error) {
	ns := r.Spec.Namespace

	if r.Spec.BackupName != "" {
		// Explicit backup name: look it up via the Backup CR to get its
		// StoragePath, then verify the directory exists.
		backupGVR := schema.GroupVersionResource{
			Group:    "replic2.io",
			Version:  "v1alpha1",
			Resource: "backups",
		}
		item, err := c.dynamic.Resource(backupGVR).Get(ctx, r.Spec.BackupName, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get Backup %q: %w", r.Spec.BackupName, err)
		}
		raw, _ := item.MarshalJSON()
		var b Backup
		if err := json.Unmarshal(raw, &b); err != nil {
			return "", fmt.Errorf("decode Backup: %w", err)
		}
		if b.Status.StoragePath == "" {
			return "", fmt.Errorf("Backup %q has no storage path (phase: %s)", r.Spec.BackupName, b.Status.Phase)
		}
		if _, err := os.Stat(b.Status.StoragePath); err != nil {
			return "", fmt.Errorf("storage path %q not found on PVC: %w", b.Status.StoragePath, err)
		}
		return b.Status.StoragePath, nil
	}

	// Auto-select: find the newest sub-directory under <root>/<namespace>/.
	nsDir := filepath.Join(backupRoot(), ns)
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		return "", fmt.Errorf("no backups found for namespace %q (dir %q): %w", ns, nsDir, err)
	}

	type dirEntry struct {
		name    string
		modTime time.Time
	}
	var dirs []dirEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, dirEntry{name: e.Name(), modTime: info.ModTime()})
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("no backup directories found under %q", nsDir)
	}
	// Sort descending by mtime — newest first.
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].modTime.After(dirs[j].modTime)
	})
	return filepath.Join(nsDir, dirs[0].name), nil
}

// ensureNamespace creates the namespace if it does not already exist.
func ensureNamespace(ctx context.Context, c *clients, ns string) error {
	_, err := c.core.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil // already exists
	}
	if !k8serrors.IsNotFound(err) {
		return fmt.Errorf("get namespace: %w", err)
	}

	nsObj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}
	_, err = c.core.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace: %w", err)
	}
	log.Printf("restore: created namespace %q", ns)
	return nil
}

// applyBackupDirectory walks backupPath and applies all .yaml files it finds.
//
// Strategy:
//  1. Apply the hardcoded coreResourceTypes first (preserves dependency order:
//     ServiceAccounts → ConfigMaps → PVCs → Services → workloads).
//  2. Walk every remaining subdirectory in backupPath (third-party CRDs that
//     were discovered at backup time and are not in coreResourceTypes).  For
//     these the GVR is derived directly from the apiVersion/kind fields inside
//     each saved YAML file, so no static registry is needed.
func applyBackupDirectory(ctx context.Context, c *clients, backupPath, ns string) error {
	// Pass 1 — core types in dependency order.
	coreApplied := make(map[string]bool) // resource name → applied
	for _, gvRes := range coreResourceTypes {
		dir := filepath.Join(backupPath, gvRes.Resource)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		applyDir(ctx, c, dir, &gvRes, ns)
		coreApplied[gvRes.Resource] = true
	}

	// Pass 2 — any subdirectories not already handled (third-party CRDs).
	topEntries, err := os.ReadDir(backupPath)
	if err != nil {
		return fmt.Errorf("read backup dir %q: %w", backupPath, err)
	}
	for _, e := range topEntries {
		if !e.IsDir() || coreApplied[e.Name()] {
			continue
		}
		dir := filepath.Join(backupPath, e.Name())
		// Pass nil for gvRes — applyDir will derive it from each file's YAML.
		applyDir(ctx, c, dir, nil, ns)
	}
	return nil
}

// applyDir applies all .yaml files inside dir.
// If gvRes is non-nil it is used directly; otherwise the GVR is derived from
// the apiVersion/kind fields inside each YAML file (used for CRD resources).
func applyDir(ctx context.Context, c *clients, dir string, gvRes *schema.GroupVersionResource, ns string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("restore: read dir %q: %v", dir, err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := applyYAMLFile(ctx, c, path, gvRes, ns); err != nil {
			log.Printf("restore: apply %s: %v", path, err)
			// Continue — best-effort restore.
		}
	}
}

// applyYAMLFile reads a single YAML file and applies it to the cluster.
// It uses Server-Side Apply (SSA) via a PATCH request so that the resource
// can be both created (if missing) and updated (if it already exists) in one
// call.  Falls back to plain Create on error.
//
// gvRes may be nil; in that case the GVR is derived from the apiVersion/kind
// fields embedded in the YAML (used for third-party CRDs).
func applyYAMLFile(ctx context.Context, c *clients, path string, gvRes *schema.GroupVersionResource, targetNS string) error {
	obj, err := decodeYAMLFile(path)
	if err != nil {
		return fmt.Errorf("decode %q: %w", path, err)
	}

	u := &unstructured.Unstructured{Object: obj}
	// Force the target namespace.
	u.SetNamespace(targetNS)
	// Clear cluster-assigned fields.
	u.SetResourceVersion("")
	u.SetUID("")
	u.SetCreationTimestamp(metav1.Time{})
	u.SetManagedFields(nil)
	delete(u.Object, "status")

	// Resolve the GVR: use the provided value or derive it from the object.
	resolved := gvRes
	if resolved == nil {
		derived, err := gvrFromUnstructured(c, u)
		if err != nil {
			return fmt.Errorf("derive GVR for %q: %w", path, err)
		}
		resolved = &derived
	}

	raw, err := json.Marshal(u.Object)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// Server-Side Apply: create-or-update in one call.
	_, err = c.dynamic.Resource(*resolved).Namespace(targetNS).Patch(
		ctx,
		u.GetName(),
		types.ApplyPatchType,
		raw,
		metav1.PatchOptions{FieldManager: "replic2-restore", Force: boolPtr(true)},
	)
	if err != nil {
		// SSA is available since k8s 1.18; fall back to Create for older clusters.
		_, createErr := c.dynamic.Resource(*resolved).Namespace(targetNS).Create(ctx, u, metav1.CreateOptions{})
		if createErr != nil && !k8serrors.IsAlreadyExists(createErr) {
			return fmt.Errorf("apply (SSA + create fallback): ssa=%v, create=%v", err, createErr)
		}
	}

	log.Printf("restore: applied %s/%s", resolved.Resource, u.GetName())
	return nil
}

// gvrFromUnstructured looks up the GVR for an unstructured object by querying
// the API discovery client using the object's apiVersion and kind fields.
// This is used for third-party CRD resources where no static GVR is known.
func gvrFromUnstructured(c *clients, u *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	gvk := u.GroupVersionKind()
	mapper, err := c.discovery.ServerPreferredResources()
	if err != nil && mapper == nil {
		return schema.GroupVersionResource{}, fmt.Errorf("discovery: %w", err)
	}
	for _, list := range mapper {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		if gv.Group != gvk.Group || gv.Version != gvk.Version {
			continue
		}
		for _, r := range list.APIResources {
			if r.Kind == gvk.Kind && !strings.Contains(r.Name, "/") {
				return schema.GroupVersionResource{
					Group:    gv.Group,
					Version:  gv.Version,
					Resource: r.Name,
				}, nil
			}
		}
	}
	return schema.GroupVersionResource{}, fmt.Errorf("no resource found for GVK %s", gvk)
}

// patchRestoreStatus writes the updated status back via the dynamic client.
func patchRestoreStatus(ctx context.Context, c *clients, gvr schema.GroupVersionResource, r *Restore) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal restore: %w", err)
	}

	_, err = c.dynamic.Resource(gvr).
		Patch(ctx, r.Name, "application/merge-patch+json", raw, metav1.PatchOptions{}, "status")
	if err != nil {
		u := unstructured.Unstructured{}
		_ = json.Unmarshal(raw, &u.Object)
		_, err = c.dynamic.Resource(gvr).Update(ctx, &u, metav1.UpdateOptions{})
	}
	return err
}

// markRestoreFailed sets phase → Failed and returns the original error.
func markRestoreFailed(ctx context.Context, c *clients, gvr schema.GroupVersionResource, r *Restore, cause error) error {
	now := metav1.Now()
	r.Status.Phase = "Failed"
	r.Status.CompletedAt = &now
	r.Status.Message = cause.Error()
	_ = patchRestoreStatus(ctx, c, gvr, r)
	return cause
}

// boolPtr returns a pointer to a bool — required by the Kubernetes API types.
func boolPtr(b bool) *bool { return &b }
