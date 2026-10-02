package tenant

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func materialBundle(t *testing.T) (Spec, []Classified) {
	t.Helper()
	spec := fullSpec()
	mat, err := NewSigningMaterial(spec, time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewSigningMaterial: %v", err)
	}
	spec.Material = mat
	objs, err := BundleClassified(spec)
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}
	return spec, objs
}

func findSecret(t *testing.T, objs []Classified, ns, name string) *corev1.Secret {
	t.Helper()
	for _, c := range objs {
		if s, ok := c.Object.(*corev1.Secret); ok && s.Namespace == ns && s.Name == name {
			return s
		}
	}
	t.Fatalf("no Secret %s/%s in bundle", ns, name)
	return nil
}

// The three-way split is the security design, not bookkeeping: the private key
// must never be readable from inside a workspace.
func TestTokenMaterialIsSplitAcrossThreeNamespaces(t *testing.T) {
	spec, objs := materialBundle(t)
	n := spec.Normalized()

	key := findSecret(t, objs, n.ControlPlaneNamespace, spec.TokenKeySecretName())
	if _, ok := key.Data[TokenPrivateKeyKey]; !ok {
		t.Error("signing-key Secret does not carry the private key")
	}
	if key.Namespace == Namespace(spec.Slug) {
		t.Fatal("the signing key is in the tenant namespace; any agent could read it")
	}
	if key.Namespace == n.PlatformNamespace {
		t.Error("the signing key should not sit in the broker's namespace either")
	}

	token := findSecret(t, objs, Namespace(spec.Slug), n.TokenSecretName)
	if len(token.Data[TokenDataKey]) == 0 {
		t.Error("root token Secret is empty")
	}
	if _, leaked := token.Data[TokenPrivateKeyKey]; leaked {
		t.Error("the private key must never be mounted into a tenant namespace")
	}

	var pubkey *corev1.ConfigMap
	for _, c := range objs {
		if cm, ok := c.Object.(*corev1.ConfigMap); ok && cm.Name == spec.TokenPubkeyConfigMapName() {
			pubkey = cm
		}
	}
	if pubkey == nil {
		t.Fatal("token public-key ConfigMap missing; the broker refuses to start without it")
	}
	if pubkey.Namespace != n.PlatformNamespace {
		t.Errorf("public-key ConfigMap is in %q, want the broker's namespace %q", pubkey.Namespace, n.PlatformNamespace)
	}
	if pubkey.Data[TokenPublicKeyKey] == "" {
		t.Error("public-key ConfigMap has no key at the path the broker reads")
	}
}

// The broker reads the verification key from a mounted ConfigMap; a path with no
// volume behind it means the broker will not start.
func TestBrokerMountsTheTokenPublicKey(t *testing.T) {
	spec, objs := materialBundle(t)
	_, d := findClassifiedDeployment(t, objs, spec.BrokerServiceName())

	var mounted bool
	for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "token-pubkey" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatal("broker does not mount token-pubkey")
	}
	var sourced bool
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "token-pubkey" && v.ConfigMap != nil && v.ConfigMap.Name == spec.TokenPubkeyConfigMapName() {
			sourced = true
		}
	}
	if !sourced {
		t.Fatalf("token-pubkey volume is not sourced from %s", spec.TokenPubkeyConfigMapName())
	}

	env := d.Spec.Template.Spec.Containers[0].Env
	got := envValue(t, env, "SCARAB_TOKEN_PUBLIC_KEY")
	if got != "/var/run/scarab/token-pubkey/"+TokenPublicKeyKey {
		t.Errorf("SCARAB_TOKEN_PUBLIC_KEY = %q, which must match the mount path", got)
	}
	// SCARAB_MODEL_SECRET is not set at all: the model key is supplied in the
	// session and persisted to the workspace volume, so the broker needs no Secret
	// name to build worker PodSpecs.
	if _, ok := envValueOK(env, "SCARAB_MODEL_SECRET"); ok {
		t.Error("SCARAB_MODEL_SECRET is set; the platform injects no model credentials")
	}
}

// The model key is supplied by the user in the session and persisted to the
// workspace volume, which workers share (§8.4, §8.10). So NO credential reference
// should appear in a rendered manifest: no envFrom, no env var, no Secret.
//
// This guards against a second mechanism being reintroduced without the contract
// changing. There was one -- an owner-supplied Secret via envFrom -- and it was
// deleted as a third way to do something the session key already does.
func TestNoModelCredentialReferenceInTheBundle(t *testing.T) {
	for _, c := range fullBundle(t) {
		d, ok := c.Object.(*appsv1.Deployment)
		if !ok {
			continue
		}
		for _, ct := range d.Spec.Template.Spec.Containers {
			if len(ct.EnvFrom) != 0 {
				t.Errorf("%s/%s has an envFrom; the platform injects no model credentials", d.Name, ct.Name)
			}
			if _, ok := envValueOK(ct.Env, "SCARAB_MODEL_SECRET"); ok {
				t.Errorf("%s/%s sets SCARAB_MODEL_SECRET", d.Name, ct.Name)
			}
		}
	}
}

// The signing key is a credential, so it must be in the deletion plan. That
// assertion lives in the provisioner package, where DeletePlan is defined.
