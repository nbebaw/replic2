package main

// client.go — Kubernetes client initialisation.
//
// Tries in-cluster config first (running inside a Pod), then falls back to
// the kubeconfig file (local development with kubectl).

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// clients groups all Kubernetes client types used by the controllers.
type clients struct {
	// core is the typed client for built-in Kubernetes resources.
	core kubernetes.Interface
	// dynamic is used to GET/PATCH status subresources on our CRDs and to
	// list/apply arbitrary resource types during backup/restore.
	dynamic dynamic.Interface
	// discovery is used to enumerate all API groups and resources installed
	// in the cluster, including third-party CRDs.
	discovery discovery.DiscoveryInterface
	// rest is the bare *rest.Config; used to build CRD-specific REST clients.
	rest *rest.Config
	// codec is used to decode raw JSON into our typed CRD objects.
	codec runtime.Codec
}

// newClients builds a clients instance from the ambient kubeconfig or
// in-cluster service-account credentials.
func newClients() (*clients, error) {
	cfg, err := loadRestConfig()
	if err != nil {
		return nil, fmt.Errorf("build rest config: %w", err)
	}

	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build core client: %w", err)
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}

	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build discovery client: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := addToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register CRD scheme: %w", err)
	}
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(SchemeGroupVersion)

	return &clients{
		core:      core,
		dynamic:   dyn,
		discovery: disc,
		rest:      cfg,
		codec:     codec,
	}, nil
}

// loadRestConfig returns an in-cluster config when running inside a Pod,
// otherwise it reads the default kubeconfig file (for local dev).
func loadRestConfig() (*rest.Config, error) {
	// In-cluster (Pod environment).
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	// Local dev — honour KUBECONFIG env, then fall back to ~/.kube/config.
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("find home dir: %w", err)
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %q: %w", kubeconfig, err)
	}
	return cfg, nil
}
