// Package restore implements the Restore controller.
//
// The controller watches Restore CRs (replic2.io/v1alpha1/Restore) cluster-wide.
// When a new CR appears with phase "" or "Pending" it:
//
//  1. Sets phase → InProgress.
//  2. Locates the backup S3 key prefix:
//     a. If spec.backupName is set, reads the Backup CR for its StoragePath.
//     b. Otherwise, lists all Backup CRs for the namespace and picks the most
//     recently completed one.
//  3. Ensures the target namespace exists (creates it when missing).
//  4. Applies every manifest stored under <keyPrefix>/ in S3 to the cluster
//     using Server-Side Apply; falls back to Create when SSA is unavailable.
//  5. Sets phase → Completed (or Failed on error).
package restore

import (
	"context"       // for cancellation / deadlines
	"encoding/json" // to decode unstructured CR bytes from the dynamic client
	"fmt"           // for error wrapping
	"log"           // for structured logging
	"os"            // for reading S3 env vars to pass to the restore agent pod
	"strings"       // for Contains (skip sub-resources like "pods/log")
	"time"          // for the poll ticker and agent pod timeout

	corev1 "k8s.io/api/core/v1"                         // Namespace, Pod, EnvVar types
	k8serrors "k8s.io/apimachinery/pkg/api/errors"      // IsNotFound, IsAlreadyExists
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"       // Now(), ListOptions, GetOptions
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured" // Unstructured (dynamic objects)
	"k8s.io/apimachinery/pkg/runtime/schema"            // GroupVersionResource
	"k8s.io/apimachinery/pkg/types"                     // ApplyPatchType, MergePatchType

	"replic2/internal/controller/backup" // GVR (Backup), CoreResourceTypes
	"replic2/internal/k8s"               // Kubernetes client wrapper
	"replic2/internal/store"             // GetObject, ListKeys, DecodeYAML
	apitypes "replic2/internal/types"    // Restore struct, phase constants
)

// restoreAgentPodTimeout is the maximum time to wait for the restore agent pod
// to complete before treating it as a failure.
const restoreAgentPodTimeout = 30 * time.Minute

// GVR is the GroupVersionResource for the Restore CRD.
var GVR = schema.GroupVersionResource{
	Group:    "replic2.io", // our custom API group
	Version:  "v1alpha1",   // CRD version
	Resource: "restores",   // plural resource name
}

// Run polls for Restore CRs every 5 seconds until ctx is cancelled.
func Run(ctx context.Context, c *k8s.Clients) {
	log.Println("restore controller: started")
	for {
		select {
		case <-ctx.Done(): // context cancelled — shut down cleanly
			log.Println("restore controller: stopped")
			return
		case <-time.After(5 * time.Second): // wait before each reconcile pass
			if err := reconcile(ctx, c); err != nil {
				log.Printf("restore controller: reconcile error: %v", err)
			}
		}
	}
}

// reconcile lists all Restore CRs and processes any that are pending.
func reconcile(ctx context.Context, c *k8s.Clients) error {
	// Fetch all Restore CRs cluster-wide using the dynamic client.
	list, err := c.Dynamic.Resource(GVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list restores: %w", err)
	}

	for _, item := range list.Items {
		// Marshal the unstructured item to JSON so we can decode it into our typed struct.
		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("restore controller: marshal item: %v", err)
			continue // skip items we cannot read
		}

		var r apitypes.Restore
		if err := json.Unmarshal(raw, &r); err != nil {
			log.Printf("restore controller: decode restore: %v", err)
			continue // skip malformed CRs
		}

		// Only process restores that have not started yet.
		if r.Status.Phase != "" && r.Status.Phase != apitypes.PhasePending {
			continue // already InProgress, Completed, or Failed — leave it alone
		}

		// Spawn one goroutine per CR so slow restores do not block the poll loop.
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
	ns := r.Spec.Namespace // target namespace to restore into
	log.Printf("restore controller: [%s] restoring namespace %q", r.Name, ns)

	// Phase: InProgress — mark the restore as started before doing any work.
	now := metav1.Now()
	r.Status.Phase = apitypes.PhaseInProgress
	r.Status.StartedAt = &now
	r.Status.Message = fmt.Sprintf("restoring namespace %q", ns)
	if err := patchStatus(ctx, c, r); err != nil {
		return fmt.Errorf("set InProgress: %w", err)
	}

	// Locate the S3 key prefix that contains the backup manifests.
	keyPrefix, err := findBackupPath(ctx, c, r)
	if err != nil {
		return markFailed(ctx, c, r, fmt.Errorf("locate backup: %w", err))
	}

	// Recreate the namespace if it does not already exist.
	if err := ensureNamespace(ctx, c, ns); err != nil {
		return markFailed(ctx, c, r, fmt.Errorf("ensure namespace: %w", err))
	}

	// Apply all manifests stored under keyPrefix in S3.
	if err := applyBackupDirectory(ctx, c, keyPrefix, ns); err != nil {
		return markFailed(ctx, c, r, fmt.Errorf("apply resources: %w", err))
	}

	// Restore raw PVC data from S3 tars (if any were backed up).
	// This runs after manifests so the PVCs exist and are Bound before we
	// try to mount them in the restore agent pods.
	if err := restorePVCData(ctx, c, r, keyPrefix, ns); err != nil {
		return markFailed(ctx, c, r, fmt.Errorf("restore PVC data: %w", err))
	}

	// Phase: Completed.
	done := metav1.Now()
	r.Status.Phase = apitypes.PhaseCompleted
	r.Status.CompletedAt = &done
	r.Status.RestoredFrom = keyPrefix // record the S3 key prefix used
	r.Status.Message = "restore complete"
	if err := patchStatus(ctx, c, r); err != nil {
		return fmt.Errorf("set Completed: %w", err)
	}

	log.Printf("restore controller: [%s] done — restored from s3 prefix: %s", r.Name, keyPrefix)
	return nil
}

// findBackupPath returns the S3 key prefix to restore from.
//
// If spec.backupName is set, the controller fetches that Backup CR and uses
// its StoragePath field (which holds the S3 key prefix).  Otherwise it scans
// all Backup CRs for the namespace and picks the most recently completed one.
func findBackupPath(ctx context.Context, c *k8s.Clients, r *apitypes.Restore) (string, error) {
	ns := r.Spec.Namespace // namespace we are looking for backups of

	if r.Spec.BackupName != "" {
		// Explicit backup name — look it up directly.
		item, err := c.Dynamic.Resource(backup.GVR).Get(ctx, r.Spec.BackupName, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get Backup %q: %w", r.Spec.BackupName, err)
		}
		raw, _ := item.MarshalJSON() // convert unstructured to JSON
		var b apitypes.Backup
		if err := json.Unmarshal(raw, &b); err != nil {
			return "", fmt.Errorf("decode Backup: %w", err)
		}
		if b.Status.StoragePath == "" {
			// The backup has no storage path — it either failed or is still running.
			return "", fmt.Errorf("Backup %q has no storage path (phase: %s)", r.Spec.BackupName, b.Status.Phase)
		}
		return b.Status.StoragePath, nil // return the S3 key prefix from the Backup status
	}

	// Auto-select: find the most recently completed Backup CR for this namespace.
	list, err := c.Dynamic.Resource(backup.GVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list backups: %w", err)
	}

	var latestBackup *apitypes.Backup // track the newest completed backup
	for _, item := range list.Items {
		raw, _ := item.MarshalJSON() // convert unstructured item to JSON
		var b apitypes.Backup
		if err := json.Unmarshal(raw, &b); err != nil {
			log.Printf("restore controller: decode backup item: %v", err)
			continue // skip malformed CRs
		}

		// Only consider completed backups for our target namespace with a valid storage path.
		if b.Spec.Namespace != ns || b.Status.Phase != apitypes.PhaseCompleted || b.Status.StoragePath == "" {
			continue
		}
		// Guard: a completed backup without a CompletedAt timestamp is invalid.
		if b.Status.CompletedAt == nil {
			continue
		}

		bCopy := b // capture a copy to avoid loop-variable aliasing
		if latestBackup == nil || bCopy.Status.CompletedAt.After(latestBackup.Status.CompletedAt.Time) {
			latestBackup = &bCopy // this is the most recent completed backup so far
		}
	}

	if latestBackup == nil {
		return "", fmt.Errorf("no completed backups found for namespace %q", ns)
	}
	return latestBackup.Status.StoragePath, nil // S3 key prefix of the latest backup
}

// ensureNamespace creates the namespace if it does not already exist.
func ensureNamespace(ctx context.Context, c *k8s.Clients, ns string) error {
	_, err := c.Core.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil // namespace already exists — nothing to do
	}
	if !k8serrors.IsNotFound(err) {
		return fmt.Errorf("get namespace: %w", err) // unexpected error
	}

	// Namespace is missing — create it.
	nsObj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}
	_, err = c.Core.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		// IsAlreadyExists is safe to ignore — created by a concurrent goroutine.
		return fmt.Errorf("create namespace: %w", err)
	}
	log.Printf("restore: created namespace %q", ns)
	return nil
}

// applyBackupDirectory enumerates all S3 manifest keys under keyPrefix and
// applies them to the cluster.
//
// Pass 1: apply coreResourceTypes (dependency-ordered: SA → CM → PVC → …).
// Pass 2: apply any remaining resource directories discovered from S3 that were
// not covered by coreResourceTypes (third-party CRD instances).
func applyBackupDirectory(ctx context.Context, c *k8s.Clients, keyPrefix, ns string) error {
	// If c.S3 is nil (e.g. in unit tests with fake clients), skip S3 operations.
	if c.S3 == nil {
		log.Printf("restore: S3 client not configured — skipping manifest apply for %q", keyPrefix)
		return nil
	}

	// List every key under this backup's prefix in S3.
	// Keys look like: <ns>/<backup-name>/<resource>/<name>.yaml
	allKeys, err := store.ListKeys(ctx, c.S3, keyPrefix+"/")
	if err != nil {
		return fmt.Errorf("list S3 keys under %q: %w", keyPrefix, err)
	}

	// Index keys by resource directory name (the segment after keyPrefix/).
	// e.g. "my-app/my-backup-01/configmaps/cfg.yaml" → resource="configmaps"
	keysByResource := make(map[string][]string) // resource → []key
	for _, key := range allKeys {
		// Strip the keyPrefix and leading slash to get the relative path.
		rel := strings.TrimPrefix(key, keyPrefix+"/") // e.g. "configmaps/cfg.yaml"
		parts := strings.SplitN(rel, "/", 2)          // split into resource + file name
		if len(parts) != 2 || !strings.HasSuffix(parts[1], ".yaml") {
			continue // skip keys that don't match expected manifest layout
		}
		resource := parts[0]                                             // e.g. "configmaps"
		keysByResource[resource] = append(keysByResource[resource], key) // group by resource
	}

	// Pass 1: apply core resource types in dependency order.
	coreApplied := make(map[string]bool) // track which resources were handled in pass 1
	for _, gvr := range backup.CoreResourceTypes() {
		keys, ok := keysByResource[gvr.Resource] // find keys for this resource type
		if !ok {
			continue // this resource type has no backups — skip silently
		}
		applyKeys(ctx, c, keys, &gvr, ns) // apply all objects for this resource type
		coreApplied[gvr.Resource] = true  // mark as handled so pass 2 skips it
	}

	// Pass 2: apply any remaining resource directories not covered by coreResourceTypes.
	for resource, keys := range keysByResource {
		if coreApplied[resource] {
			continue // already handled in pass 1
		}
		if resource == "pvc-data" {
			continue // raw PVC archive — not a manifest; skip it
		}
		// GVR is unknown for CRD resources — pass nil to derive it from the object content.
		applyKeys(ctx, c, keys, nil, ns)
	}
	return nil
}

// applyKeys fetches each S3 object in keys and applies it to the cluster.
// If gvr is non-nil it is used directly; otherwise the GVR is derived from the
// YAML content's apiVersion/kind fields (used for CRD resources).
func applyKeys(ctx context.Context, c *k8s.Clients, keys []string, gvr *schema.GroupVersionResource, ns string) {
	for _, key := range keys {
		if err := applyS3Object(ctx, c, key, gvr, ns); err != nil {
			log.Printf("restore: apply %s: %v", key, err) // best-effort: log and continue
		}
	}
}

// applyS3Object downloads one S3 object and applies it to the cluster via
// Server-Side Apply (create-or-update in one call).  Falls back to Create
// for older clusters that predate SSA.
func applyS3Object(ctx context.Context, c *k8s.Clients, key string, gvr *schema.GroupVersionResource, targetNS string) error {
	// Download and decode the YAML manifest from S3.
	obj, err := store.GetObject(ctx, c.S3, key)
	if err != nil {
		return fmt.Errorf("get S3 object %q: %w", key, err)
	}

	// Wrap the decoded map in an Unstructured object for the dynamic client.
	u := &unstructured.Unstructured{Object: obj}
	u.SetNamespace(targetNS)              // override namespace to the restore target
	u.SetResourceVersion("")              // must be blank for a clean SSA apply
	u.SetUID("")                          // UID is server-assigned — strip it
	u.SetCreationTimestamp(metav1.Time{}) // creation time is server-assigned — strip it
	u.SetManagedFields(nil)               // managed fields are server-side metadata
	delete(u.Object, "status")            // status is reconciled by controllers, not stored

	// PersistentVolumeClaims: strip the old binding so the provisioner creates
	// a fresh PV.  Without this, SSA re-applies the old volumeName which points
	// to a deleted PV, leaving the PVC in "Lost" state forever.
	if u.GetKind() == "PersistentVolumeClaim" {
		spec, _, _ := unstructured.NestedMap(u.Object, "spec")
		if spec != nil {
			delete(spec, "volumeName") // old PV is gone — let provisioner assign a new one
			// Annotations that the provisioner writes at bind time must also be
			// removed so the provisioner does not see them as stale hints.
			annotations := u.GetAnnotations()
			delete(annotations, "pv.kubernetes.io/bind-completed")
			delete(annotations, "pv.kubernetes.io/bound-by-controller")
			delete(annotations, "volume.beta.kubernetes.io/storage-provisioner")
			delete(annotations, "volume.kubernetes.io/selected-node")
			delete(annotations, "volume.kubernetes.io/storage-provisioner")
			u.SetAnnotations(annotations)
			if err := unstructured.SetNestedMap(u.Object, spec, "spec"); err != nil {
				log.Printf("restore: strip PVC binding for %s: %v", u.GetName(), err)
			}
		}
	}

	// Resolve the GVR: use the provided one or derive it from the object's apiVersion/kind.
	resolved := gvr
	if resolved == nil {
		derived, err := gvrFromObject(c, u)
		if err != nil {
			return fmt.Errorf("derive GVR for %q: %w", key, err)
		}
		resolved = &derived
	}

	// Marshal the cleaned object back to JSON for the patch call.
	raw, err := json.Marshal(u.Object)
	if err != nil {
		return fmt.Errorf("marshal object for %q: %w", key, err)
	}

	// Server-Side Apply: create or update the object in one atomic call.
	_, err = c.Dynamic.Resource(*resolved).Namespace(targetNS).Patch(
		ctx,
		u.GetName(),
		types.ApplyPatchType, // SSA patch type
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

// gvrFromObject looks up the GVR for an Unstructured object by querying the
// discovery client using the object's apiVersion and kind.
// Used for third-party CRD objects where no static GVR is known.
func gvrFromObject(c *k8s.Clients, u *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	gvk := u.GroupVersionKind() // extract apiVersion+kind from the object

	// Query the API server's discovery endpoint for all preferred namespaced resources.
	mapper, err := c.Discovery.ServerPreferredResources()
	if err != nil && mapper == nil {
		return schema.GroupVersionResource{}, fmt.Errorf("discovery: %w", err)
	}

	// Walk the resource lists to find one whose group/version and kind match.
	for _, list := range mapper {
		gv, err := schema.ParseGroupVersion(list.GroupVersion) // parse "apps/v1" or "v1"
		if err != nil {
			continue // skip unparseable group versions
		}
		if gv.Group != gvk.Group || gv.Version != gvk.Version {
			continue // wrong group or version — skip this list
		}
		for _, r := range list.APIResources {
			// Sub-resources (e.g. "pods/log") contain "/" — skip them.
			if strings.Contains(r.Name, "/") {
				continue
			}
			if r.Kind == gvk.Kind {
				return schema.GroupVersionResource{
					Group:    gv.Group,
					Version:  gv.Version,
					Resource: r.Name, // e.g. "certificates" for cert.cert-manager.io
				}, nil
			}
		}
	}
	return schema.GroupVersionResource{}, fmt.Errorf("no resource found for GVK %s", gvk)
}

// ---------------------------------------------------------------------------
// PVC data restore — agent pods that pull tar archives from S3 into new PVCs.
// ---------------------------------------------------------------------------

// restorePVCData scans the S3 prefix for pvc-data/*.tar objects.  For each tar
// found it waits for the matching PVC to be Bound (the provisioner may take a
// few seconds), then spawns a restore agent pod that downloads the tar from S3
// and extracts it into the freshly provisioned PVC.
//
// This mirrors backupPVCData / backupSinglePVC from the backup controller but
// in reverse: instead of "tar | aws s3 cp" it runs "aws s3 cp | tar -x".
func restorePVCData(ctx context.Context, c *k8s.Clients, r *apitypes.Restore, keyPrefix, ns string) error {
	// Skip if S3 is not configured (unit tests with fake clients).
	if c.S3 == nil {
		return nil
	}

	// List all S3 keys under the pvc-data sub-prefix.
	pvcPrefix := keyPrefix + "/pvc-data/"
	keys, err := store.ListKeys(ctx, c.S3, pvcPrefix)
	if err != nil {
		return fmt.Errorf("list pvc-data keys: %w", err)
	}
	if len(keys) == 0 {
		log.Printf("restore: [%s] no pvc-data objects found — skipping PVC data restore", r.Name)
		return nil
	}

	for _, key := range keys {
		// Derive PVC name from the archive key.
		// Key format: <keyPrefix>/pvc-data/<pvcName>.tar  (full)
		//         or: <keyPrefix>/pvc-data/<pvcName>-incremental.tar
		// We skip incremental archives — they do not contain a self-consistent
		// snapshot and cannot be extracted stand-alone.
		fileName := strings.TrimPrefix(key, pvcPrefix) // e.g. "example-app-data.tar"
		if strings.HasSuffix(fileName, "-incremental.tar") {
			log.Printf("restore: [%s] skipping incremental archive %q — full restore only", r.Name, key)
			continue
		}
		if !strings.HasSuffix(fileName, ".tar") {
			continue // not a tar archive — skip
		}
		pvcName := strings.TrimSuffix(fileName, ".tar") // e.g. "example-app-data"

		if err := restoreSinglePVC(ctx, c, r, ns, key, pvcName); err != nil {
			// One PVC failure should not abort the rest.
			log.Printf("restore: [%s] PVC %q data restore error: %v", r.Name, pvcName, err)
		}
	}
	return nil
}

// restoreSinglePVC waits for pvcName to become Bound, then spawns an agent pod
// that downloads the tar from S3 and extracts it into the PVC's /data mount.
func restoreSinglePVC(ctx context.Context, c *k8s.Clients, r *apitypes.Restore, ns, s3Key, pvcName string) error {
	// ------------------------------------------------------------------
	// 1. Wait for the PVC to reach Bound phase (provisioner may need time).
	// ------------------------------------------------------------------
	log.Printf("restore: [%s] waiting for PVC %q to become Bound", r.Name, pvcName)
	bindDeadline := time.Now().Add(5 * time.Minute)
	for {
		if time.Now().After(bindDeadline) {
			return fmt.Errorf("PVC %q did not become Bound within 5 minutes", pvcName)
		}
		pvc, err := c.Core.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
		if err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("get PVC %q: %w", pvcName, err)
		}
		if err == nil && pvc.Status.Phase == corev1.ClaimBound {
			break // PVC is ready
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	log.Printf("restore: [%s] PVC %q is Bound — starting data restore", r.Name, pvcName)

	// ------------------------------------------------------------------
	// 2. Build the S3 source URI and shell command for the agent pod.
	//    The agent runs: aws s3 cp <s3URI> - | tar -x -C /data
	//    (reverse of the backup command).
	// ------------------------------------------------------------------
	bucket := os.Getenv("S3_BUCKET") // S3 bucket name
	s3URI := fmt.Sprintf("s3://%s/%s", bucket, s3Key)

	// Build the optional --endpoint-url flag for MinIO / non-AWS providers.
	endpointFlag := ""
	if endpoint := os.Getenv("S3_ENDPOINT"); endpoint != "" {
		endpointFlag = fmt.Sprintf(" --endpoint-url '%s'", endpoint)
	}

	// The agent downloads the tar from S3 and pipes it straight into tar -x.
	// We install tar first (amazon/aws-cli image does not include it by default).
	shellCmd := fmt.Sprintf(
		`yum install -y -q tar && aws s3 cp '%s' -%s | tar -x -C /data`,
		s3URI, endpointFlag,
	)

	// ------------------------------------------------------------------
	// 3. Build the pod name (max 63 chars).
	// ------------------------------------------------------------------
	podName := fmt.Sprintf("replic2-restore-%s-%s", r.Name, pvcName)
	if len(podName) > 63 {
		podName = podName[:63]
	}

	// ------------------------------------------------------------------
	// 4. S3 credentials for the agent pod.
	// ------------------------------------------------------------------
	agentEnv := []corev1.EnvVar{
		{Name: "AWS_ACCESS_KEY_ID", Value: os.Getenv("S3_ACCESS_KEY_ID")},
		{Name: "AWS_SECRET_ACCESS_KEY", Value: os.Getenv("S3_SECRET_ACCESS_KEY")},
		{Name: "AWS_DEFAULT_REGION", Value: os.Getenv("S3_REGION")},
	}

	// ------------------------------------------------------------------
	// 5. Define and create the restore agent pod.
	//    The PVC is mounted read-write at /data so tar can write into it.
	// ------------------------------------------------------------------
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "replic2",
				"replic2.io/restore":           r.Name,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, // run once; do not retry
			Containers: []corev1.Container{
				{
					Name:    "agent",
					Image:   "amazon/aws-cli",
					Command: []string{"sh", "-c", shellCmd},
					Env:     agentEnv,
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "target-pvc",
							MountPath: "/data", // extract tar contents here
							ReadOnly:  false,   // write access required
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "target-pvc",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName, // the freshly provisioned PVC
							ReadOnly:  false,
						},
					},
				},
			},
		},
	}

	// Clean up any stale pod from a previous failed attempt before creating.
	if err := deleteAndWaitForRestorePodGone(ctx, c, ns, podName); err != nil {
		return fmt.Errorf("cleanup stale restore pod %q: %w", podName, err)
	}

	if _, err := c.Core.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create restore agent pod for PVC %q: %w", pvcName, err)
	}
	log.Printf("restore: [%s] restore agent pod %q created for PVC %q", r.Name, podName, pvcName)

	// Always clean up the pod when we return.
	defer func() {
		_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})
		log.Printf("restore: [%s] restore agent pod %q deleted", r.Name, podName)
	}()

	// ------------------------------------------------------------------
	// 6. Poll until the agent pod succeeds or fails.
	// ------------------------------------------------------------------
	deadline := time.Now().Add(restoreAgentPodTimeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("restore agent pod %q timed out after %s", podName, restoreAgentPodTimeout)
		}
		p, err := c.Core.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get restore agent pod %q: %w", podName, err)
		}
		switch p.Status.Phase {
		case corev1.PodSucceeded:
			log.Printf("restore: [%s] PVC %q data restored from %s", r.Name, pvcName, s3URI)
			return nil
		case corev1.PodFailed:
			return fmt.Errorf("restore agent pod %q failed — check pod logs for details", podName)
		default:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// deleteAndWaitForRestorePodGone deletes the named pod (ignoring "not found")
// and polls until the API server confirms it no longer exists.
func deleteAndWaitForRestorePodGone(ctx context.Context, c *k8s.Clients, ns, podName string) error {
	err := c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("delete pod: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("pod %q still exists after 30s", podName)
		}
		_, err := c.Core.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil && k8serrors.IsNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ---------------------------------------------------------------------------
// Exported wrappers — thin shims that expose internal functions to the test
// suite without changing the internal API.
// ---------------------------------------------------------------------------

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
func ApplyBackupDirectory(ctx context.Context, c *k8s.Clients, keyPrefix, ns string) error {
	return applyBackupDirectory(ctx, c, keyPrefix, ns)
}

// ReconcileRestores is the exported entry point for tests.
func ReconcileRestores(ctx context.Context, c *k8s.Clients) error { return reconcile(ctx, c) }

// ---------------------------------------------------------------------------
// Status helpers
// ---------------------------------------------------------------------------

// patchStatus writes only the status sub-object back via a merge-patch on the
// /status subresource; falls back to a full Update if necessary.
func patchStatus(ctx context.Context, c *k8s.Clients, r *apitypes.Restore) error {
	// Build the JSON merge-patch body: {"status": { … }}.
	statusOnly, err := json.Marshal(map[string]interface{}{"status": r.Status})
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}

	// Attempt the preferred path: subresource patch.
	_, err = c.Dynamic.Resource(GVR).
		Patch(ctx, r.Name, types.MergePatchType, statusOnly, metav1.PatchOptions{}, "status")
	if err == nil {
		return nil // success
	}

	// Fallback: re-fetch to get the current resourceVersion, inject our status, and Update.
	latest, getErr := c.Dynamic.Resource(GVR).Get(ctx, r.Name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("patch status (subresource): %v; re-fetch: %w", err, getErr)
	}
	raw, _ := json.Marshal(r.Status) // marshal our typed struct to JSON
	var statusMap map[string]interface{}
	_ = json.Unmarshal(raw, &statusMap) // decode into a generic map
	latest.Object["status"] = statusMap // overwrite the status field

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
	r.Status.Message = cause.Error() // record the failure reason
	_ = patchStatus(ctx, c, r)       // best-effort — ignore patch error on failure path
	return cause                     // propagate the original error
}

// boolPtr returns a pointer to the given bool value.
// Used for the Force field in SSA PatchOptions.
func boolPtr(b bool) *bool { return &b }
