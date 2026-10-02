package tenant

import (
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fullSpec exercises every object the bundle can emit.
func fullSpec() Spec {
	return Spec{
		Slug:          testSlug,
		OwnerID:       "owner-1",
		Hostname:      testSlug + ".gobackto.work",
		HTTPPort:      DefaultHTTPPort,
		BrokerImage:   "ghcr.io/example/scarab-broker:test",
		AgentImage:    "ghcr.io/example/scarab-agent:test",
		WithMemoryPVC: true,
	}
}

func fullBundle(t *testing.T) []Classified {
	t.Helper()
	objs, err := BundleClassified(fullSpec())
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}
	return objs
}

func findClassifiedDeployment(t *testing.T, objs []Classified, name string) (Class, *appsv1.Deployment) {
	t.Helper()
	for _, c := range objs {
		if d, ok := c.Object.(*appsv1.Deployment); ok && d.Name == name {
			return c.Class, d
		}
	}
	t.Fatalf("no Deployment named %q in bundle", name)
	return "", nil
}

func findClassifiedIngress(t *testing.T, objs []Classified, name string) (Class, *networkingv1.Ingress) {
	t.Helper()
	for _, c := range objs {
		if ing, ok := c.Object.(*networkingv1.Ingress); ok && ing.Name == name {
			return c.Class, ing
		}
	}
	t.Fatalf("no Ingress named %q in bundle", name)
	return "", nil
}

func envValue(t *testing.T, env []corev1.EnvVar, name string) string {
	t.Helper()
	v, ok := envValueOK(env, name)
	if !ok {
		t.Fatalf("env var %q not set", name)
	}
	return v
}

func envValueOK(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// The broker must be RECONCILED. Classifying it as runtime would mean a deleted
// broker is never recreated, and the tenant -- which cannot write the scarab
// namespace -- would be left silently unable to spawn agents.
func TestBrokerDeploymentIsReconciledNotRuntime(t *testing.T) {
	class, _ := findClassifiedDeployment(t, fullBundle(t), testSpec().BrokerServiceName())
	if class == ClassRuntime {
		t.Fatal("broker Deployment is ClassRuntime; a deleted broker would never come back")
	}
	if class != ClassPlatform {
		t.Errorf("broker Deployment class = %q, want %q", class, ClassPlatform)
	}
}

// The broker is the only tenant-side component that talks to the API server, so
// it MUST hold a credential. The ServiceAccount deliberately does not automount,
// which means the Deployment has to supply the token explicitly.
func TestBrokerDeploymentSuppliesAnAPIToken(t *testing.T) {
	objs := fullBundle(t)

	var sa *corev1.ServiceAccount
	for _, c := range objs {
		if v, ok := c.Object.(*corev1.ServiceAccount); ok && v.Name == testSpec().BrokerServiceAccountName() {
			sa = v
		}
	}
	if sa == nil {
		t.Fatal("broker ServiceAccount missing")
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("broker ServiceAccount should keep automountServiceAccountToken=false, with the token supplied per-pod")
	}

	_, d := findClassifiedDeployment(t, objs, testSpec().BrokerServiceName())
	var found bool
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.ServiceAccountToken == nil {
				continue
			}
			found = true
			if src.ServiceAccountToken.ExpirationSeconds == nil || *src.ServiceAccountToken.ExpirationSeconds <= 0 {
				t.Error("projected service account token should have a bounded lifetime")
			}
		}
	}
	if !found {
		t.Error("broker Deployment has no projected serviceAccountToken volume; the broker cannot obtain an API token and cannot function")
	}

	// And it must be mounted where the broker's client actually looks. The broker
	// uses rest.InClusterConfig(), which reads <mount>/token and <mount>/ca.crt, so
	// the standard service-account path is required -- a custom path leaves it
	// unable to reach the API server.
	const saPath = "/var/run/secrets/kubernetes.io/serviceaccount"
	var mounted bool
	for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == saPath {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("projected token volume is not mounted at %s, so rest.InClusterConfig() cannot find it", saPath)
	}

	// TLS to the API server needs the cluster CA at that same path, and the
	// namespace is what in-cluster clients report as their own.
	var hasCA, hasNamespace bool
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.ConfigMap != nil && src.ConfigMap.Name == "kube-root-ca.crt" {
				hasCA = true
			}
			if src.DownwardAPI != nil {
				hasNamespace = true
			}
		}
	}
	if !hasCA {
		t.Error("projected volume does not supply the cluster CA; the broker cannot verify the API server's TLS certificate")
	}
	if !hasNamespace {
		t.Error("projected volume does not supply the namespace")
	}
}

// The workspace label on the broker POD is load-bearing: the tenant's
// allow-broker-egress policy matches it, and there is no admission-time error if
// it is missing -- the tenant simply cannot reach its broker.
func TestBrokerPodCarriesTheWorkspaceLabel(t *testing.T) {
	_, d := findClassifiedDeployment(t, fullBundle(t), testSpec().BrokerServiceName())
	labels := d.Spec.Template.Labels
	if labels[LabelWorkspace] != testSlug {
		t.Errorf("broker pod %s = %q, want %q; the tenant egress policy will not match it",
			LabelWorkspace, labels[LabelWorkspace], testSlug)
	}
	if labels[LabelComponent] != ComponentBroker {
		t.Errorf("broker pod %s = %q, want %q", LabelComponent, labels[LabelComponent], ComponentBroker)
	}
}

// The broker cannot read the ResourceQuota, so its budget env is its only source
// of truth. If these drift from the quota, its accounting silently diverges from
// what the API server will admit.
func TestBrokerEnvMirrorsTheQuota(t *testing.T) {
	_, d := findClassifiedDeployment(t, fullBundle(t), testSpec().BrokerServiceName())
	env := d.Spec.Template.Spec.Containers[0].Env
	l := testSpec().Normalized().Limits

	if got, want := envValue(t, env, "SCARAB_MEMORY_BUDGET"), l.RequestsMemory.String(); got != want {
		t.Errorf("SCARAB_MEMORY_BUDGET = %q, want %q (the quota's requests.memory, the binding dimension)", got, want)
	}
	if got, want := envValue(t, env, "SCARAB_POD_BUDGET"), strconv.Itoa(int(l.Pods)); got != want {
		t.Errorf("SCARAB_POD_BUDGET = %q, want %q", got, want)
	}
	if got, want := envValue(t, env, "SCARAB_CONTAINER_CPU_MAX"), l.ContainerCPUMax.String(); got != want {
		t.Errorf("SCARAB_CONTAINER_CPU_MAX = %q, want %q", got, want)
	}
	if got, want := envValue(t, env, "SCARAB_CONTAINER_MEM_MAX"), l.ContainerMemoryMax.String(); got != want {
		t.Errorf("SCARAB_CONTAINER_MEM_MAX = %q, want %q", got, want)
	}
	if got := envValue(t, env, "SCARAB_WORKSPACE_NAMESPACE"); got != Namespace(testSlug) {
		t.Errorf("SCARAB_WORKSPACE_NAMESPACE = %q, want %q", got, Namespace(testSlug))
	}
}

// The root agent is the one object the tenant owns. It must never be reconciled
// back, and Recreate is mandatory because openebs-hostpath is RWO.
func TestRootAgentIsRuntimeAndRecreates(t *testing.T) {
	class, d := findClassifiedDeployment(t, fullBundle(t), NameRootDeployment)
	if class != ClassRuntime {
		t.Errorf("root agent class = %q, want %q (the tenant may replace it)", class, ClassRuntime)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("root agent strategy = %q, want Recreate; RollingUpdate deadlocks on an RWO volume", d.Spec.Strategy.Type)
	}
	pod := d.Spec.Template.Spec
	if pod.ServiceAccountName != SARoot {
		t.Errorf("root agent serviceAccountName = %q, want %q; omitting it is denied at admission", pod.ServiceAccountName, SARoot)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("root agent must not mount an API token")
	}
	c := pod.Containers[0]
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Error("root agent needs a /healthz readiness probe, or the Service routes to a dead bridge")
	}
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("root agent container must set allowPrivilegeEscalation=false (PSA restricted)")
	}
}

// The capability token must never land on /workspace, which every worker can
// read.
func TestRootAgentTokenIsAMountedSecretNotOnTheSharedVolume(t *testing.T) {
	_, d := findClassifiedDeployment(t, fullBundle(t), NameRootDeployment)

	var tokenVolume *corev1.Volume
	for i := range d.Spec.Template.Spec.Volumes {
		if d.Spec.Template.Spec.Volumes[i].Name == "cap-token" {
			tokenVolume = &d.Spec.Template.Spec.Volumes[i]
		}
	}
	if tokenVolume == nil {
		t.Fatal("root agent has no cap-token volume")
	}
	if tokenVolume.Secret == nil {
		t.Fatalf("cap-token volume is not a Secret (got %+v)", tokenVolume.VolumeSource)
	}
	if tokenVolume.Secret.SecretName != testSpec().Normalized().TokenSecretName {
		t.Errorf("cap-token secret = %q, want %q", tokenVolume.Secret.SecretName, testSpec().Normalized().TokenSecretName)
	}

	for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "cap-token" && m.MountPath != MountToken {
			t.Errorf("token mount path = %q, want %q", m.MountPath, MountToken)
		}
		if m.Name == "workspace" && m.MountPath != MountWorkspace {
			t.Errorf("workspace mount path = %q, want %q", m.MountPath, MountWorkspace)
		}
	}
}

// The Ingress must use Traefik's default certificate. Naming a secret would
// require copying the wildcard private key into the tenant namespace, where any
// agent process could read it -- and a leaked wildcard key impersonates every
// workspace on the domain.
func TestIngressUsesTheDefaultCertificateAndTheHostname(t *testing.T) {
	class, ing := findClassifiedIngress(t, fullBundle(t), testSlug)
	if class != ClassEndpoint {
		t.Errorf("Ingress class = %q, want %q", class, ClassEndpoint)
	}
	if len(ing.Spec.TLS) == 0 {
		t.Fatal("Ingress has no TLS block; Traefik only serves the default certificate to a TLS-enabled router")
	}
	for _, tls := range ing.Spec.TLS {
		if tls.SecretName != "" {
			t.Errorf("Ingress names tls.secretName %q; the wildcard private key must not enter the tenant namespace", tls.SecretName)
		}
	}
	if _, ok := ing.Annotations["cert-manager.io/cluster-issuer"]; ok {
		t.Error("Ingress must not request a per-workspace certificate; the wildcard already covers it")
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != fullSpec().Hostname {
		t.Errorf("Ingress host = %+v, want %q (Spec.Hostname must be used)", ing.Spec.Rules, fullSpec().Hostname)
	}
	backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if backend.Name != NameRootService || backend.Port.Number != DefaultHTTPPort {
		t.Errorf("Ingress backend = %s:%d, want %s:%d", backend.Name, backend.Port.Number, NameRootService, DefaultHTTPPort)
	}
}

// A workspace with no endpoint must not get an ingress policy carrying port 0,
// which is not a valid port and would silently admit nothing.
func TestNoEndpointObjectsWhenHTTPPortIsZero(t *testing.T) {
	spec := fullSpec()
	spec.HTTPPort = 0
	objs, err := BundleClassified(spec)
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}
	for _, c := range objs {
		switch v := c.Object.(type) {
		case *networkingv1.Ingress:
			t.Errorf("Ingress emitted for a workspace with no endpoint")
		case *networkingv1.NetworkPolicy:
			if v.Name == PolicyIngressToAgent {
				t.Error("allow-ingress-to-workspace emitted for a workspace with no endpoint (port 0 is not a valid port)")
			}
		case *corev1.Service:
			if v.Name == NameRootService {
				t.Error("root-pi Service emitted for a workspace with no endpoint")
			}
		}
	}
}

// Images do not exist yet (§8.6), so emitting a Deployment that can never pull
// would be worse than omitting it.
func TestNoDeploymentsWhenImagesAreUnset(t *testing.T) {
	spec := fullSpec()
	spec.BrokerImage = ""
	spec.AgentImage = ""
	objs, err := BundleClassified(spec)
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}
	for _, c := range objs {
		if _, ok := c.Object.(*appsv1.Deployment); ok {
			t.Errorf("Deployment %T emitted with no image configured", c.Object)
		}
	}
}

// The agent directory IS the credential store, and it must live on the workspace
// volume so the root agent and its workers share one key (§8.4, §8.10). A drift
// back to ephemeral storage would silently break "bring your own key": the user
// would re-enter it after every pod restart and workers would have none.
func TestRootAgentUsesTheSharedCredentialStore(t *testing.T) {
	_, d := findClassifiedDeployment(t, fullBundle(t), NameRootDeployment)
	mounts := d.Spec.Template.Spec.Containers[0].VolumeMounts

	if got := envValue(t, d.Spec.Template.Spec.Containers[0].Env, "PI_CODING_AGENT_DIR"); got != AgentDir {
		t.Errorf("PI_CODING_AGENT_DIR = %q, want %q", got, AgentDir)
	}
	if !strings.HasPrefix(AgentDir, MountWorkspace+"/") {
		t.Errorf("AgentDir %q is not on the workspace volume %q, so workers cannot share the store", AgentDir, MountWorkspace)
	}

	// Setting the variable is not enough: the path has to be under a mount, or
	// the bridge fails at runtime writing auth.json.
	covered := false
	for _, m := range mounts {
		if strings.HasPrefix(AgentDir, strings.TrimSuffix(m.MountPath, "/")+"/") {
			covered = true
		}
	}
	if !covered {
		t.Errorf("AgentDir %q is not covered by any volumeMount on the root agent", AgentDir)
	}

	// And it must NOT be the scratch volume: /scratch is ephemeral by design.
	for _, m := range mounts {
		if m.Name == "scratch" && strings.HasPrefix(AgentDir, m.MountPath) {
			t.Errorf("AgentDir %q is on the scratch volume, which is ephemeral", AgentDir)
		}
	}
}

// The reconciliation class should be observable on the cluster, not only in Go.
func TestEveryObjectCarriesItsReconcileClass(t *testing.T) {
	for _, c := range fullBundle(t) {
		acc, ok := c.Object.(metav1.Object)
		if !ok {
			t.Fatalf("%T has no ObjectMeta", c.Object)
		}
		got := acc.GetAnnotations()[AnnotationReconcile]
		if got != string(c.Class) {
			t.Errorf("%T %q annotation %s = %q, want %q", c.Object, acc.GetName(), AnnotationReconcile, got, c.Class)
		}
	}
}

// Tenant-namespace pod templates must satisfy the ValidatingAdmissionPolicy as
// well as Pod Security Admission. Cheaper to catch here than at the API server.
func TestTenantPodTemplatesSatisfyTheAdmissionPolicy(t *testing.T) {
	tenantNS := Namespace(testSlug)
	for _, c := range fullBundle(t) {
		d, ok := c.Object.(*appsv1.Deployment)
		if !ok || d.Namespace != tenantNS {
			continue // the broker lives in the platform namespace; see below
		}
		pod := d.Spec.Template.Spec
		if pod.ServiceAccountName != SARoot && pod.ServiceAccountName != SAWorker {
			t.Errorf("%s: serviceAccountName %q is not allowlisted", d.Name, pod.ServiceAccountName)
		}
		if pod.AutomountServiceAccountToken != nil && *pod.AutomountServiceAccountToken {
			t.Errorf("%s: must not automount an API token", d.Name)
		}
		if pod.NodeName != "" {
			t.Errorf("%s: spec.nodeName is forbidden", d.Name)
		}
		if pod.RuntimeClassName != nil {
			t.Errorf("%s: runtimeClassName is forbidden", d.Name)
		}
		for _, v := range pod.Volumes {
			switch {
			case v.PersistentVolumeClaim != nil, v.EmptyDir != nil, v.Secret != nil,
				v.ConfigMap != nil, v.Projected != nil, v.DownwardAPI != nil, v.Ephemeral != nil:
			default:
				t.Errorf("%s: volume %q is not in the allowlist: %+v", d.Name, v.Name, v.VolumeSource)
			}
		}
		for _, ct := range pod.Containers {
			sc := ct.SecurityContext
			if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Errorf("%s/%s: allowPrivilegeEscalation must be false", d.Name, ct.Name)
			}
			if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 {
				t.Errorf("%s/%s: capabilities must be dropped", d.Name, ct.Name)
			}
		}
	}
}

// The broker is platform infrastructure, not a tenant pod, and the two are held
// to different rules. This asymmetry is easy to get wrong in either direction:
//
//   - The broker POD must carry the workspace label, or the tenant's
//     allow-broker-egress policy does not match it and the tenant silently cannot
//     reach its broker.
//   - The broker NAMESPACE must NOT carry the workspace label, or the tenant
//     ValidatingAdmissionPolicy applies to the broker and rejects it -- the
//     broker's ServiceAccount is not pi-root/pi-worker, and it legitimately needs
//     an API token that tenant-pod-invariants forbids.
//
// Pod Security Admission still applies, because the scarab namespace is labelled
// restricted in my-opps.
func TestBrokerPodSatisfiesPSAButIsOutsideTheTenantPolicy(t *testing.T) {
	_, d := findClassifiedDeployment(t, fullBundle(t), testSpec().BrokerServiceName())

	if d.Namespace == Namespace(testSlug) {
		t.Fatal("broker must not run in the tenant namespace")
	}
	if got := d.Spec.Template.Labels[LabelWorkspace]; got != testSlug {
		t.Errorf("broker pod %s = %q, want %q; the tenant egress policy will not match it",
			LabelWorkspace, got, testSlug)
	}
	sa := d.Spec.Template.Spec.ServiceAccountName
	if sa == SARoot || sa == SAWorker {
		t.Errorf("broker must not use a tenant identity (%q)", sa)
	}
	if sa != testSpec().BrokerServiceAccountName() {
		t.Errorf("broker serviceAccountName = %q, want %q", sa, testSpec().BrokerServiceAccountName())
	}

	for _, ct := range d.Spec.Template.Spec.Containers {
		sc := ct.SecurityContext
		if sc == nil {
			t.Fatalf("broker container %q has no securityContext", ct.Name)
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("broker container %q must set runAsNonRoot (PSA restricted in the scarab namespace)", ct.Name)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("broker container %q must set allowPrivilegeEscalation=false", ct.Name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 {
			t.Errorf("broker container %q must drop capabilities", ct.Name)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != "RuntimeDefault" {
			t.Errorf("broker container %q must set a RuntimeDefault seccomp profile", ct.Name)
		}
	}
}

// The capability token must be readable by the pod and by nothing else.
//
// Subtle enough to have been got wrong twice in opposite directions, so the invariant
// is asserted as a PAIR of facts rather than one mode:
//
//  1. defaultMode is 0400, so the file is not world-readable. The first version was
//     0444, which the red team flagged: the token authorizes broker calls, and "any
//     process in the pod" includes a worker.
//  2. the pod sets fsGroup, so the kubelet chowns the Secret volume to root:<fsGroup>
//     and adds the group read bit. That is what makes an effective 0440 -- readable by
//     the pod's group and not by the world.
//
// Asserting only the mode would push the next person back to 0444 to fix an unreadable
// token; asserting only fsGroup would miss the world bit. Together they are the actual
// requirement.
func TestCapabilityTokenIsReadableByThePodAndNotTheWorld(t *testing.T) {
	_, d := findClassifiedDeployment(t, fullBundle(t), NameRootDeployment)

	psc := d.Spec.Template.Spec.SecurityContext
	if psc == nil || psc.FSGroup == nil {
		t.Fatal("root agent pod has no fsGroup; the token Secret and the workspace volume " +
			"stay root-owned, and the only way to make them usable is to loosen their " +
			"modes -- which is how /workspace ended up world-writable")
	}
	if psc.RunAsUser == nil || *psc.FSGroup != *psc.RunAsUser {
		t.Errorf("fsGroup %v must match runAsUser %v, or the kubelet chowns the volume to "+
			"a group the process is not in", psc.FSGroup, psc.RunAsUser)
	}

	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name != "cap-token" {
			continue
		}
		if v.Secret == nil {
			t.Fatal("cap-token volume is not a Secret")
		}
		if v.Secret.DefaultMode == nil {
			t.Fatal("cap-token volume has no explicit defaultMode")
		}
		mode := *v.Secret.DefaultMode
		if mode&0o004 != 0 {
			t.Errorf("cap-token defaultMode %#o is world-readable; the token authorizes broker "+
				"calls and must not be readable outside the pod's group", mode)
		}
		if mode&0o400 == 0 {
			t.Errorf("cap-token defaultMode %#o grants the owner no read bit; fsGroup adds the "+
				"group bit, not the owner's", mode)
		}
		return
	}
	t.Fatal("root agent Deployment has no cap-token volume")
}

// Every workspace endpoint must carry the edge middlewares.
//
// These are not authentication and are not a substitute for it, but their ABSENCE is
// silent: Traefik serves the route happily without them. That is how the platform came
// to be reachable from the public internet with no allowlist anywhere in the cluster,
// which the first red-team review demonstrated by driving a workspace from a public IP.
// The edge chain, in order. Asserted as a whole string because ORDER IS
// SECURITY-RELEVANT: `strip-untrusted-user` must run before `forwardauth`, or it would
// delete the X-Auth-User that ForwardAuth had just set and the bridge would silently
// see nobody. A set-membership check would pass with the two swapped.
//
// There is deliberately no allowlist here. Its absence is asserted too, because it was
// removed on purpose and re-adding it would silently narrow the platform back to the LAN.
func TestWorkspaceIngressCarriesTheEdgeMiddlewares(t *testing.T) {
	want := "traefik-strip-untrusted-user@kubernetescrd," +
		"traefik-security-headers@kubernetescrd," +
		"traefik-forwardauth@kubernetescrd"

	for _, c := range fullBundle(t) {
		ing, ok := c.Object.(*networkingv1.Ingress)
		if !ok {
			continue
		}
		got := ing.Annotations[AnnotationTraefikMiddlewares]
		if got != want {
			t.Errorf("Ingress %q middlewares\n got: %q\nwant: %q", ing.Name, got, want)
		}
		if strings.Contains(got, "allowlist") {
			t.Errorf("Ingress %q carries an IP allowlist; the gate is identity-based, not location-based", ing.Name)
		}
		return
	}
	t.Fatal("bundle has no Ingress")
}

// The bridge disables its Host and Origin checks when this is unset, and disables them
// QUIETLY -- a warning in a log, not a failure. So the only place the omission is
// catchable is here.
func TestRootAgentIsToldItsOwnHostname(t *testing.T) {
	for _, c := range fullBundle(t) {
		dep, ok := c.Object.(*appsv1.Deployment)
		if !ok || dep.Name != NameRootDeployment {
			continue
		}
		for _, container := range dep.Spec.Template.Spec.Containers {
			for _, env := range container.Env {
				if env.Name == "SCARAB_HOSTNAME" {
					if want := testSlug + ".gobackto.work"; env.Value != want {
						t.Errorf("SCARAB_HOSTNAME = %q, want the workspace's public hostname %q", env.Value, want)
					}
					return
				}
			}
		}
		t.Error("the root agent Deployment does not set SCARAB_HOSTNAME; the bridge's Host and Origin checks would be disabled")
		return
	}
	t.Fatal("bundle has no root agent Deployment")
}

// The bridge verifies town's assertions for itself, and it cannot read the control
// plane's copy -- a ConfigMap cannot be mounted across namespaces. So the key has to be
// PUBLISHED into the tenant namespace, and pestilence is the only party that can: town
// holds no Kubernetes credential at all.
func TestTownAssertionKeyIsPublishedIntoTheTenantNamespace(t *testing.T) {
	const pem = "-----BEGIN PUBLIC KEY-----\nstub\n-----END PUBLIC KEY-----\n"
	spec := fullSpec()
	spec.AssertionPublicKeyPEM = pem

	objs, err := BundleClassified(spec)
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}

	var found *corev1.ConfigMap
	for _, c := range objs {
		if cm, ok := c.Object.(*corev1.ConfigMap); ok && cm.Name == "town-assertion-pubkey" {
			found = cm
			if c.Class != ClassBoundary {
				t.Errorf("class = %v, want ClassBoundary -- it lives in the tenant namespace", c.Class)
			}
		}
	}
	if found == nil {
		t.Fatal("the assertion public key was not published; the bridge cannot verify town without it")
	}
	if found.Namespace != spec.Namespace() {
		t.Errorf("namespace = %q, want the tenant namespace %q", found.Namespace, spec.Namespace())
	}
	if got := found.Data["assertion-key.pub"]; got != pem {
		t.Errorf("the key data is not the PEM that was supplied")
	}
}

func TestTownAssertionKeyIsAbsentWhenNotConfigured(t *testing.T) {
	// A bundle must still render without one, which is what render-bundle and most
	// tests do -- so this is about not crashing, not about security.
	objs, err := BundleClassified(fullSpec())
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}
	for _, c := range objs {
		if cm, ok := c.Object.(*corev1.ConfigMap); ok && cm.Name == "town-assertion-pubkey" {
			t.Error("a ConfigMap was emitted with no key configured")
		}
	}
}

func TestRootAgentCanReachThePublishedAssertionKey(t *testing.T) {
	// Publishing it is only half the job: if the mount or the path is wrong the bridge
	// starts with no key, and the symptom is a verification failure on every request
	// rather than a missing file at deploy time.
	spec := fullSpec()
	spec.AssertionPublicKeyPEM = "-----BEGIN PUBLIC KEY-----\nstub\n-----END PUBLIC KEY-----\n"

	objs, err := BundleClassified(spec)
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}
	for _, c := range objs {
		dep, ok := c.Object.(*appsv1.Deployment)
		if !ok || dep.Name != NameRootDeployment {
			continue
		}
		pod := dep.Spec.Template.Spec

		var mounted bool
		for _, m := range pod.Containers[0].VolumeMounts {
			if m.Name == "town-assertion-pubkey" {
				mounted = m.ReadOnly
				if m.MountPath != "/var/run/scarab/assertion" {
					t.Errorf("mountPath = %q", m.MountPath)
				}
			}
		}
		if !mounted {
			t.Fatal("the root agent does not mount the assertion key read-only")
		}

		var told string
		for _, e := range pod.Containers[0].Env {
			if e.Name == "SCARAB_ASSERTION_PUBKEY" {
				told = e.Value
			}
		}
		if told != "/var/run/scarab/assertion/assertion-key.pub" {
			t.Errorf("SCARAB_ASSERTION_PUBKEY = %q, which is not where the key is mounted", told)
		}
		return
	}
	t.Fatal("bundle has no root agent Deployment")
}
