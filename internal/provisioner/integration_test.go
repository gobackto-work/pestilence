package provisioner

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gobackto-work/pestilence/internal/tenant"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"

	"github.com/golang-jwt/jwt/v5"
)

// TestIntegrationEnsureAndDelete exercises the reconciler against a real cluster.
//
// It is skipped unless PESTILENCE_INTEGRATION=1, so `go test ./...` stays
// hermetic. Run it with:
//
//	PESTILENCE_INTEGRATION=1 go test ./internal/provisioner -run Integration -v
//
// It creates a real namespace, real PVCs and real network policies, then removes
// them. That is the point: the unit tests assert the objects are shaped
// correctly, and this asserts the API server accepts them and that teardown
// leaves nothing behind.
func TestIntegrationEnsureAndDelete(t *testing.T) {
	if os.Getenv("PESTILENCE_INTEGRATION") == "" {
		t.Skip("set PESTILENCE_INTEGRATION=1 to run against a live cluster")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath())
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}

	ctx := context.Background()
	r := New(dyn, "pestilence-itest")

	spec := tenant.Spec{
		Slug:     integrationSlug(),
		OwnerID:  "integration-test",
		HTTPPort: 8000,
		// Stand-in refs so the Deployments are emitted, and therefore checked by
		// admission. They can never pull, which does not matter: the point is that
		// the API server ACCEPTS the pod templates. A rejected pod never appears,
		// so waiting for one is a real test of PSA and the tenant VAP.
		BrokerImage: "example.invalid/scarab-broker:itest",
		AgentImage:  "example.invalid/scarab-agent:itest",
	}
	platform := spec.BrokerServiceAccountName()

	// The broker lives in the platform namespace, which is cluster-level
	// infrastructure declared in my-opps. REQUIRE it rather than creating it: a
	// test that silently created it would produce an unlabelled namespace with no
	// Pod Security Admission enforcement, quietly weakening the boundary it is
	// supposed to be checking.
	if err := requirePlatformNamespace(ctx, dyn, "scarab"); err != nil {
		t.Fatalf("%v\n\nCreate it with: kubectl apply -f my-opps/cluster/manifests/namespace-scarab.yaml", err)
	}

	t.Cleanup(func() {
		// Best effort, so a failed assertion does not leak a namespace.
		_ = r.Delete(context.Background(), spec)
	})

	t.Logf("workspace slug: %s", spec.Slug)

	// --- provision -------------------------------------------------------
	if err := r.Ensure(ctx, spec); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Every reconciled object should now exist.
	missing, err := r.Missing(ctx, spec)
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("objects missing after Ensure: %v", missing)
	}

	// --- idempotency -----------------------------------------------------
	tokenAfterFirst := readSecretKey(t, ctx, dyn, tenant.Namespace(spec.Slug), spec.Normalized().TokenSecretName, tenant.TokenDataKey)
	if tokenAfterFirst == "" {
		t.Fatal("root token Secret is empty")
	}

	if err := r.Ensure(ctx, spec); err != nil {
		t.Fatalf("second Ensure should be a no-op, got: %v", err)
	}

	// The signing material is created ONCE. A regenerated key would change the
	// public key the broker verifies with, silently invalidating the token the
	// running root agent holds.
	if got := readSecretKey(t, ctx, dyn, tenant.Namespace(spec.Slug), spec.Normalized().TokenSecretName, tenant.TokenDataKey); got != tokenAfterFirst {
		t.Error("the capability token changed on a second reconcile; the signing material is not being reused")
	}
	if got := readSecretKey(t, ctx, dyn, spec.Normalized().ControlPlaneNamespace, spec.Normalized().TokenKeySecretName(), tenant.TokenPrivateKeyKey); got == "" {
		t.Error("signing key Secret is missing from the control plane namespace")
	}
	// The public key the broker reads must match the token that was minted. This is
	// the end-to-end assertion that what pestilence mints and publishes is what a
	// verifier can actually consume.
	pubPEM := readConfigMapKey(t, ctx, dyn, spec.Normalized().PlatformNamespace, spec.Normalized().TokenPubkeyConfigMapName(), tenant.TokenPublicKeyKey)
	if pubPEM == "" {
		t.Fatal("token public-key ConfigMap is empty; the broker would refuse to start")
	}
	verifyToken(t, tokenAfterFirst, parsePublicKey(t, pubPEM))

	// --- the boundary is actually enforced -------------------------------
	ns, err := dyn.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}).
		Get(ctx, tenant.Namespace(spec.Slug), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if got := ns.GetLabels()["pod-security.kubernetes.io/enforce"]; got != "restricted" {
		t.Errorf("namespace PSA enforce label = %q, want restricted", got)
	}

	// The broker identity must exist OUTSIDE the tenant namespace.
	saGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}
	if _, err := dyn.Resource(saGVR).Namespace("scarab").Get(ctx, platform, metav1.GetOptions{}); err != nil {
		t.Errorf("broker ServiceAccount should exist in the platform namespace: %v", err)
	}

	// --- the pod templates are actually admissible -----------------------
	// Both Deployments run under PSA restricted, and the tenant one also under the
	// ValidatingAdmissionPolicy. Neither is exercised anywhere else, so this is the
	// only check that the templates pestilence emits would really be accepted.
	// A rejected pod never appears, so waiting for one is the test.
	for _, d := range []struct{ ns, name string }{
		{tenant.Namespace(spec.Slug), tenant.NameRootDeployment},
		{spec.Normalized().PlatformNamespace, spec.BrokerServiceName()},
	} {
		if err := waitForPodOf(ctx, dyn, d.ns, d.name); err != nil {
			t.Errorf("no pod appeared for Deployment/%s in %s, so admission rejected its template: %v", d.name, d.ns, err)
		}
	}

	// --- teardown --------------------------------------------------------
	if err := r.Delete(ctx, spec); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.WaitForDeletion(ctx, spec, 5*time.Minute); err != nil {
		t.Fatalf("WaitForDeletion: %v", err)
	}

	// Nothing may survive, including the object a namespace deletion misses.
	if _, err := dyn.Resource(saGVR).Namespace("scarab").Get(ctx, platform, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("broker ServiceAccount survived teardown (err=%v)", err)
	}
	if _, err := dyn.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}).
		Get(ctx, tenant.Namespace(spec.Slug), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("tenant namespace survived teardown (err=%v)", err)
	}
	// A signing key that outlives its workspace is exactly the residue the
	// teardown invariant forbids: it lives outside the tenant namespace, so the
	// namespace deletion does not remove it.
	if got := readSecretKey(t, ctx, dyn, spec.Normalized().ControlPlaneNamespace, spec.Normalized().TokenKeySecretName(), tenant.TokenPrivateKeyKey); got != "" {
		t.Error("the workspace signing key survived teardown")
	}
}

func readSecretKey(t *testing.T, ctx context.Context, dyn dynamic.Interface, ns, name, key string) string {
	t.Helper()
	u, err := dyn.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}).
		Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("get secret %s/%s: %v", ns, name, err)
	}
	var secret corev1.Secret
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &secret); err != nil {
		t.Fatalf("decode secret %s/%s: %v", ns, name, err)
	}
	return string(secret.Data[key])
}

func readConfigMapKey(t *testing.T, ctx context.Context, dyn dynamic.Interface, ns, name, key string) string {
	t.Helper()
	u, err := dyn.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}).
		Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap %s/%s: %v", ns, name, err)
	}
	var cm corev1.ConfigMap
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &cm); err != nil {
		t.Fatalf("decode configmap %s/%s: %v", ns, name, err)
	}
	return cm.Data[key]
}

// parsePublicKey decodes the PKIX PEM the ConfigMap carries.
//
// Local rather than exported from the tenant package: the control plane never
// parses a public key -- the broker does, in another repository -- so as
// production code it was unreachable, and the gate said so.
func parsePublicKey(t *testing.T, encoded string) ed25519.PublicKey {
	t.Helper()
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		t.Fatal("public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("public key is %T, want ed25519.PublicKey", key)
	}
	return pub
}

// verifyToken checks a token against the public key, pinning EdDSA exactly as the
// broker does. If this passes and the broker rejects the token, the two sides have
// diverged on the wire format.
func verifyToken(t *testing.T, token string, pub ed25519.PublicKey) {
	t.Helper()
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) {
		return pub, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}))
	if err != nil {
		t.Errorf("the token does not verify against the published public key: %v", err)
		return
	}
	if got, _ := claims["workspace"].(string); got == "" {
		t.Error("a verified token carries no workspace claim")
	}
}

// waitForPodOf waits until a pod owned by the named Deployment exists. The pod
// does not have to run: it will sit in ImagePullBackOff, because the image refs
// are deliberately unresolvable. What matters is that it was ADMITTED, which is
// the only thing standing between a bad PodSpec and a silent runtime failure.
func waitForPodOf(ctx context.Context, dyn dynamic.Interface, ns, deployment string) error {
	pods := dyn.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}).Namespace(ns)
	deadline := time.Now().Add(60 * time.Second)
	for {
		list, err := pods.List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		for _, item := range list.Items {
			for _, ref := range item.GetOwnerReferences() {
				if ref.Kind == "ReplicaSet" && strings.HasPrefix(ref.Name, deployment+"-") {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after 60s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func requirePlatformNamespace(ctx context.Context, dyn dynamic.Interface, name string) error {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	if _, err := dyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("platform namespace %q is required: %w", name, err)
	}
	return nil
}
