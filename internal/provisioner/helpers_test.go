package provisioner

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Test helpers shared by the unit and integration tests.

func toUnstructuredMap(obj runtime.Object) (map[string]any, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
}

func groupOf(m map[string]any) string {
	u := &unstructured.Unstructured{Object: m}
	return u.GroupVersionKind().Group
}

func versionOf(m map[string]any) string {
	u := &unstructured.Unstructured{Object: m}
	return u.GroupVersionKind().Version
}

func kindOf(m map[string]any) string {
	u := &unstructured.Unstructured{Object: m}
	return u.GroupVersionKind().Kind
}

// kubeconfigPath resolves the kubeconfig used by the integration test.
func kubeconfigPath() string {
	if p := os.Getenv("KUBECONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

func integrationSlug() string {
	return fmt.Sprintf("itest-%d", time.Now().Unix()%100000)
}
