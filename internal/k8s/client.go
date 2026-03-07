// Package k8s initialises all Kubernetes clients needed by replic2.
//
// It tries in-cluster credentials first (running inside a Pod) and falls back
// to the kubeconfig file for local development.
//
// The Clients struct is the single value passed to every controller and HTTP
// handler that needs to talk to the API server.
package k8s

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

	"replic2/internal/types"
)

// Clients groups every Kubernetes client type used by replic2.
type Clients struct {
	// Core is the typed client for built-in Kubernetes resources
	// (namespaces, leases, etc.).
	Core kubernetes.Interface

	// Dynamic is used to GET/PATCH CRD status sub-resources and to
	// list/apply arbitrary resource types during backup and restore.
	Dynamic dynamic.Interface

	// Discovery is used to enumerate all API groups installed in the
	// cluster so the backup controller can discover third-party CRDs.
	Discovery discovery.DiscoveryInterface

	// REST is the raw *rest.Config; kept for building CRD-specific clients
	// if needed in the future.
	REST *rest.Config

	// Codec decodes raw API-server JSON into our typed CRD objects.
	Codec runtime.Codec
}

// New builds a Clients instance from the ambient kubeconfig or in-cluster
// service-account credentials.
func New() (*Clients, error) {
	cfg, err := loadRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("build REST config: %w", err)
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
	if err := types.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register CRD scheme: %w", err)
	}
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(types.SchemeGroupVersion)

	return &Clients{
		Core:      core,
		Dynamic:   dyn,
		Discovery: disc,
		REST:      cfg,
		Codec:     codec,
	}, nil
}

// loadRESTConfig returns an in-cluster config when running inside a Pod,
// otherwise reads the kubeconfig file for local development.
func loadRESTConfig() (*rest.Config, error) {
	// In-cluster — works automatically inside a Pod.
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	// Local dev — honour $KUBECONFIG, then fall back to ~/.kube/config.
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
