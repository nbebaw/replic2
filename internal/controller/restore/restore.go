// Package restore implements the Restore controller.
//
// The controller watches Restore CRs (replic2.io/v1alpha1/Restore) cluster-wide.
// When a new CR appears with phase "" or "Pending" it:
//
//  1. Sets phase → InProgress.
//  2. Locates the backup directory on the PVC:
//     a. If spec.backupName is set, reads the Backup CR for its StoragePath.
//     b. Otherwise, selects the newest sub-directory under
//     <BACKUP_ROOT>/<namespace>/ by mtime.
//  3. Ensures the target namespace exists (creates it when missing).
//  4. Applies every .yaml file in the backup directory to the cluster using
//     Server-Side Apply; falls back to Create when SSA is unavailable.
//  5. Sets phase → Completed (or Failed on error).
package restore

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

	"replic2/internal/controller/backup"
	"replic2/internal/k8s"
	"replic2/internal/store"
	apitypes "replic2/internal/types"
)

// GVR is the GroupVersionResource for the Restore CRD.
var GVR = schema.GroupVersionResource{
	Group:    "replic2.io",
	Version:  "v1alpha1",
	Resource: "restores",
}

// Run polls for Restore CRs every 5 seconds until ctx is cancelled.
func Run(ctx context.Context, c *k8s.Clients) {
	log.Println("restore controller: started")
	for {
		select {
		case <-ctx.Done():
			log.Println("restore controller: stopped")
			return
		case <-time.After(5 * time.Second):
			if err := reconcile(ctx, c); err != nil {
				log.Printf("restore controller: reconcile error: %v", err)
			}
		}
	}
}

// reconcile lists all Restore CRs and processes any that are pending.
func reconcile(ctx context.Context, c *k8s.Clients) error {
	list, err := c.Dynamic.Resource(GVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list restores: %w", err)
	}

	for _, item := range list.Items {
		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("restore controller: marshal item: %v", err)
			continue
		}

		var r apitypes.Restore
		if err := json.Unmarshal(raw, &r); err != nil {
			log.Printf("restore controller: decode restore: %v", err)
			continue
		}

		if r.Status.Phase != "" && r.Status.Phase != apitypes.PhasePending {
			continue
		}

		go func(r apitypes.Restore) {
			if err := process(ctx, c, &r); err != nil {
				log.Printf("restore controller: [%s] failed: %v", r.Name, err)
			}
		}(r)
	}
	return nil
}

// process runs the full restore workflow for one Restore CR.
func process(ctx context.Context, c *k8s.Clients, r *apitypes.Restore) error {
	ns := r.Spec.Namespace
	log.Printf("restore controller: [%s] restoring namespace %q", r.Name, ns)

	// Phase: InProgress
	now := metav1.Now()
	r.Status.Phase = apitypes.PhaseInProgress
	r.Status.StartedAt = &now
	r.Status.Message = fmt.Sprintf("restoring namespace %q", ns)
	if err := patchStatus(ctx, c, r); err != nil {
		return fmt.Errorf("set InProgress: %w", err)
	}

	backupPath, err := findBackupPath(ctx, c, r)
	if err != nil {
		return markFailed(ctx, c, r, fmt.Errorf("locate backup: %w", err))
	}

	if err := ensureNamespace(ctx, c, ns); err != nil {
		return markFailed(ctx, c, r, fmt.Errorf("ensure namespace: %w", err))
	}

	if err := applyBackupDirectory(ctx, c, backupPath, ns); err != nil {
		return markFailed(ctx, c, r, fmt.Errorf("apply resources: %w", err))
	}

	// Phase: Completed
	done := metav1.Now()
	r.Status.Phase = apitypes.PhaseCompleted
	r.Status.CompletedAt = &done
	r.Status.RestoredFrom = backupPath
	r.Status.Message = "restore complete"
	if err := patchStatus(ctx, c, r); err != nil {
		return fmt.Errorf("set Completed: %w", err)
	}

	log.Printf("restore controller: [%s] done — restored from: %s", r.Name, backupPath)
	return nil
}

// findBackupPath returns the PVC directory to restore from.
//
// If spec.backupName is set, the controller fetches that Backup CR and uses
// its StoragePath.  Otherwise it picks the newest sub-directory under
// <BACKUP_ROOT>/<namespace>/ by modification time.
func findBackupPath(ctx context.Context, c *k8s.Clients, r *apitypes.Restore) (string, error) {
	ns := r.Spec.Namespace

	if r.Spec.BackupName != "" {
		item, err := c.Dynamic.Resource(backup.GVR).Get(ctx, r.Spec.BackupName, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get Backup %q: %w", r.Spec.BackupName, err)
		}
		raw, _ := item.MarshalJSON()
		var b apitypes.Backup
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

	// Auto-select: newest sub-directory under <root>/<namespace>/.
	nsDir := filepath.Join(store.BackupRoot(), ns)
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
	// Sort newest first.
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].modTime.After(dirs[j].modTime)
	})
	return filepath.Join(nsDir, dirs[0].name), nil
}

// ensureNamespace creates the namespace if it does not already exist.
func ensureNamespace(ctx context.Context, c *k8s.Clients, ns string) error {
	_, err := c.Core.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil // already exists
	}
	if !k8serrors.IsNotFound(err) {
		return fmt.Errorf("get namespace: %w", err)
	}

	nsObj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}
	_, err = c.Core.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace: %w", err)
	}
	log.Printf("restore: created namespace %q", ns)
	return nil
}

// applyBackupDirectory walks backupPath and applies all .yaml files.
//
// Pass 1 applies the hardcoded coreResourceTypes in dependency order.
// Pass 2 applies any remaining sub-directories (third-party CRDs backed up
// via discovery).
func applyBackupDirectory(ctx context.Context, c *k8s.Clients, backupPath, ns string) error {
	coreApplied := make(map[string]bool)
	for _, gvr := range backup.CoreResourceTypes() {
		dir := filepath.Join(backupPath, gvr.Resource)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		applyDir(ctx, c, dir, &gvr, ns)
		coreApplied[gvr.Resource] = true
	}

	// Second pass: CRD directories not already handled.
	topEntries, err := os.ReadDir(backupPath)
	if err != nil {
		return fmt.Errorf("read backup dir %q: %w", backupPath, err)
	}
	for _, e := range topEntries {
		if !e.IsDir() || coreApplied[e.Name()] {
			continue
		}
		// Pass nil for gvr so applyYAMLFile derives the GVR from the YAML content.
		applyDir(ctx, c, filepath.Join(backupPath, e.Name()), nil, ns)
	}
	return nil
}

// applyDir applies all .yaml files inside dir.
// If gvr is non-nil it is used directly; otherwise the GVR is derived from
// each file's apiVersion/kind fields (used for CRD resources).
func applyDir(ctx context.Context, c *k8s.Clients, dir string, gvr *schema.GroupVersionResource, ns string) {
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
		if err := applyYAMLFile(ctx, c, path, gvr, ns); err != nil {
			log.Printf("restore: apply %s: %v", path, err)
			// Best-effort: continue with remaining files.
		}
	}
}

// applyYAMLFile reads one YAML file and applies it to the cluster via
// Server-Side Apply (create-or-update in one call).  Falls back to Create
// for older clusters that predate SSA.
func applyYAMLFile(ctx context.Context, c *k8s.Clients, path string, gvr *schema.GroupVersionResource, targetNS string) error {
	obj, err := store.ReadYAML(path)
	if err != nil {
		return fmt.Errorf("decode %q: %w", path, err)
	}

	u := &unstructured.Unstructured{Object: obj}
	u.SetNamespace(targetNS)
	u.SetResourceVersion("")
	u.SetUID("")
	u.SetCreationTimestamp(metav1.Time{})
	u.SetManagedFields(nil)
	delete(u.Object, "status")

	resolved := gvr
	if resolved == nil {
		derived, err := gvrFromObject(c, u)
		if err != nil {
			return fmt.Errorf("derive GVR for %q: %w", path, err)
		}
		resolved = &derived
	}

	raw, err := json.Marshal(u.Object)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	_, err = c.Dynamic.Resource(*resolved).Namespace(targetNS).Patch(
		ctx,
		u.GetName(),
		types.ApplyPatchType,
		raw,
		metav1.PatchOptions{FieldManager: "replic2-restore", Force: boolPtr(true)},
	)
	if err != nil {
		// SSA fallback: plain Create (idempotent via AlreadyExists check).
		_, createErr := c.Dynamic.Resource(*resolved).Namespace(targetNS).Create(ctx, u, metav1.CreateOptions{})
		if createErr != nil && !k8serrors.IsAlreadyExists(createErr) {
			return fmt.Errorf("apply (SSA + create fallback): ssa=%v, create=%v", err, createErr)
		}
	}

	log.Printf("restore: applied %s/%s", resolved.Resource, u.GetName())
	return nil
}

// gvrFromObject looks up the GVR for an Unstructured object by querying
// the discovery client using the object's apiVersion and kind.
// Used for third-party CRD objects where no static GVR is known.
func gvrFromObject(c *k8s.Clients, u *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	gvk := u.GroupVersionKind()
	mapper, err := c.Discovery.ServerPreferredResources()
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

// GVRFromObject is the exported version used in tests.
func GVRFromObject(c *k8s.Clients, u *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	return gvrFromObject(c, u)
}

// EnsureNamespace is the exported version used in tests.
func EnsureNamespace(ctx context.Context, c *k8s.Clients, ns string) error {
	return ensureNamespace(ctx, c, ns)
}

// FindBackupPath is the exported version used in tests.
func FindBackupPath(ctx context.Context, c *k8s.Clients, r *apitypes.Restore) (string, error) {
	return findBackupPath(ctx, c, r)
}

// ApplyBackupDirectory is the exported version used in tests.
func ApplyBackupDirectory(ctx context.Context, c *k8s.Clients, backupPath, ns string) error {
	return applyBackupDirectory(ctx, c, backupPath, ns)
}

// ReconcileRestores is the exported entry point for tests.
func ReconcileRestores(ctx context.Context, c *k8s.Clients) error { return reconcile(ctx, c) }

// patchStatus writes only the status sub-object back via a merge-patch on the
// /status subresource; falls back to a full Update if necessary.
func patchStatus(ctx context.Context, c *k8s.Clients, r *apitypes.Restore) error {
	statusOnly, err := json.Marshal(map[string]interface{}{"status": r.Status})
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}

	_, err = c.Dynamic.Resource(GVR).
		Patch(ctx, r.Name, types.MergePatchType, statusOnly, metav1.PatchOptions{}, "status")
	if err == nil {
		return nil
	}

	latest, getErr := c.Dynamic.Resource(GVR).Get(ctx, r.Name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("patch status (subresource): %v; re-fetch: %w", err, getErr)
	}
	raw, _ := json.Marshal(r.Status)
	var statusMap map[string]interface{}
	_ = json.Unmarshal(raw, &statusMap)
	latest.Object["status"] = statusMap
	if _, updateErr := c.Dynamic.Resource(GVR).Update(ctx, latest, metav1.UpdateOptions{}); updateErr != nil {
		return fmt.Errorf("patch status (subresource): %v; update fallback: %w", err, updateErr)
	}
	return nil
}

// markFailed sets phase → Failed and returns the original error.
func markFailed(ctx context.Context, c *k8s.Clients, r *apitypes.Restore, cause error) error {
	now := metav1.Now()
	r.Status.Phase = apitypes.PhaseFailed
	r.Status.CompletedAt = &now
	r.Status.Message = cause.Error()
	_ = patchStatus(ctx, c, r)
	return cause
}

func boolPtr(b bool) *bool { return &b }
