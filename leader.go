package main

// leader.go — Lease-based leader election using client-go.
//
// Only the elected leader runs the backup and restore controllers; the standby
// pod remains ready to take over if the leader crashes.  This prevents
// duplicate backup/restore operations when the Deployment runs multiple
// replicas.
//
// The Lease resource is stored in the same namespace as the replic2 Pod.
// The holder identity defaults to the Pod name (from the POD_NAME env var) so
// that kubectl get lease shows which pod is currently the leader.

import (
	"context"
	"log"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	leaseName     = "replic2-leader"
	leaseDuration = 15 * time.Second
	leaseRenew    = 10 * time.Second
	leaseRetry    = 2 * time.Second
)

// runWithLeaderElection wraps fn inside a leader-election loop.
// fn is called when this instance becomes the leader and is cancelled when
// leadership is lost.  runWithLeaderElection itself returns only when ctx is
// cancelled.
func runWithLeaderElection(ctx context.Context, c *clients, fn func(ctx context.Context)) {
	id := leaderID()
	ns := namespace()

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: ns,
		},
		Client: c.core.CoordinationV1(),
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

// leaderID returns a unique identifier for this instance.
// Uses POD_NAME (set via the Downward API in deployment.yaml) or falls back
// to the OS hostname.
func leaderID() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	return hostname()
}
