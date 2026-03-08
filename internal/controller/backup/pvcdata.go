// pvcdata.go — raw PVC data backup via a self-terminating agent pod.
//
// When spec.includePVCData is true, the backup controller cannot mount the
// source PVC directly (RWO volumes allow only one writer at a time and the
// source PVC may already be mounted on a different node).  Instead it:
//
//  1. Spawns a short-lived "agent" pod in the source namespace that mounts
//     the source PVC read-only.  The pod's command IS the tar invocation,
//     writing the archive to stdout.  No backup PVC is mounted in the agent
//     pod — the backup PVC is only ever mounted by the replic2 controller.
//
//  2. Waits for the pod to reach Succeeded or Failed by polling its phase.
//     The pod terminates itself as soon as tar exits — no exec, no SPDY, no
//     force-kill needed.
//
//  3. Streams the pod's logs (tar's stdout) directly to an archive file on
//     the backup PVC, which is already mounted in this process.
//
//  4. Deletes the pod via defer — by the time the defer runs the pod is
//     already in the Succeeded phase, so the delete just removes a completed
//     pod rather than killing a live one.
//
// For incremental backups, "--newer-mtime=<RFC3339>" is prepended to the tar
// command so only files modified after the previous backup's completedAt are
// included in the archive.
//
// Archive naming:
//   - Full:        <pvcDataDir>/<pvcName>.tar
//   - Incremental: <pvcDataDir>/<pvcName>-incremental.tar
package backup

import (
	"context"       // for cancellation / deadlines
	"fmt"           // for error wrapping
	"io"            // for io.Copy (stream logs to archive file)
	"log"           // for structured logging
	"os"            // for MkdirAll and Create (write archive to backup PVC)
	"path/filepath" // for Join (build archive path)
	"time"          // for sinceTime, agentPodTimeout, RFC3339 format

	corev1 "k8s.io/api/core/v1"                   // Pod, PVC types and phase constants
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // ObjectMeta, ListOptions, DeleteOptions

	"replic2/internal/k8s"            // Kubernetes client wrapper (c.Core)
	apitypes "replic2/internal/types" // Backup struct (b.Name for logging/labels)
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

// backupSinglePVC spawns an agent pod whose command IS the tar invocation.
// tar writes to stdout, which the Kubernetes log API captures.  Once the pod
// reaches Succeeded the controller streams the pod logs to an archive file on
// the backup PVC.  The pod is then deleted (it is already completed by that
// point, so no force-kill occurs).
func backupSinglePVC(ctx context.Context, c *k8s.Clients, b *apitypes.Backup, ns, pvcDataDir, pvcName string, sinceTime time.Time) error {
	// -----------------------------------------------------------------------
	// 1. Decide archive name and build the tar command (pod's entrypoint).
	//    tar writes the archive to stdout (no -f flag = stdout by default).
	// -----------------------------------------------------------------------
	archiveName := pvcName + ".tar" // full backup
	if !sinceTime.IsZero() {
		archiveName = pvcName + "-incremental.tar" // incremental backup
	}
	archivePath := filepath.Join(pvcDataDir, archiveName) // destination on the backup PVC

	// Build the tar argv.  All output goes to stdout so the log API captures it.
	tarCmd := []string{"tar", "-c", "-C", "/data", "."} // full: archive everything
	if !sinceTime.IsZero() {
		// GNU tar --newer-mtime accepts RFC3339 timestamps.
		tarCmd = []string{
			"tar",
			"--newer-mtime=" + sinceTime.UTC().Format(time.RFC3339), // mtime cut-off
			"-c", "-C", "/data", ".", // archive only files newer than sinceTime
		}
	}

	// -----------------------------------------------------------------------
	// 2. Build the pod name; truncate to the 63-char Kubernetes DNS label limit.
	// -----------------------------------------------------------------------
	podName := fmt.Sprintf("replic2-backup-%s-%s", b.Name, pvcName)
	if len(podName) > 63 {
		podName = podName[:63]
	}

	// -----------------------------------------------------------------------
	// 3. Define the agent pod.
	//    - Mounts the source PVC read-only at /data.
	//    - Its command IS the tar invocation — it exits as soon as tar finishes.
	//    - The backup PVC is never mounted here; data flows via the log stream.
	// -----------------------------------------------------------------------
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns, // must be in the same namespace as the source PVC
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "replic2", // for kubectl filtering
				"replic2.io/backup":            b.Name,    // links the pod to its backup CR
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, // run once; do not restart on failure
			Containers: []corev1.Container{
				{
					Name:    "agent",
					Image:   "busybox:stable", // provides GNU tar
					Command: tarCmd,           // tar runs as PID 1; pod exits when tar exits
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "source-pvc",
							MountPath: "/data", // source files are accessible here
							ReadOnly:  true,    // never modify source data
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
							ClaimName: pvcName, // the PVC to back up
							ReadOnly:  true,    // safe: no accidental writes to source
						},
					},
				},
			},
		},
	}

	// Delete any leftover pod from a previous (failed) attempt so Create succeeds.
	_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})

	if _, err := c.Core.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create agent pod for PVC %q: %w", pvcName, err)
	}
	log.Printf("backup controller: [%s] agent pod %q created for PVC %q", b.Name, podName, pvcName)

	// Always delete the pod when this function returns, regardless of outcome.
	// In the success path the pod is already Succeeded at this point, so the
	// delete just removes a completed pod cleanly.
	defer func() {
		_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})
		log.Printf("backup controller: [%s] agent pod %q deleted", b.Name, podName)
	}()

	// -----------------------------------------------------------------------
	// 4. Poll until the pod reaches a terminal phase (Succeeded or Failed).
	//    The pod terminates itself as soon as tar exits — no force-kill needed.
	// -----------------------------------------------------------------------
	deadline := time.Now().Add(agentPodTimeout) // absolute deadline
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("agent pod %q timed out after %s", podName, agentPodTimeout)
		}

		p, err := c.Core.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get agent pod %q: %w", podName, err)
		}

		switch p.Status.Phase {
		case corev1.PodSucceeded:
			// tar finished successfully — proceed to stream logs below.
			log.Printf("backup controller: [%s] agent pod %q succeeded", b.Name, podName)
		case corev1.PodFailed:
			// tar exited non-zero — surface the error.
			return fmt.Errorf("agent pod %q failed (check pod logs for tar error)", podName)
		default:
			// Still Pending or Running — wait and poll again.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second): // poll interval
			}
			continue // go back to the top of the loop
		}
		break // only reached on PodSucceeded
	}

	// -----------------------------------------------------------------------
	// 5. Stream the pod logs (tar's stdout) into the archive file on the
	//    backup PVC.  The pod has already exited so the log stream is complete
	//    and will not block.
	// -----------------------------------------------------------------------
	logStream, err := c.Core.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{
		Container: "agent", // the container whose stdout we want
	}).Stream(ctx)
	if err != nil {
		return fmt.Errorf("get logs for pod %q: %w", podName, err)
	}
	defer logStream.Close()

	// Open (or create) the destination archive file on the backup PVC.
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("create archive file %q: %w", archivePath, err)
	}
	defer archiveFile.Close()

	// Copy the log stream (tar bytes) directly into the archive file.
	written, err := io.Copy(archiveFile, logStream)
	if err != nil {
		return fmt.Errorf("stream logs to archive %q: %w", archivePath, err)
	}

	log.Printf("backup controller: [%s] archived PVC %q → %s (%d bytes)", b.Name, pvcName, archivePath, written)
	return nil
}
