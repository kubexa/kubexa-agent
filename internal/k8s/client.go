package k8s

import (
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ─────────────────────────────────────────
// Client
// ─────────────────────────────────────────

// Client kubernetes clientset wrapper
type Client struct {
	Clientset kubernetes.Interface
	Config    *rest.Config
}

// NewClient in-cluster or local kubeconfig'e göre
// automatically create appropriate client.
//
// Priority order:
//  1. In-cluster config (production, running in pod)
//  2. KUBECONFIG env variable
//  3. $HOME/.kube/config (local development)
func NewClient() (*Client, error) {
	cfg, mode, err := buildConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes config creation failed: %w", err)
	}

	// rate limiter settings — prevent high event throughput
	cfg.QPS = 50
	cfg.Burst = 100

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes clientset creation failed: %w", err)
	}

	// verify connection
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("kubernetes api access failed: %w", err)
	}

	log.Info().
		Str("mode", mode).
		Str("server_version", version.GitVersion).
		Msg("kubernetes client ready")

	return &Client{
		Clientset: clientset,
		Config:    cfg,
	}, nil
}

// ─────────────────────────────────────────
// Config Builder
// ─────────────────────────────────────────

func buildConfig() (*rest.Config, string, error) {
	// 1. in-cluster (production, running in pod)
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, "in-cluster", nil
	}

	// 2. KUBECONFIG environment variable
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, "", fmt.Errorf("KUBECONFIG not found (%s): %w", kubeconfig, err)
		}
		return cfg, "kubeconfig-env", nil
	}

	// 3. default kubeconfig path ($HOME/.kube/config)
	defaultPath := clientcmd.RecommendedHomeFile // $HOME/.kube/config
	cfg, err := clientcmd.BuildConfigFromFlags("", defaultPath)
	if err != nil {
		return nil, "", fmt.Errorf(
			"kubernetes config not found (in-cluster, KUBECONFIG, %s): %w",
			defaultPath, err,
		)
	}

	return cfg, "kubeconfig-default", nil
}
