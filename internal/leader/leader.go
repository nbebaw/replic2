// Package leader implements Lease-based leader election using client-go.
//
// Only the elected leader runs the backup and restore controllers.  Standby
// pods keep the HTTP server alive and are ready to take over if the leader
// crashes, preventing duplicate backup/restore operations when the Deployment
// runs multiple replicas.
//
// The Lease object is stored in the same namespace as the replic2 Pod.
// The holder identity defaults to the pod name (POD_NAME env var) so that
// "kubectl get lease" shows which pod is currently elected.
package leader

import (
	"context"
	"log"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"replic2/internal/k8s"
)

const (
	leaseName     = "replic2-leader"
	leaseDuration = 15 * time.Second
	leaseRenew    = 10 * time.Second
	leaseRetry    = 2 * time.Second
)

// Run wraps fn in a leader-election loop.
//
// fn is called with a leaderCtx that is cancelled when leadership is lost.
// Run blocks until ctx is cancelled.  Standby pods sit idle inside Run,
// but the HTTP server continues serving in a separate goroutine.
func Run(ctx context.Context, c *k8s.Clients, ns string, fn func(leaderCtx context.Context)) {
	id := identity()

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: ns,
		},
		Client: c.Core.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   leaseRenew,
		RetryPeriod:     leaseRetry,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				log.Printf("leader election: %q became leader — starting controllers", id)
				fn(leaderCtx)
			},
			OnStoppedLeading: func() {
				log.Printf("leader election: %q lost leadership", id)
			},
			OnNewLeader: func(identity string) {
				if identity != id {
					log.Printf("leader election: current leader is %q", identity)
				}
			},
		},
	})
}

// identity returns a unique name for this instance.
// Uses POD_NAME (set via Downward API) or falls back to the OS hostname.
func identity() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
