// pvcdata.go — raw PVC data backup via a self-terminating agent pod.
//
// When spec.includePVCData is true, the backup controller cannot mount the
// source PVC directly (RWO volumes allow only one writer at a time and the
// source PVC may already be mounted on a different node).  Instead it:
//
//  1. Spawns a short-lived "agent" pod in the source namespace that mounts
//     the source PVC read-only.  The pod's command tars the PVC contents and
//     pipes the archive bytes directly to S3 using the AWS CLI.
//     No backup PVC is involved at all.
//
//  2. Waits for the pod to reach Succeeded or Failed by polling its phase.
//     The pod terminates itself as soon as the AWS CLI exits.
//
//  3. Deletes the pod via defer — by the time the defer runs the pod is
//     already in the Succeeded phase, so the delete just removes a completed
//     pod rather than killing a live one.
//
// S3 key layout:
//
//   - Full backup:        <keyPrefix>/pvc-data/<pvcName>.tar
//   - Incremental backup: <keyPrefix>/pvc-data/<pvcName>-incremental.tar
//
// For incremental backups, a shell script uses `find -newer` to list only
// files changed since the previous backup's completedAt timestamp, then pipes
// that file list into tar via `-T`.
package backup

import (
	"context" // for cancellation / deadlines
	"fmt"     // for error wrapping
	"log"     // for structured logging
	"os"      // for reading S3 env vars to pass to the agent pod
	"time"    // for sinceTime, agentPodTimeout, RFC3339 format

	corev1 "k8s.io/api/core/v1"                    // Pod, PVC types and phase constants
	k8serrors "k8s.io/apimachinery/pkg/api/errors" // IsNotFound
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"  // ObjectMeta, ListOptions, DeleteOptions

	"replic2/internal/k8s"            // Kubernetes client wrapper (c.Core)
	apitypes "replic2/internal/types" // Backup struct (b.Name for logging/labels)
)

// backupPVCData iterates over every PVC in namespace ns and calls
// backupSinglePVC for each one that is in the Bound phase.
//
// keyPrefix is the S3 key prefix for this backup (e.g. "my-app/my-backup-01").
// sinceTime controls incremental vs full:
//   - zero value  → full backup (all files included)
//   - non-zero    → incremental (only files with mtime > sinceTime)
func backupPVCData(ctx context.Context, c *k8s.Clients, b *apitypes.Backup, ns, keyPrefix string, sinceTime time.Time) error {
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

	for _, pvc := range pvcList.Items {
		// Only back up PVCs that are actively bound to a volume.
		if pvc.Status.Phase != corev1.ClaimBound {
			log.Printf("backup controller: [%s] PVC %q is not Bound (phase=%s) — skipping", b.Name, pvc.Name, pvc.Status.Phase)
			continue
		}
		if err := backupSinglePVC(ctx, c, b, ns, keyPrefix, pvc.Name, sinceTime); err != nil {
			// One failing PVC should not abort the rest — log and continue.
			log.Printf("backup controller: [%s] PVC %q data backup error: %v", b.Name, pvc.Name, err)
		}
	}
	return nil
}

// backupSinglePVC spawns an agent pod that mounts the source PVC read-only and
// runs a single command that tars the contents and pipes them directly to S3
// via the AWS CLI (`tar ... | aws s3 cp - s3://...`).
//
// The pod exits as soon as the pipeline completes.  The controller polls until
// the pod reaches Succeeded or Failed, then deletes it.
func backupSinglePVC(ctx context.Context, c *k8s.Clients, b *apitypes.Backup, ns, keyPrefix, pvcName string, sinceTime time.Time) error {
	// -----------------------------------------------------------------------
	// 1. Build the S3 destination key and the shell command.
	//    The agent runs a single sh -c "..." invocation that:
	//    a. Builds a tar archive of /data (full or incremental).
	//    b. Pipes it directly to `aws s3 cp - s3://<bucket>/<key>`.
	//    The bucket and credentials come from env vars on the pod.
	// -----------------------------------------------------------------------
	bucket := os.Getenv("S3_BUCKET") // same bucket that the controller uses

	archiveName := pvcName + ".tar" // full backup
	if !sinceTime.IsZero() {
		archiveName = pvcName + "-incremental.tar" // incremental backup
	}

	// S3 destination key: <keyPrefix>/pvc-data/<pvcName>.tar
	s3Key := fmt.Sprintf("%s/pvc-data/%s", keyPrefix, archiveName)
	s3URI := fmt.Sprintf("s3://%s/%s", bucket, s3Key) // full s3:// URI for the AWS CLI

	// Build the optional --endpoint-url flag for MinIO / non-AWS S3 providers.
	// This must be part of the aws command string, not appended outside the
	// sh -c "..." quoted expression.
	endpointFlag := ""
	if endpoint := os.Getenv("S3_ENDPOINT"); endpoint != "" {
		endpointFlag = fmt.Sprintf(" --endpoint-url '%s'", endpoint) // leading space intentional
	}

	// Build the shell command.  For incremental backups we use find -newer to
	// limit the archive to files modified after sinceTime.
	// The amazon/aws-cli image (Amazon Linux 2023) does not include tar by
	// default, so we install it first with a quiet yum call.  The install is
	// fast (~1 s) because the package is tiny and the Amazon Linux repo is
	// always reachable inside any cloud/kind cluster.
	var shellCmd string
	if sinceTime.IsZero() {
		// Full backup: archive everything under /data.
		shellCmd = fmt.Sprintf(
			`yum install -y -q tar && tar -c -C /data . | aws s3 cp - '%s'%s`,
			s3URI, endpointFlag,
		)
	} else {
		// Incremental backup:
		//   1. Create a reference file with the cutoff timestamp.
		//   2. Find all files newer than the reference file.
		//   3. Pipe that file list into tar, then upload to S3.
		ts := sinceTime.UTC().Format("2006-01-02 15:04:05") // touch -d format
		shellCmd = fmt.Sprintf(
			`yum install -y -q tar && touch -d '%s' /tmp/ref && find /data -newer /tmp/ref > /tmp/files && tar -c -C /data -T /tmp/files | aws s3 cp - '%s'%s`,
			ts, s3URI, endpointFlag,
		)
	}

	// -----------------------------------------------------------------------
	// 2. Build the pod name; truncate to the 63-char Kubernetes DNS label limit.
	// -----------------------------------------------------------------------
	podName := fmt.Sprintf("replic2-backup-%s-%s", b.Name, pvcName)
	if len(podName) > 63 {
		podName = podName[:63]
	}

	// -----------------------------------------------------------------------
	// 3. Collect S3 env vars to inject into the agent pod.
	//    The AWS CLI inside the pod reads credentials from standard env vars.
	// -----------------------------------------------------------------------
	agentEnv := []corev1.EnvVar{
		// AWS credentials and region — standard AWS CLI env vars.
		{Name: "AWS_ACCESS_KEY_ID", Value: os.Getenv("S3_ACCESS_KEY_ID")},
		{Name: "AWS_SECRET_ACCESS_KEY", Value: os.Getenv("S3_SECRET_ACCESS_KEY")},
		{Name: "AWS_DEFAULT_REGION", Value: os.Getenv("S3_REGION")},
		// S3_BUCKET is needed only if the shell script computed s3URI incorrectly,
		// but we keep it for completeness / debugging inside the pod.
		{Name: "S3_BUCKET", Value: bucket},
	}

	// -----------------------------------------------------------------------
	// 4. Define the agent pod.
	//    - Uses the amazon/aws-cli image which bundles both tar and the AWS CLI.
	//    - Mounts the source PVC read-only at /data.
	//    - Runs a single sh -c command; exits when done.
	//    - S3 credentials are passed via env vars — never written to disk.
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
					Image:   "amazon/aws-cli",               // provides both aws CLI and a POSIX shell
					Command: []string{"sh", "-c", shellCmd}, // runs as PID 1; exits when done
					Env:     agentEnv,                       // S3 credentials
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

	// Delete any leftover pod from a previous (failed) attempt so Create succeeds,
	// then wait until the API server confirms it is gone before creating a new one.
	if err := deleteAndWaitForPodGone(ctx, c, ns, podName); err != nil {
		return fmt.Errorf("cleanup stale agent pod %q: %w", podName, err)
	}

	if _, err := c.Core.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create agent pod for PVC %q: %w", pvcName, err)
	}
	log.Printf("backup controller: [%s] agent pod %q created for PVC %q", b.Name, podName, pvcName)

	// Always delete the pod when this function returns, regardless of outcome.
	defer func() {
		_ = c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})
		log.Printf("backup controller: [%s] agent pod %q deleted", b.Name, podName)
	}()

	// -----------------------------------------------------------------------
	// 5. Poll until the pod reaches a terminal phase (Succeeded or Failed).
	//    The pod exits as soon as the tar | aws s3 cp pipeline completes.
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
			// Pipeline finished successfully — archive is in S3.
			log.Printf("backup controller: [%s] agent pod %q succeeded — PVC %q archived to %s", b.Name, podName, pvcName, s3URI)
			return nil // success path
		case corev1.PodFailed:
			// aws s3 cp or tar exited non-zero.
			return fmt.Errorf("agent pod %q failed — check pod logs for error", podName)
		default:
			// Still Pending or Running — wait and poll again.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second): // poll interval
			}
		}
	}
}

// deleteAndWaitForPodGone deletes the named pod (ignoring "not found") and then
// polls until the API server confirms it no longer exists.  This prevents a race
// where Create is called before the terminating pod has been fully removed.
func deleteAndWaitForPodGone(ctx context.Context, c *k8s.Clients, ns, podName string) error {
	// Issue the delete; ignore "not found" — pod may already be absent.
	err := c.Core.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("delete pod: %w", err)
	}

	// Poll until the pod disappears from the API server.
	deadline := time.Now().Add(30 * time.Second) // generous but bounded
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("pod %q still exists after 30s", podName)
		}
		_, err := c.Core.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil && k8serrors.IsNotFound(err) {
			// Pod is gone — safe to create a new one.
			return nil
		}
		// Still present (or transient API error) — wait and retry.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second): // poll every 2 s
		}
	}
}
