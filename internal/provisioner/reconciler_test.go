package provisioner

import (
	"testing"

	"github.com/gobackto-work/pestilence/internal/tenant"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const testSlug = "fuzzy-wombat-x7q3"

func testSpec() tenant.Spec {
	return tenant.Spec{
		Slug:     testSlug,
		OwnerID:  "owner-1",
		Hostname: testSlug + ".gobackto.work",
		HTTPPort: 8000,
	}
}

// fullSpec additionally sets the images, so the Deployments are emitted. The
// values are stand-ins: the real images do not exist yet (scarab §8.6).
func fullSpec() tenant.Spec {
	s := testSpec()
	s.BrokerImage = "ghcr.io/example/scarab-broker:test"
	s.AgentImage = "ghcr.io/example/scarab-agent:test"
	return s
}

// Defect fix: the broker Deployment lives in the platform namespace, so deleting
// the tenant namespace does not remove it. It must be stopped before its
// ServiceAccount is revoked, or the broker could restart holding authority after
// teardown began.
func TestDeletePlanStopsTheBrokerBeforeRevokingItsIdentity(t *testing.T) {
	plan, err := DeletePlan(fullSpec())
	if err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	deployment, serviceAccount := -1, -1
	for i, c := range plan {
		switch obj := c.Object.(type) {
		case *appsv1.Deployment:
			if obj.Name == fullSpec().BrokerServiceName() {
				deployment = i
			}
		case *corev1.ServiceAccount:
			if obj.Name == fullSpec().BrokerServiceAccountName() {
				serviceAccount = i
			}
		}
	}
	if deployment == -1 {
		t.Fatal("broker Deployment is not in the deletion plan; it would survive teardown")
	}
	if serviceAccount == -1 {
		t.Fatal("broker ServiceAccount is not in the deletion plan")
	}
	if deployment > serviceAccount {
		t.Error("broker identity is revoked before the broker is stopped; stop compute first")
	}
}

// The broker Service is both an endpoint and platform-scoped, so a naive plan
// would list it twice.
func TestDeletePlanHasNoDuplicates(t *testing.T) {
	plan, err := DeletePlan(fullSpec())
	if err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range plan {
		key := objectKey(c.Object)
		if seen[key] {
			t.Errorf("%s appears twice in the delete plan", key)
		}
		seen[key] = true
	}
}

// The deletion order is a security property: authority is revoked before compute
// is destroyed (design §31). Asserting the order here means a future refactor
// cannot quietly reverse it.
func TestDeletePlanRevokesAuthorityBeforeCompute(t *testing.T) {
	plan, err := DeletePlan(testSpec())
	if err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("DeletePlan returned nothing to delete")
	}

	// Phase 1: endpoints, so nothing new arrives.
	if plan[0].Class != tenant.ClassEndpoint {
		t.Errorf("first deletion is %s, want an endpoint object (remove the route first)", plan[0].Class)
	}

	// Locate each phase.
	lastEndpoint, firstPlatform, firstRuntime := -1, -1, -1
	for i, c := range plan {
		acc, ok := c.Object.(interface{ GetNamespace() string })
		if !ok {
			t.Fatalf("%T has no namespace accessor", c.Object)
		}
		platform := acc.GetNamespace() == testSpec().Normalized().PlatformNamespace
		switch c.Class {
		case tenant.ClassEndpoint:
			lastEndpoint = i
		case tenant.ClassRuntime:
			if firstRuntime == -1 {
				firstRuntime = i
			}
		}
		if platform && firstPlatform == -1 {
			firstPlatform = i
		}
	}

	if firstPlatform == -1 {
		t.Error("the broker identity is not in the deletion plan; it lives outside the tenant namespace and would survive teardown")
	}
	if firstRuntime != -1 && firstPlatform > firstRuntime {
		t.Error("broker authority is revoked after workloads are destroyed; revoke first")
	}
	if firstRuntime != -1 && lastEndpoint > firstRuntime {
		t.Error("the route is removed after workloads are destroyed; remove the route first")
	}
}

// Deleting the namespace removes everything inside it, so boundary objects in the
// tenant namespace must not be listed individually. Listing them would also mean
// deleting the Namespace twice, in the wrong order.
func TestDeletePlanExcludesTenantNamespaceBoundaryObjects(t *testing.T) {
	tenantNS := tenant.Namespace(testSlug)
	plan, err := DeletePlan(testSpec())
	if err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	for _, c := range plan {
		acc, ok := c.Object.(interface{ GetNamespace() string })
		if !ok {
			t.Fatalf("%T has no namespace accessor", c.Object)
		}
		if acc.GetNamespace() != tenantNS {
			continue // platform-scoped, handled explicitly
		}
		if c.Class == tenant.ClassBoundary {
			t.Errorf("tenant-namespace boundary object %T is in the plan; the namespace deletion covers it", c.Object)
		}
		if _, isNamespace := c.Object.(*corev1.Namespace); isNamespace {
			t.Error("the Namespace itself must not be in the plan; it is deleted separately, last")
		}
	}
}

// The broker's identity must be reachable by the plan, because it is the one
// object that a namespace deletion does NOT clean up.
func TestDeletePlanIncludesTheBrokerIdentity(t *testing.T) {
	plan, err := DeletePlan(testSpec())
	if err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	want := testSpec().BrokerServiceAccountName()
	var found bool
	for _, c := range plan {
		sa, ok := c.Object.(*corev1.ServiceAccount)
		if ok && sa.Name == want {
			found = true
			if sa.Namespace != testSpec().Normalized().PlatformNamespace {
				t.Errorf("broker ServiceAccount namespace = %q, want %q", sa.Namespace, testSpec().Normalized().PlatformNamespace)
			}
		}
	}
	if !found {
		t.Errorf("broker ServiceAccount %q missing from the deletion plan", want)
	}
}

func TestDeletePlanRejectsInvalidSlug(t *testing.T) {
	if _, err := DeletePlan(tenant.Spec{Slug: "../escape"}); err == nil {
		t.Error("DeletePlan accepted an invalid slug")
	}
}

// The signing key is a credential, so it must be in the deletion plan. An object
// missing from the plan survives teardown, which is precisely the residue
// Invariant 9 forbids.
func TestDeletePlanIncludesTheSigningKey(t *testing.T) {
	spec := fullSpec()
	plan, err := DeletePlan(spec)
	if err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	want := spec.TokenKeySecretName()
	for _, c := range plan {
		if s, ok := c.Object.(*corev1.Secret); ok && s.Name == want {
			return
		}
	}
	t.Errorf("signing-key Secret %q is not in the delete plan", want)
}

// resource() guesses a GVR from the object's kind. A wrong guess silently targets
// the wrong resource, so every kind the bundle emits is pinned explicitly here.
// Adding a new kind to the bundle without adding it here fails the test.
func TestKindToResourceMapping(t *testing.T) {
	want := map[schema.GroupVersionKind]string{
		{Group: "", Version: "v1", Kind: "Namespace"}:                            "namespaces",
		{Group: "", Version: "v1", Kind: "ResourceQuota"}:                        "resourcequotas",
		{Group: "", Version: "v1", Kind: "LimitRange"}:                           "limitranges",
		{Group: "", Version: "v1", Kind: "ServiceAccount"}:                       "serviceaccounts",
		{Group: "", Version: "v1", Kind: "Service"}:                              "services",
		{Group: "", Version: "v1", Kind: "Secret"}:                               "secrets",
		{Group: "", Version: "v1", Kind: "ConfigMap"}:                            "configmaps",
		{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"}:                "persistentvolumeclaims",
		{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}:       "networkpolicies",
		{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"}:             "ingresses",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"}:        "roles",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}: "rolebindings",
		{Group: "apps", Version: "v1", Kind: "Deployment"}:                       "deployments",
		{Group: "batch", Version: "v1", Kind: "Job"}:                             "jobs",
	}

	classified, err := tenant.BundleClassified(fullSpec())
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}
	seen := map[schema.GroupVersionKind]bool{}
	for _, c := range classified {
		m, err := toUnstructuredMap(c.Object)
		if err != nil {
			t.Fatalf("convert %T: %v", c.Object, err)
		}
		gvk := schema.GroupVersionKind{
			Group:   groupOf(m),
			Version: versionOf(m),
			Kind:    kindOf(m),
		}
		seen[gvk] = true
		expected, ok := want[gvk]
		if !ok {
			t.Errorf("bundle emits %s but the expected GVR map has no entry for it; add one", gvk)
			continue
		}
		gvr, _ := meta.UnsafeGuessKindToResource(gvk)
		if gvr.Resource != expected {
			t.Errorf("UnsafeGuessKindToResource(%s).Resource = %q, want %q", gvk, gvr.Resource, expected)
		}
	}
	for gvk := range want {
		if !seen[gvk] {
			// Not a failure: the map may cover kinds pestilence never emits. Job is
			// the example -- the broker creates Jobs at runtime within its own
			// namespace-scoped Role, and pestilence never does.
			t.Logf("expected-GVR map lists %s, which the bundle does not emit", gvk)
		}
	}
}

// An object without apiVersion/kind cannot be applied server-side, and failing
// early with a clear message beats an opaque API error.
func TestResourceRequiresTypeMeta(t *testing.T) {
	r := New(nil, "test")
	if _, _, err := r.resource(&corev1.Service{}); err == nil {
		t.Error("resource() accepted an object with no TypeMeta")
	}
}

// The broker's certificate and private key live in a Secret in the platform namespace, and
// DeletePlan is built from a spec that carries NO material -- the controller rebuilds it
// from the stored record, which holds none. So a Secret gated on material would be absent
// from the plan, and the broker's private key would survive teardown unnoticed.
//
// This is the credential-residue trap the token key Secret already guards against, and the
// new Secret has to guard against it the same way: emitted unconditionally.
func TestDeletePlanIncludesTheBrokerTLSSecretEvenWithoutMaterial(t *testing.T) {
	spec := fullSpec()
	if spec.Material.TLSCertPEM != "" || spec.Material.TLSKeyPEM != "" {
		t.Fatal("fullSpec carries TLS material; this test is about the case where it does not")
	}

	plan, err := DeletePlan(spec)
	if err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	for _, c := range plan {
		if s, ok := c.Object.(*corev1.Secret); ok && s.Name == spec.BrokerTLSSecretName() {
			return
		}
	}
	t.Fatalf("Secret %q is not in the deletion plan; the broker's private key would "+
		"survive teardown", spec.BrokerTLSSecretName())
}

// And the certificate belongs to the broker, so it must be ordered with it rather than
// after the tenant namespace is gone.
func TestDeletePlanRemovesTheBrokerTLSSecretFromThePlatformNamespace(t *testing.T) {
	spec := fullSpec()
	plan, err := DeletePlan(spec)
	if err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	for _, c := range plan {
		s, ok := c.Object.(*corev1.Secret)
		if !ok || s.Name != spec.BrokerTLSSecretName() {
			continue
		}
		if s.Namespace != spec.Normalized().PlatformNamespace {
			t.Errorf("broker TLS Secret is in %q, want the platform namespace %q",
				s.Namespace, spec.Normalized().PlatformNamespace)
		}
		return
	}
	t.Fatal("broker TLS Secret is not in the deletion plan")
}
