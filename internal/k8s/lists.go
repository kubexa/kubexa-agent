package k8s

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ListNamespaceNames returns all namespace names (sorted), cluster-scoped LIST permission required.
func ListNamespaceNames(ctx context.Context, c kubernetes.Interface) ([]string, error) {
	list, err := c.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	sort.Strings(names)
	return names, nil
}

// ListPodsInNamespace returns pods in a namespace; labelSelector uses Kubernetes label query syntax.
func ListPodsInNamespace(ctx context.Context, c kubernetes.Interface, namespace, labelSelector string) ([]corev1.Pod, error) {
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	list, err := c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
