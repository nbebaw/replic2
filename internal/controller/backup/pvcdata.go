// pvcdata.go — raw PVC data backup via a temporary agent pod.
//
// When spec.includePVCData is true, the backup controller cannot mount the
// source PVC directly (RWO volumes allow only one writer at a time, and
// replic2 is already running on a different node).  Instead it spawns a
// short-lived "agent" pod in the source namespace that:
//
//  1. Mounts the source PVC at /data (read-only).
//  2. Mounts the replic2 backup PVC (store.BackupPVCName()) at
//     store.BackupRoot() (read-write) — using a PVC volume, not a HostPath.
//     This is required because the backup root only exists inside the replic2
//     pod; the path does not exist on the underlying node filesystem, so a
//     HostPath mount would fail with "read-only file system" at container init.
//  3. Runs "tar -cf <archivePath> -C /data ." to archive all files.
//     For incremental backups, "--newer-mtime=<RFC3339>" is prepended so only
//     files modified after the previous backup's completedAt are included.
//
// The controller waits up to agentPodTimeout for the pod to reach
// Succeeded or Failed, then cleans up the pod regardless of outcome.
package backup

import (
	"context"       // for cancellation / deadlines
	"fmt"           // for error wrapping
	"log"           // for structured logging
	"os"            // for MkdirAll (create pvc-data directory)
	"path/filepath" // for Join (build archive path)
	"time"          // for sinceTime, agentPodTimeout, RFC3339 format

	corev1 "k8s.io/api/core/v1"                   // Pod, PVC types and constants
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // ObjectMeta, ListOptions, DeleteOptions

	"replic2/internal/k8s"            // Kubernetes client wrapper
	"replic2/internal/store"          // BackupRoot() — PVC mount path on the replic2 pod
	apitypes "replic2/internal/types" // Backup struct (for b.Name used in logging/labels)
)

// backupPVCData iterates over every PVC in namespace ns and calls
// backupSinglePVC for each one that is in the Bound phase.
//
// sinceTime controls incremental vs full:
//   - zero value  → full backup (all files included)
//   - non-zero    → incremental (only files with mtime > sinceTime)
func backupPVCData(ctx context.Context, c *k8s.Clients, b *apitypes.Backup, ns, storagePath string, sinceTime time.Time) error {
	// List all PVCs in the target namespace.
	pvcList, err := c.Core.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list PVCs in %q: %w", ns, err)
	}
	if len(pvcList.Items) == 0 {
		// No PVCs at all — nothing to do.
		log.Printf("backup controller: [%s] no PVCs found in namespace %q — skipping PVC data backup", b.Name, ns)
		return nil
	}

	// Create the pvc-data sub-directory inside this backup's storage path.
	pvcDataDir := filepath.Join(storagePath, "pvc-data")
	if err := os.MkdirAll(pvcDataDir, 0o755); err != nil {
		return fmt.Errorf("mkdir pvc-data: %w", err)
	}

	for _, pvc := range pvcList.Items {
		// Only back up PVCs that are actively bound to a volume.
		if pvc.Status.Phase != corev1.ClaimBound {
			log.Printf("backup controller: [%s] PVC %q is not Bound (phase=%s) — skipping", b.Name, pvc.Name, pvc.Status.Phase)
			continue
		}
		if err := backupSinglePVC(ctx, c, b, ns, pvcDataDir, pvc.Name, sinceTime); err != nil {
			// One failing PVC should not abort the rest — log and continue.
			log.Printf("backup controller: [%s] PVC %q data backup error: %v", b.Name, pvc.Name, err)
		}
	}
	return nil
}

// backupSinglePVC spawns an agent pod that mounts pvcName (read-only) and
// writes a tar archive to the backup PVC.
//
// Archive naming:
//   - Full:        <pvcDataDir>/<pvcName>.tar
//   - Incremental: <pvcDataDir>/<pvcName>-incremental.tar
func backupSinglePVC(ctx context.Context, c *k8s.Clients, b *apitypes.Backup, ns, pvcDataDir, pvcName string, sinceTime time.Time) error {
	// Choose archive filename based on whether this is incremental.
	archiveName := pvcName + ".tar" // default: full backup
	if !sinceTime.IsZero() {
		archiveName = pvcName + "-incremental.tar" // incremental backup
	}
	archivePath := filepath.Join(pvcDataDir, archiveName) // absolute path on the backup PVC

	// Build the tar argument list.
	// For incremental runs, prepend --newer-mtime so only changed files are archived.
	tarArgs := []string{"-cf", archivePath, "-C", "/data", "."} // full: archive everything
	if !sinceTime.IsZero() {
		// GNU tar accepts RFC3339 timestamps for --newer-mtime.
		tarArgs = []string{
			"--newer-mtime=" + sinceTime.UTC().Format(time.RFC3339), // cut-off timestamp
			"-cf", archivePath, // output archive
			"-C", "/data", ".", // source directory inside the agent pod
		}
	}

	// Build the pod name from the backup name and PVC name.
	// Truncate to 63 characters to stay within the DNS label limit.
	podName := fmt.Sprintf("replic2-backup-%s-%s", b.Name, pvcName)
	if len(podName) > 63 {
		podName = podName[:63] // hard cap at the Kubernetes DNS label limit
	}

	backupRoot := store.BackupRoot()   // where the backup PVC is mounted in the agent pod
	backupPVC := store.BackupPVCName() // the PVC that holds all backups

	// Define the agent pod.  It uses BusyBox for the tar command, mounts the
	// source PVC read-only and the replic2 backup PVC read-write.
	// We mount the backup PVC directly (not via HostPath) because the backup
	// root path only exists inside the replic2 pod — not on the host node.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns, // run in the same namespace as the source PVC
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "replic2", // for kubectl selection
				"replic2.io/backup":            b.Name,    // link pod to this backup
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, // run once; do not retry on failure
			Containers: []corev1.Container{
				{
					Name:    "agent",
					Image:   "busybox:stable",                    // provides GNU tar
					Command: append([]string{"tar"}, tarArgs...), // tar command with our args
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "source-pvc", // the PVC being backed up
							MountPath: "/data",      // mounted read-only inside the container
							ReadOnly:  true,
						},
						{
							Name:      "backup-storage", // the replic2 backup PVC (write destination)
							MountPath: backupRoot,       // same mount path as in the replic2 pod
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					// source-pvc: the PVC whose data we are archiving (read-only).
					Name: "source-pvc",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName, // the PVC we want to back up
							ReadOnly:  true,    // safe: we never modify source data
						},
					},
				},
				{
					// backup-storage: the replic2 backup PVC mounted directly.
					// Using a PVC volume (not HostPath) because the backup root path
					// only exists inside the replic2 container, not on the node.
					Name: "backup-storage",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: backupPVC, // e.g. "replic2-backups" (store.BackupPVCName())
							ReadOnly:  false,     // we need to write the tar archive here
						},
					},
				},
			},
		},
	}

	// Delete any leftover pod from a previous (failed) attempt so Create succeeds.
	_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{}) // ignore "not found"

	// Create the agent pod.
	if _, err := c.Core.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create agent pod for PVC %q: %w", pvcName, err)
	}
	log.Printf("backup controller: [%s] agent pod %q created for PVC %q", b.Name, podName, pvcName)

	// Poll until the pod reaches a terminal phase (Succeeded / Failed) or we time out.
	deadline := time.Now().Add(agentPodTimeout) // when to give up
	for {
		// Check for timeout first.
		if time.Now().After(deadline) {
			_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{}) // clean up
			return fmt.Errorf("agent pod %q timed out after %s", podName, agentPodTimeout)
		}

		// Fetch the current pod status.
		p, err := c.Core.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get agent pod %q: %w", podName, err)
		}

		switch p.Status.Phase {
		case corev1.PodSucceeded: // tar finished successfully
			log.Printf("backup controller: [%s] agent pod %q succeeded — archive: %s", b.Name, podName, archivePath)
			_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{}) // clean up
			return nil

		case corev1.PodFailed: // tar exited non-zero
			_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{}) // clean up
			return fmt.Errorf("agent pod %q failed", podName)
		}

		// Not terminal yet — wait 5 seconds then poll again.
		select {
		case <-ctx.Done(): // context cancelled (leader lost, shutdown, …)
			_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{}) // clean up
			return ctx.Err()
		case <-time.After(5 * time.Second): // wait before next poll
		}
	}
}
