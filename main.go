// main.go — entry point for replic2.
//
// This file wires together the packages in internal/ and starts three
// long-running components:
//
//  1. HTTP server  — always active, serves liveness / readiness probes.
//  2. Leader election loop — only the elected pod runs the controllers.
//  3. Graceful shutdown — SIGTERM / SIGINT drains the HTTP server cleanly.
//
// All business logic lives in the internal/ sub-packages; this file contains
// no logic of its own.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"replic2/internal/controller/backup"
	"replic2/internal/controller/restore"
	"replic2/internal/controller/scheduled"
	"replic2/internal/k8s"
	"replic2/internal/leader"
	"replic2/internal/server"
)

func main() {
	// Root context — cancelled on SIGTERM / SIGINT.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// ---- Kubernetes clients ----
	clients, err := k8s.New()
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}

	// ---- HTTP server ----
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	gin.SetMode(gin.ReleaseMode)

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%s", port),
		Handler: server.NewRouter(clients, time.Now()),
	}

	go func() {
		log.Printf("replic2 listening on :%s", port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	// ---- Controllers (leader-elected) ----
	// The leader runs all three controllers concurrently.
	// Standby pods block inside leader.Run but keep the HTTP server active.
	ns := podNamespace()
	go leader.Run(ctx, clients, ns, func(leaderCtx context.Context) {
		go backup.Run(leaderCtx, clients)
		go restore.Run(leaderCtx, clients)
		go scheduled.Run(leaderCtx, clients)
		<-leaderCtx.Done()
	})

	// ---- Graceful shutdown ----
	<-ctx.Done()
	log.Println("replic2: shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
}

// podNamespace returns the namespace this pod is running in.
// Reads POD_NAMESPACE (set via the Downward API) or defaults to "default".
func podNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}
