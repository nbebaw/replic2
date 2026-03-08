// pvcdata.go — raw PVC data backup via a temporary agent pod + exec streaming.
//
// When spec.includePVCData is true, the backup controller cannot mount the
// source PVC directly (RWO volumes allow only one writer at a time and the
// source PVC may already be mounted on a different node). Instead it:
//
//  1. Spawns a short-lived "agent" pod in the source namespace that mounts
//     the source PVC read-only and runs `sleep 3600` as its command.
//  2. Waits for the pod to reach the Running phase.
//  3. Uses client-go's remotecommand (SPDY/WebSocket exec) to run
//     `tar -c [-C /data . | --newer-mtime=T -C /data .]` inside the pod
//     and streams the tar output directly to a file on the backup PVC
//     (which is already mounted by the replic2 controller itself).
//     This avoids mounting the backup PVC inside the agent pod entirely,
//     so there is no RWO conflict on multi-node clusters.
//  4. Deletes the agent pod regardless of outcome.
//
// For incremental backups, "--newer-mtime=<RFC3339>" is prepended to the
// tar command so only files modified after the previous backup's completedAt
// are included in the archive.
package backup

import (
	"context"       // for cancellation / deadlines
	"fmt"           // for error wrapping
	"log"           // for structured logging
	"os"            // for MkdirAll and Create (write archive to backup PVC)
	"path/filepath" // for Join (build archive path)
	"time"          // for sinceTime, agentPodTimeout, RFC3339 format

	corev1 "k8s.io/api/core/v1"                   // Pod, PVC types and phase constants
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // ObjectMeta, ListOptions, DeleteOptions
	"k8s.io/client-go/kubernetes/scheme"          // ParameterCodec for PodExecOptions
	"k8s.io/client-go/tools/remotecommand"        // SPDY/WebSocket exec streaming

	"replic2/internal/k8s"            // Kubernetes client wrapper (c.REST, c.Core)
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

// backupSinglePVC spawns an agent pod that mounts pvcName read-only, then
// execs `tar` inside it and streams the output directly to an archive file on
// the replic2 backup PVC (which is already mounted in this process).
//
// Archive naming:
//   - Full:        <pvcDataDir>/<pvcName>.tar
//   - Incremental: <pvcDataDir>/<pvcName>-incremental.tar
//
// Because the backup PVC is never mounted inside the agent pod, this approach
// works on multi-node clusters with RWO backup PVCs.
func backupSinglePVC(ctx context.Context, c *k8s.Clients, b *apitypes.Backup, ns, pvcDataDir, pvcName string, sinceTime time.Time) error {
	// -----------------------------------------------------------------------
	// 1. Decide archive name and build the tar command to exec in the pod.
	// -----------------------------------------------------------------------
	archiveName := pvcName + ".tar" // default: full backup
	if !sinceTime.IsZero() {
		archiveName = pvcName + "-incremental.tar" // incremental backup
	}
	archivePath := filepath.Join(pvcDataDir, archiveName) // local path on the backup PVC

	// tar writes to stdout ("-" as the output file).
	// For incremental runs prepend --newer-mtime so only changed files stream.
	tarCmd := []string{"tar", "-c", "-C", "/data", "."} // full: stream everything
	if !sinceTime.IsZero() {
		// GNU tar accepts RFC3339 for --newer-mtime.
		tarCmd = []string{
			"tar",
			"--newer-mtime=" + sinceTime.UTC().Format(time.RFC3339), // mtime cut-off
			"-c", "-C", "/data", ".", // stream changed files to stdout
		}
	}

	// -----------------------------------------------------------------------
	// 2. Build the pod name; truncate to the 63-char DNS label limit.
	// -----------------------------------------------------------------------
	podName := fmt.Sprintf("replic2-backup-%s-%s", b.Name, pvcName)
	if len(podName) > 63 {
		podName = podName[:63]
	}

	// -----------------------------------------------------------------------
	// 3. Define the agent pod.
	//    It mounts ONLY the source PVC (read-only).  The backup PVC is never
	//    mounted here — we stream tar output back to the controller instead.
	//    The pod runs `sleep 3600` so it stays alive while we exec into it.
	// -----------------------------------------------------------------------
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns, // same namespace as the source PVC
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "replic2", // for kubectl filtering
				"replic2.io/backup":            b.Name,    // links the pod to its backup
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, // do not restart on failure
			Containers: []corev1.Container{
				{
					Name:    "agent",
					Image:   "busybox:stable",          // provides tar and sleep
					Command: []string{"sleep", "3600"}, // stay alive for exec window
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "source-pvc",
							MountPath: "/data", // source files appear here
							ReadOnly:  true,    // never modify source data
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					// source-pvc: the PVC whose data we are archiving.
					Name: "source-pvc",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName, // the PVC to back up
							ReadOnly:  true,    // read-only: safe, no accidental writes
						},
					},
				},
			},
		},
	}

	// Clean up any leftover pod from a previous attempt before creating a new one.
	_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})

	if _, err := c.Core.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create agent pod for PVC %q: %w", pvcName, err)
	}
	log.Printf("backup controller: [%s] agent pod %q created for PVC %q", b.Name, podName, pvcName)

	// Always delete the pod when we are done, regardless of success or failure.
	defer func() {
		_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})
		log.Printf("backup controller: [%s] agent pod %q deleted", b.Name, podName)
	}()

	// -----------------------------------------------------------------------
	// 4. Wait for the pod to reach the Running phase so exec is available.
	// -----------------------------------------------------------------------
	deadline := time.Now().Add(agentPodTimeout) // absolute timeout
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("agent pod %q did not reach Running within %s", podName, agentPodTimeout)
		}

		p, err := c.Core.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get agent pod %q: %w", podName, err)
		}

		if p.Status.Phase == corev1.PodRunning {
			break // pod is ready for exec
		}
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			return fmt.Errorf("agent pod %q reached terminal phase %q before exec", podName, p.Status.Phase)
		}

		// Poll every 2 seconds while waiting for Running.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	// -----------------------------------------------------------------------
	// 5. Open the destination archive file on the backup PVC.
	//    This file lives in the replic2 controller's own mount — we write to
	//    it by piping the exec stdout stream directly into it.
	// -----------------------------------------------------------------------
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("create archive file %q: %w", archivePath, err)
	}
	defer archiveFile.Close()

	// -----------------------------------------------------------------------
	// 6. Exec the tar command inside the agent pod and stream stdout here.
	//    remotecommand uses SPDY (or WebSocket on newer servers) to multiplex
	//    stdin/stdout/stderr over a single HTTP/2 connection.
	// -----------------------------------------------------------------------
	execReq := c.Core.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "agent",
			Command:   tarCmd, // the tar command to run
			Stdin:     false,  // we only need output
			Stdout:    true,   // tar stream goes here
			Stderr:    true,   // capture stderr for error reporting
			TTY:       false,  // no terminal needed
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.REST, "POST", execReq.URL())
	if err != nil {
		return fmt.Errorf("create SPDY executor for pod %q: %w", podName, err)
	}

	// stderr is captured into a byte buffer so we can include it in the error.
	var stderrBuf limitedBuffer
	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: archiveFile, // write tar stream directly to the archive file
		Stderr: &stderrBuf,  // capture tar's error messages
	})
	if streamErr != nil {
		return fmt.Errorf("exec tar in pod %q: %w (stderr: %s)", podName, streamErr, stderrBuf.String())
	}

	log.Printf("backup controller: [%s] streamed archive for PVC %q → %s", b.Name, pvcName, archivePath)
	return nil
}

// limitedBuffer is a simple byte buffer capped at 4 KiB, used to capture
// stderr from the tar exec without risking unbounded memory growth.
type limitedBuffer struct {
	buf []byte
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	const maxLen = 4096 // never store more than 4 KiB of stderr
	if len(b.buf) < maxLen {
		remaining := maxLen - len(b.buf)
		if len(p) > remaining {
			p = p[:remaining] // truncate rather than grow past the cap
		}
		b.buf = append(b.buf, p...)
	}
	return len(p), nil // always report success so the stream does not abort
}

func (b *limitedBuffer) String() string { return string(b.buf) }
