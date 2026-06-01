package k8s

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// PodClient provides list and watch operations for pods in a single namespace.
type PodClient interface {
	// List returns pods in the namespace matching the given list options.
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error)

	// Watch returns a watch.Interface for pods in the namespace.
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
}

// podClient implements PodClient for one namespace.
type podClient struct {
	client    *client
	namespace string
}

// List returns pods in the namespace.
func (p *podClient) List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error) {
	var list *corev1.PodList
	err := p.client.withMetrics(ctx, "list", "pods", func(ctx context.Context) error {
		var listErr error
		list, listErr = p.client.kube.CoreV1().Pods(p.namespace).List(ctx, opts)
		return listErr
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// Watch returns a watch.Interface for pods in the namespace.
func (p *podClient) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	var watcher watch.Interface
	err := p.client.withMetrics(ctx, "watch", "pods", func(ctx context.Context) error {
		var watchErr error
		watcher, watchErr = p.client.kube.CoreV1().Pods(p.namespace).Watch(ctx, opts)
		return watchErr
	})
	if err != nil {
		return nil, err
	}
	return watcher, nil
}
