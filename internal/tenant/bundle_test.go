package tenant

import (
	"net"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const testSlug = "fuzzy-wombat-x7q3"

func testSpec() Spec {
	return Spec{
		Slug:     testSlug,
		OwnerID:  "owner-1",
		Hostname: testSlug + ".gobackto.work",
		HTTPPort: 8000,
	}
}

func testBundle(t *testing.T) []runtime.Object {
	t.Helper()
	classified, err := BundleClassified(testSpec())
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}
	if len(classified) == 0 {
		t.Fatal("BundleClassified returned no objects")
	}
	objs := make([]runtime.Object, 0, len(classified))
	for _, c := range classified {
		objs = append(objs, c.Object)
	}
	return objs
}

func findNamespace(t *testing.T, objs []runtime.Object, name string) *corev1.Namespace {
	t.Helper()
	for _, obj := range objs {
		if v, ok := obj.(*corev1.Namespace); ok && v.Name == name {
			return v
		}
	}
	t.Fatalf("no Namespace named %q in bundle", name)
	return nil
}

func findNetpol(t *testing.T, objs []runtime.Object, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	for _, obj := range objs {
		if v, ok := obj.(*networkingv1.NetworkPolicy); ok && v.Name == name {
			return v
		}
	}
	t.Fatalf("no NetworkPolicy named %q in bundle", name)
	return nil
}

func findRole(t *testing.T, objs []runtime.Object, name string) *rbacv1.Role {
	t.Helper()
	for _, obj := range objs {
		if v, ok := obj.(*rbacv1.Role); ok && v.Name == name {
			return v
		}
	}
	t.Fatalf("no Role named %q in bundle", name)
	return nil
}

func findRoleBinding(t *testing.T, objs []runtime.Object, name string) *rbacv1.RoleBinding {
	t.Helper()
	for _, obj := range objs {
		if v, ok := obj.(*rbacv1.RoleBinding); ok && v.Name == name {
			return v
		}
	}
	t.Fatalf("no RoleBinding named %q in bundle", name)
	return nil
}

func findQuota(t *testing.T, objs []runtime.Object, name string) *corev1.ResourceQuota {
	t.Helper()
	for _, obj := range objs {
		if v, ok := obj.(*corev1.ResourceQuota); ok && v.Name == name {
			return v
		}
	}
	t.Fatalf("no ResourceQuota named %q in bundle", name)
	return nil
}

func findLimitRange(t *testing.T, objs []runtime.Object, name string) *corev1.LimitRange {
	t.Helper()
	for _, obj := range objs {
		if v, ok := obj.(*corev1.LimitRange); ok && v.Name == name {
			return v
		}
	}
	t.Fatalf("no LimitRange named %q in bundle", name)
	return nil
}

// Invariant: an invalid slug must never reach the provisioner, because it is
// interpolated into a namespace name and a public hostname.
func TestBundleRejectsInvalidSlug(t *testing.T) {
	for _, bad := range []string{"", "Upper", "-lead", "trail-", "has space", "a/b", "..", strings.Repeat("x", 41)} {
		if _, err := BundleClassified(Spec{Slug: bad}); err == nil {
			t.Errorf("Bundle accepted invalid slug %q", bad)
		}
	}
}

// Invariant 6 (part): Pod Security Admission restricted is enforced by label on
// the namespace, not merely requested of workloads.
func TestNamespaceEnforcesRestrictedPSA(t *testing.T) {
	ns := findNamespace(t, testBundle(t), Namespace(testSlug))
	for _, key := range []string{
		"pod-security.kubernetes.io/enforce",
		"pod-security.kubernetes.io/audit",
		"pod-security.kubernetes.io/warn",
	} {
		if got := ns.Labels[key]; got != "restricted" {
			t.Errorf("namespace label %s = %q, want %q", key, got, "restricted")
		}
	}
}

// Invariant 5: workers receive no Kubernetes token. Also applies to the root
// agent, which reaches Kubernetes only through the broker.
func TestServiceAccountsNeverMountTokens(t *testing.T) {
	seen := map[string]bool{}
	for _, obj := range testBundle(t) {
		sa, ok := obj.(*corev1.ServiceAccount)
		if !ok {
			continue
		}
		seen[sa.Name] = true
		if sa.AutomountServiceAccountToken == nil {
			t.Errorf("ServiceAccount %q does not set automountServiceAccountToken", sa.Name)
			continue
		}
		if *sa.AutomountServiceAccountToken {
			t.Errorf("ServiceAccount %q mounts an API token", sa.Name)
		}
	}
	for _, want := range []string{SARoot, SAWorker} {
		if !seen[want] {
			t.Errorf("missing ServiceAccount %q", want)
		}
	}
}

// Objects may only live in one of the three namespaces the platform owns: the
// tenant's, the broker's, or the control plane's. Anything else would be an
// accidental cross-tenant leak.
func TestObjectsLiveInAnExpectedNamespace(t *testing.T) {
	n := testSpec().Normalized()
	allowed := map[string]bool{
		Namespace(testSlug):     true,
		n.PlatformNamespace:     true,
		n.ControlPlaneNamespace: true,
	}
	for _, obj := range testBundle(t) {
		if _, ok := obj.(*corev1.Namespace); ok {
			continue // cluster-scoped
		}
		acc, ok := obj.(metav1.Object)
		if !ok {
			t.Fatalf("%T does not implement metav1.Object", obj)
		}
		if !allowed[acc.GetNamespace()] {
			t.Errorf("%T %q is in namespace %q, which the platform does not own",
				obj, acc.GetName(), acc.GetNamespace())
		}
	}
}

// The broker's identity must sit outside the tenant namespace, so that a tenant
// cannot reference it even if it obtained pod-create rights. Kubernetes
// ServiceAccount references are namespace-local, which is what makes this hold.
func TestBrokerIdentityIsOutsideTheTenant(t *testing.T) {
	platform := testSpec().Normalized().PlatformNamespace
	want := testSpec().BrokerServiceAccountName()
	var found bool
	for _, obj := range testBundle(t) {
		sa, ok := obj.(*corev1.ServiceAccount)
		if !ok || sa.Name != want {
			continue
		}
		found = true
		if sa.Namespace != platform {
			t.Errorf("broker ServiceAccount is in %q, want %q", sa.Namespace, platform)
		}
		if sa.Namespace == Namespace(testSlug) {
			t.Error("broker ServiceAccount lives in the tenant namespace; a tenant with pod-create could assume it")
		}
		if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
			t.Error("broker ServiceAccount must not mount a token")
		}
	}
	if !found {
		t.Errorf("broker ServiceAccount %q missing from the bundle", want)
	}
}

// The namespace must be the only cluster-scoped object in the bundle. A
// ClusterRole or ClusterRoleBinding here would grant power outside the tenant
// boundary, which is precisely what the boundary exists to prevent.
func TestNamespaceIsTheOnlyClusterScopedObject(t *testing.T) {
	for _, obj := range testBundle(t) {
		acc, ok := obj.(metav1.Object)
		if !ok {
			t.Fatalf("%T does not implement metav1.Object", obj)
		}
		if acc.GetNamespace() != "" {
			continue
		}
		if _, isNamespace := obj.(*corev1.Namespace); !isNamespace {
			t.Errorf("unexpected cluster-scoped object %T %q", obj, acc.GetName())
		}
	}
}

// Invariant 7: a tenant must not reach the LAN or cluster-internal ranges on its
// own initiative. Any peer that is a bare IP block must be the internet rule,
// and that rule must exclude every private range.
func TestEgressCannotReachPrivateRanges(t *testing.T) {
	privates := make([]*net.IPNet, 0, len(privateCIDRs))
	for _, c := range privateCIDRs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("bad private CIDR %q: %v", c, err)
		}
		privates = append(privates, n)
	}

	var sawInternetRule bool
	for _, obj := range testBundle(t) {
		np, ok := obj.(*networkingv1.NetworkPolicy)
		if !ok {
			continue
		}
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock == nil {
					continue
				}
				if peer.IPBlock.CIDR != "0.0.0.0/0" {
					t.Errorf("%s: unexpected IPBlock egress %q; only the internet rule may use IP blocks", np.Name, peer.IPBlock.CIDR)
					continue
				}
				sawInternetRule = true
				for _, p := range privates {
					if !containsCIDR(peer.IPBlock.Except, p.String()) {
						t.Errorf("%s: internet egress does not exclude %s", np.Name, p.String())
					}
				}
			}
		}
	}
	if !sawInternetRule {
		t.Error("no internet egress rule found; tenants would have no outbound access at all")
	}
}

// The home LAN is the specific asset this rule exists to protect.
func TestHomeLANIsExplicitlyExcluded(t *testing.T) {
	np := findNetpol(t, testBundle(t), PolicyInternetEgress)
	if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].To) != 1 {
		t.Fatalf("unexpected shape for %s", PolicyInternetEgress)
	}
	except := np.Spec.Egress[0].To[0].IPBlock.Except
	if !containsCIDR(except, "192.168.0.0/16") {
		t.Errorf("internet egress does not exclude the home LAN (192.168.0.0/16): %v", except)
	}
}

// A NetworkPolicy rule with an empty peer list matches *every* destination.
// Such a rule would silently undo the deny-by-default posture.
func TestNoRuleMatchesEveryPeer(t *testing.T) {
	for _, obj := range testBundle(t) {
		np, ok := obj.(*networkingv1.NetworkPolicy)
		if !ok {
			continue
		}
		for i, r := range np.Spec.Egress {
			if len(r.To) == 0 {
				t.Errorf("%s: egress rule %d has no peers, which matches every destination", np.Name, i)
			}
		}
		for i, r := range np.Spec.Ingress {
			if len(r.From) == 0 {
				t.Errorf("%s: ingress rule %d has no peers, which matches every source", np.Name, i)
			}
		}
	}
}

// Egress may only target namespaces we intend.
func TestEgressNamespaceAllowlist(t *testing.T) {
	allowed := map[string]bool{"kube-system": true, testSpec().Normalized().PlatformNamespace: true}
	for _, obj := range testBundle(t) {
		np, ok := obj.(*networkingv1.NetworkPolicy)
		if !ok {
			continue
		}
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.NamespaceSelector == nil {
					continue
				}
				name, ok := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
				if !ok {
					t.Errorf("%s: egress targets a namespace selector without an explicit namespace name", np.Name)
					continue
				}
				if !allowed[name] {
					t.Errorf("%s: egress targets unexpected namespace %q", np.Name, name)
				}
			}
		}
	}
}

// Both directions are denied by default before anything is permitted.
func TestDefaultDenyCoversBothDirections(t *testing.T) {
	np := findNetpol(t, testBundle(t), PolicyDefaultDeny)
	var hasIngress, hasEgress bool
	for _, ptype := range np.Spec.PolicyTypes {
		switch ptype {
		case networkingv1.PolicyTypeIngress:
			hasIngress = true
		case networkingv1.PolicyTypeEgress:
			hasEgress = true
		}
	}
	if !hasIngress || !hasEgress {
		t.Errorf("default-deny policyTypes = %v, want both Ingress and Egress", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 0 || len(np.Spec.Egress) != 0 {
		t.Error("default-deny must carry no rules; an empty rule matches everything")
	}
}

// Invariant 3: a tenant cannot modify its own security policy. Enforced by
// omitting these resources from the broker's Role entirely.
func TestBrokerRoleExcludesSecurityControls(t *testing.T) {
	forbidden := map[string]bool{
		"resourcequotas":  true,
		"limitranges":     true,
		"networkpolicies": true,
		"serviceaccounts": true,
		"roles":           true,
		"rolebindings":    true,
		"secrets":         true,
		"namespaces":      true,
		"pods/exec":       true,
	}
	role := findRole(t, testBundle(t), NameBrokerRole)
	if len(role.Rules) == 0 {
		t.Fatal("broker Role has no rules")
	}
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			if forbidden[res] {
				t.Errorf("broker Role grants access to %q", res)
			}
		}
		for _, group := range rule.APIGroups {
			if group == "*" {
				t.Error("broker Role grants a wildcard API group")
			}
		}
		for _, verb := range rule.Verbs {
			if verb == "*" {
				t.Error("broker Role grants a wildcard verb")
			}
		}
	}
}

// Invariant 4: a spawned worker cannot choose its ServiceAccount. The broker's
// identity lives outside the tenant namespace so the tenant cannot reference it
// even with pod-create rights, because ServiceAccount references are
// namespace-local.
func TestBrokerRoleBindingUsesAnIdentityOutsideTheTenant(t *testing.T) {
	rb := findRoleBinding(t, testBundle(t), NameBrokerBind)
	if len(rb.Subjects) != 1 {
		t.Fatalf("expected exactly 1 subject, got %d", len(rb.Subjects))
	}
	sub := rb.Subjects[0]
	if sub.Kind != "ServiceAccount" {
		t.Errorf("subject kind = %q, want ServiceAccount", sub.Kind)
	}
	if sub.Namespace == Namespace(testSlug) {
		t.Error("broker ServiceAccount lives in the tenant namespace; a tenant with pod-create could assume it")
	}
	if sub.Namespace != testSpec().Normalized().PlatformNamespace {
		t.Errorf("subject namespace = %q, want %q", sub.Namespace, testSpec().Normalized().PlatformNamespace)
	}
	if rb.RoleRef.Kind != "Role" {
		t.Errorf("RoleRef.Kind = %q, want Role (namespace-scoped, never a ClusterRole)", rb.RoleRef.Kind)
	}
}

// Invariant: the provisioner's access to a tenant namespace comes from a
// per-namespace RoleBinding, not a cluster-wide grant. If the grant is absent or
// ordered after the objects it authorises, provisioning 403s in the namespace it
// just created; if it binds anything but the control-plane ServiceAccount, the
// tenant role could be handed to another identity.
func TestTenantGrantIsFirstAndBindsTheControlPlane(t *testing.T) {
	classified, err := BundleClassified(testSpec())
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}

	// The namespace is cluster-scoped and needs no grant, so it is created first;
	// the grant must be the very next object.
	if _, ok := classified[0].Object.(*corev1.Namespace); !ok {
		t.Fatalf("first object = %T, want *corev1.Namespace", classified[0].Object)
	}
	grant, ok := classified[1].Object.(*rbacv1.RoleBinding)
	if !ok || grant.Name != NameTenantGrant {
		t.Fatalf("second object = %T, want the %q RoleBinding", classified[1].Object, NameTenantGrant)
	}
	if classified[1].Class != ClassBoundary {
		t.Errorf("tenant grant class = %q, want boundary (teardown relies on the namespace cascade)", classified[1].Class)
	}
	if grant.Namespace != Namespace(testSlug) {
		t.Errorf("tenant grant namespace = %q, want %q", grant.Namespace, Namespace(testSlug))
	}

	// The binding grants the pre-created role to the control-plane identity only.
	if grant.RoleRef.Kind != "ClusterRole" || grant.RoleRef.Name != ClusterRoleTenant {
		t.Errorf("roleRef = %s/%s, want ClusterRole/%s", grant.RoleRef.Kind, grant.RoleRef.Name, ClusterRoleTenant)
	}
	if len(grant.Subjects) != 1 {
		t.Fatalf("expected exactly 1 subject, got %d", len(grant.Subjects))
	}
	sub := grant.Subjects[0]
	wantNS := testSpec().Normalized().ControlPlaneNamespace
	if sub.Kind != "ServiceAccount" || sub.Name != SAControlPlane || sub.Namespace != wantNS {
		t.Errorf("subject = %s %s/%s, want ServiceAccount %s/%s", sub.Kind, sub.Namespace, sub.Name, wantNS, SAControlPlane)
	}
}

// Invariant 8: every resource-consuming dimension is bounded.
func TestQuotaBoundsEveryDimension(t *testing.T) {
	q := findQuota(t, testBundle(t), NameQuota)
	for _, key := range []corev1.ResourceName{
		corev1.ResourceRequestsCPU,
		corev1.ResourceLimitsCPU,
		corev1.ResourceRequestsMemory,
		corev1.ResourceLimitsMemory,
		corev1.ResourcePods,
		corev1.ResourceRequestsStorage,
		corev1.ResourcePersistentVolumeClaims,
		corev1.ResourceServices,
		corev1.ResourceName("count/jobs.batch"),
	} {
		if _, ok := q.Spec.Hard[key]; !ok {
			t.Errorf("quota does not bound %s", key)
		}
	}
	if !q.Spec.Hard[corev1.ResourceLimitsMemory].Equal(testSpec().Normalized().Limits.LimitsMemory) {
		t.Error("quota memory limit does not match the spec")
	}
}

// A workspace must not be able to run an unbounded number of agents.
func TestPodCountIsBounded(t *testing.T) {
	q := findQuota(t, testBundle(t), NameQuota)
	pods := q.Spec.Hard[corev1.ResourcePods]
	if pods.Value() <= 0 || pods.Value() > 12 {
		t.Errorf("pod quota = %d, want a small positive bound", pods.Value())
	}
}

func TestLimitRangeSetsDefaultsAndCeiling(t *testing.T) {
	lr := findLimitRange(t, testBundle(t), NameLimitRange)
	if len(lr.Spec.Limits) != 1 {
		t.Fatalf("expected 1 limit item, got %d", len(lr.Spec.Limits))
	}
	item := lr.Spec.Limits[0]
	for _, m := range []corev1.ResourceList{item.Default, item.DefaultRequest, item.Max} {
		if len(m) == 0 {
			t.Error("limit range item has an empty resource list")
		}
	}
}

func containsCIDR(list []string, want string) bool {
	for _, c := range list {
		if c == want {
			return true
		}
	}
	return false
}
