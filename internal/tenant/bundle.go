// Package tenant renders the Kubernetes objects that make up a workspace.
//
// The central security claim of the platform is that **the tenant namespace is
// the unit of compromise**: anything running inside it may attack anything else
// inside it, and must not be able to reach outside it. Everything in this file
// exists to make that claim true.
//
// Two things are deliberately absent from this package:
//
//   - Tenant workloads. The root Pi and its workers are created by the broker
//     (scarab), which owns the PodSpec. pestilence never emits a PodSpec, so
//     there is no path by which a tenant-supplied securityContext, volume, or
//     ServiceAccount name can reach the API server.
//   - Any tenant-facing credential to the Kubernetes API. Every ServiceAccount
//     created here has automountServiceAccountToken=false.
package tenant

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// StorageClass is the single default StorageClass on the cluster. It is
// node-local and RWO, which is why a workspace's pods are pinned to the node
// holding its volume.
const StorageClass = "openebs-hostpath"

// Labels applied to every object this package emits.
const (
	LabelWorkspace = "agents.gobackto.work/workspace"
	LabelComponent = "agents.gobackto.work/component"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "pestilence"

	// AnnotationReconcile records an object's reconciliation class on the object
	// itself, so the class is observable on the cluster and not only in Go.
	AnnotationReconcile = "agents.gobackto.work/reconcile"
)

// Components.
const (
	ComponentRoot   = "root-agent"
	ComponentWorker = "worker"
	ComponentBroker = "broker"
	ComponentPolicy = "policy"
	ComponentStore  = "storage"
)

// ServiceAccount names inside the tenant namespace.
const (
	SARoot   = "pi-root"
	SAWorker = "pi-worker"
)

// The control-plane identity and the pre-created ClusterRole it binds.
//
// Both are created by my-opps, not here. pestilence holds `bind` on the
// ClusterRole but cannot edit or widen it, so what that role allows is the
// ceiling on what the binding below can grant. See
// my-opps/cluster/manifests/rbac-pestilence-scoped.yaml.
const (
	SAControlPlane    = "pestilence"
	ClusterRoleTenant = "pestilence-tenant"
)

// Object names.
const (
	NameQuota       = "workspace"
	NameLimitRange  = "workspace"
	NameStorage     = "workspace"
	NameMemory      = "memory"
	NameRootService = "root-pi"
	NameBrokerRole  = "broker"
	NameBrokerBind  = "broker"
	NameTenantGrant = "pestilence-tenant"

	PolicyDefaultDeny    = "default-deny"
	PolicyIntraNamespace = "allow-intra-namespace"
	PolicyDNS            = "allow-dns"
	PolicyBrokerEgress   = "allow-broker-egress"
	PolicyIngressToAgent = "allow-ingress-to-workspace"
	PolicyInternetEgress = "allow-internet-egress"
)

// privateCIDRs are the ranges a tenant must never reach on its own initiative.
// They cover RFC1918 (including the home LAN), carrier-grade NAT, loopback, and
// link-local (which is where cloud instance metadata lives).
var privateCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"127.0.0.0/8",
	"100.64.0.0/10",
}

// BundleClassified renders the complete set of objects required for one
// workspace, each tagged with the provisioner's treatment of it (see Class).
//
// It is the only entry point: everything that renders a workspace needs the class
// alongside the object, and a second variant that dropped it was dead code.
//
// Objects live in three namespaces: the tenant's (`ws-<slug>`), the platform
// namespace hosting the broker (`scarab`), and the control plane's own, which
// holds the signing key. The only cross-namespace reference is the RoleBinding
// subject, which deliberately points at a ServiceAccount in the platform
// namespace — Kubernetes ServiceAccount references are namespace-local, so a
// tenant cannot reference that identity at all.
//
// Order is stable.
func BundleClassified(spec Spec) ([]Classified, error) {
	if err := ValidateSlug(spec.Slug); err != nil {
		return nil, err
	}
	s := spec.Normalized()

	objs := []Classified{
		{ClassBoundary, s.namespace()},
		// The tenant grant MUST follow the namespace and precede every other
		// namespaced object: until it exists, the provisioner has no permission
		// inside the namespace it just created.
		{ClassBoundary, s.tenantGrant()},
		{ClassBoundary, s.resourceQuota()},
		{ClassBoundary, s.limitRange()},
		{ClassBoundary, s.defaultDenyPolicy()},
		{ClassBoundary, s.intraNamespacePolicy()},
		{ClassBoundary, s.dnsPolicy()},
		{ClassBoundary, s.brokerEgressPolicy()},
		{ClassBoundary, s.internetEgressPolicy()},
		{ClassBoundary, s.serviceAccount(SARoot)},
		{ClassBoundary, s.serviceAccount(SAWorker)},
		{ClassBoundary, s.brokerRole()},
		{ClassBoundary, s.brokerRoleBinding()},
		// PersistentVolumeClaims are classified as boundary because the workspace
		// is not usable without them. Note the caveat: recreating a deleted PVC
		// yields an EMPTY volume. Data is not recovered, so a PVC disappearing is
		// an event worth alerting on rather than silently healing.
		{ClassBoundary, s.pvc(NameStorage, s.Limits.Storage)},
	}
	if s.WithMemoryPVC {
		objs = append(objs, Classified{ClassBoundary, s.pvc(NameMemory, memoryPVCSize(s))})
	}

	if s.HTTPPort > 0 {
		// The ingress policy is emitted only when there IS an endpoint. Emitting
		// it unconditionally produces a policy with port 0, which is not a valid
		// port and silently fails to admit anything.
		objs = append(objs,
			Classified{ClassBoundary, s.ingressPolicy()},
			Classified{ClassEndpoint, s.rootService()},
		)
		if s.Hostname != "" {
			objs = append(objs, Classified{ClassEndpoint, s.ingress()})
		}
	}

	if s.BrokerImage != "" {
		objs = append(objs, Classified{ClassPlatform, s.brokerDeployment()})
		objs = append(objs, s.brokerPlatformCredentials()...)
		// The agent's copy of the certificate, in the TENANT namespace, because a Secret
		// volume reference is namespace-local and the agent cannot reach the platform
		// one. Emitted unconditionally for the same delete-plan reason.
		objs = append(objs, Classified{ClassBoundary, s.brokerCAConfigMap()})
	}
	// Town's public assertion key, in the TENANT namespace so the bridge can verify
	// the identity headers for itself. Omitted when absent, so a bundle still renders
	// without one -- which is what render-bundle and most tests do.
	if s.AssertionPublicKeyPEM != "" {
		objs = append(objs, Classified{ClassBoundary, s.assertionPubkeyConfigMap()})
	}
	// The broker's identity and address, in the platform namespace. The Service is
	// inert without a workload, and the ServiceAccount is what makes the
	// RoleBinding grant real.
	objs = append(objs,
		Classified{ClassBoundary, s.brokerServiceAccount()},
		Classified{ClassEndpoint, s.brokerService()},
	)

	if s.AgentImage != "" {
		objs = append(objs, Classified{ClassRuntime, s.rootAgentDeployment()})
	}

	// Token material. Emitted unconditionally, including when the bundle is being
	// rendered for a delete plan: the key Secret is a credential in the control
	// plane's namespace, and an object that is absent from the plan would survive
	// teardown unnoticed.
	objs = append(objs,
		Classified{ClassBoundary, s.tokenKeySecret()},
		Classified{ClassBoundary, s.rootTokenSecret()},
		Classified{ClassBoundary, s.tokenPubkeyConfigMap()},
	)

	annotate(objs)
	return objs, nil
}

// brokerPlatformCredentials returns the credentials that sit beside the broker in the
// platform namespace.
//
// Emitted UNCONDITIONALLY, including when the bundle is rendered for a delete plan.
// DeletePlan rebuilds the spec from the stored record, which carries no material, so an
// object gated on material would be absent from the plan and the credential would survive
// teardown unnoticed.
func (s Spec) brokerPlatformCredentials() []Classified {
	return []Classified{
		{ClassPlatform, s.brokerTLSSecret()},
		{ClassPlatform, s.reportTokenSecret()},
	}
}

// annotate stamps the reconciliation class onto each object.
func annotate(objs []Classified) {
	for _, c := range objs {
		acc, ok := c.Object.(metav1.Object)
		if !ok {
			continue
		}
		annotations := acc.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[AnnotationReconcile] = string(c.Class)
		acc.SetAnnotations(annotations)
	}
}

func boolPtr(b bool) *bool { return &b }

func protoTCP() *corev1.Protocol { p := corev1.ProtocolTCP; return &p }
func protoUDP() *corev1.Protocol { p := corev1.ProtocolUDP; return &p }

func tcpPort(p int32) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{Protocol: protoTCP(), Port: &intstr.IntOrString{Type: intstr.Int, IntVal: p}}
}

func udpPort(p int32) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{Protocol: protoUDP(), Port: &intstr.IntOrString{Type: intstr.Int, IntVal: p}}
}

func (s Spec) labels(component string) map[string]string {
	return map[string]string{
		LabelWorkspace: s.Slug,
		LabelComponent: component,
		LabelManagedBy: ManagedByValue,
	}
}

func (s Spec) meta(name, component string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: s.Namespace(),
		Labels:    s.labels(component),
	}
}

// platformMeta addresses an object in the namespace that hosts the broker.
//
// The component label is fixed rather than passed: every object in that namespace
// is the broker's, and a parameter would let a caller label a broker object as
// something else -- which is the label the tenant's egress policy matches on.
func (s Spec) platformMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: s.PlatformNamespace,
		Labels:    s.labels(ComponentBroker),
	}
}

// ---------------------------------------------------------------------------
// Namespace and admission
// ---------------------------------------------------------------------------

// namespace carries the Pod Security Admission labels. Restricted is the
// strongest built-in profile: it requires runAsNonRoot, forbids privilege
// escalation, drops all capabilities, requires a RuntimeDefault seccomp profile,
// and rejects hostPath/hostNetwork/hostPID/hostIPC/hostPort.
//
// Note this is the *cluster's* PSA implementation, so it is enforced by the API
// server even though tenants hold no Kubernetes credentials. It is the second
// defensive layer behind the broker, not the only one.
func (s Spec) namespace() *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Namespace(),
			Labels: map[string]string{
				"pod-security.kubernetes.io/enforce": "restricted",
				"pod-security.kubernetes.io/audit":   "restricted",
				"pod-security.kubernetes.io/warn":    "restricted",
				LabelWorkspace:                       s.Slug,
				LabelManagedBy:                       ManagedByValue,
			},
		},
	}
}

// tenantGrant binds the pre-created ClusterRole/pestilence-tenant to the
// control-plane ServiceAccount, in this workspace's namespace.
//
// This object is where the provisioner's tenant-namespace powers come from. It
// used to hold them cluster-wide via its own ClusterRole; now it holds only
// `bind` on this ClusterRole, and the reach follows the binding. That turns a
// compromise of the control plane from cluster compromise into tenant-namespace
// compromise: it can only reach namespaces where it created this binding, and
// only with the verbs the pre-created role carries.
//
// ORDER MATTERS. It is applied before every other namespaced object, because
// until it exists the provisioner is refused in the namespace it just created.
//
// The subject is the control-plane ServiceAccount and nothing else. The
// admission policy `pestilence-rolebinding-shapes` denies any other subject, so
// this cannot be used to hand the tenant role to a different identity.
func (s Spec) tenantGrant() *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: s.meta(NameTenantGrant, ComponentPolicy),
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     ClusterRoleTenant,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      SAControlPlane,
			Namespace: s.ControlPlaneNamespace,
		}},
	}
}

func (s Spec) resourceQuota() *corev1.ResourceQuota {
	l := s.Limits
	count := func(n int32) resource.Quantity { return *resource.NewQuantity(int64(n), resource.DecimalSI) }
	return &corev1.ResourceQuota{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"},
		ObjectMeta: s.meta(NameQuota, ComponentPolicy),
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU:              l.RequestsCPU,
				corev1.ResourceLimitsCPU:                l.LimitsCPU,
				corev1.ResourceRequestsMemory:           l.RequestsMemory,
				corev1.ResourceLimitsMemory:             l.LimitsMemory,
				corev1.ResourcePods:                     count(l.Pods),
				corev1.ResourceRequestsStorage:          l.Storage,
				corev1.ResourcePersistentVolumeClaims:   count(l.PVCs),
				corev1.ResourceServices:                 count(l.Services),
				corev1.ResourceName("count/jobs.batch"): count(l.Jobs),
			},
		},
	}
}

func (s Spec) limitRange() *corev1.LimitRange {
	l := s.Limits
	return &corev1.LimitRange{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "LimitRange"},
		ObjectMeta: s.meta(NameLimitRange, ComponentPolicy),
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{{
				Type: corev1.LimitTypeContainer,
				Default: corev1.ResourceList{
					corev1.ResourceCPU:    l.ContainerCPUDefault,
					corev1.ResourceMemory: l.ContainerMemoryDefault,
				},
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU:    l.ContainerCPURequest,
					corev1.ResourceMemory: l.ContainerMemoryRequest,
				},
				Max: corev1.ResourceList{
					corev1.ResourceCPU:    l.ContainerCPUMax,
					corev1.ResourceMemory: l.ContainerMemoryMax,
				},
			}},
		},
	}
}

// ---------------------------------------------------------------------------
// Network policy
//
// The model is deny-by-default, with a small set of explicit permits. Note that
// a single permitted destination is enough to admit traffic, so the broad
// "internet" rule does not weaken the narrower rules.
// ---------------------------------------------------------------------------

func (s Spec) netpol(name string, ingress bool, egress bool) *networkingv1.NetworkPolicy {
	types := []networkingv1.PolicyType{}
	if ingress {
		types = append(types, networkingv1.PolicyTypeIngress)
	}
	if egress {
		types = append(types, networkingv1.PolicyTypeEgress)
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: s.meta(name, ComponentPolicy),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: types,
		},
	}
}

// defaultDenyPolicy selects every pod in the namespace and declares both
// directions with no rules at all, so no traffic is permitted unless another
// policy admits it.
func (s Spec) defaultDenyPolicy() *networkingv1.NetworkPolicy {
	return s.netpol(PolicyDefaultDeny, true, true)
}

// intraNamespacePolicy permits traffic between pods in this workspace. That is
// not a security concession: the namespace is already the unit of compromise, so
// traffic inside it is assumed hostile regardless. It exists so the root agent
// and its workers can talk to each other.
func (s Spec) intraNamespacePolicy() *networkingv1.NetworkPolicy {
	np := s.netpol(PolicyIntraNamespace, true, true)
	self := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}
	np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{self}}}
	np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{self}}}
	return np
}

// dnsPolicy permits DNS to the cluster resolver. It is a separate rule because
// the private-CIDR exclusion below also covers the service CIDR, so DNS would
// otherwise be blocked.
func (s Spec) dnsPolicy() *networkingv1.NetworkPolicy {
	np := s.netpol(PolicyDNS, false, true)
	np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{udpPort(53), tcpPort(53)},
	}}
	return np
}

// brokerEgressPolicy permits the workspace to reach its broker. The broker is
// the only privileged interface the tenant has, and every call through it is
// authenticated and authorised against the workspace identity.
func (s Spec) brokerEgressPolicy() *networkingv1.NetworkPolicy {
	np := s.netpol(PolicyBrokerEgress, false, true)
	np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": s.PlatformNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelWorkspace: s.Slug}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{tcpPort(s.BrokerPort)},
	}}
	return np
}

// ingressPolicy admits the ingress controller to the workspace's HTTP endpoint.
// Nothing else may initiate a connection inbound.
func (s Spec) ingressPolicy() *networkingv1.NetworkPolicy {
	np := s.netpol(PolicyIngressToAgent, true, false)
	np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": s.IngressNamespace}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{tcpPort(s.HTTPPort)},
	}}
	return np
}

// internetEgressPolicy is the MVP egress model: "internet yes, LAN no".
//
// Kubernetes NetworkPolicy cannot match by hostname, so this admits 0.0.0.0/0
// and subtracts the private ranges. That reaches model APIs, package registries
// and GitHub, while excluding the home LAN, cloud metadata endpoints, and the
// cluster's own pod and service CIDRs.
//
// Per-hostname allowlisting is a later phase and requires a policy engine that
// understands FQDNs (Cilium's toFQDNs, or an egress proxy the tenant cannot
// bypass).
func (s Spec) internetEgressPolicy() *networkingv1.NetworkPolicy {
	np := s.netpol(PolicyInternetEgress, false, true)
	np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{
			IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: privateCIDRs},
		}},
	}}
	return np
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

// serviceAccount creates a tenant-pod identity with no API credential.
//
// A token is never mounted. This is the first security invariant: no Pi
// container holds a Kubernetes API credential, so there is nothing to steal and
// nothing to escalate with.
func (s Spec) serviceAccount(name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:                     metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta:                   s.meta(name, ComponentWorker),
		AutomountServiceAccountToken: boolPtr(false),
	}
}

// brokerRole is the complete set of verbs the per-workspace broker may use
// inside the tenant namespace.
//
// It deliberately omits resourcequotas, limitranges, networkpolicies,
// serviceaccounts, roles, rolebindings, secrets and namespaces. That omission is
// how the invariant "a tenant cannot modify its own security policy" is enforced
// at the RBAC layer rather than by convention.
func (s Spec) brokerRole() *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: s.meta(NameBrokerRole, ComponentBroker),
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch", "create", "delete"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/log"},
				Verbs:     []string{"get"},
			},
			{
				APIGroups: []string{"batch"},
				Resources: []string{"jobs"},
				Verbs:     []string{"get", "list", "watch", "create", "delete"},
			},
		},
	}
}

// brokerRoleBinding binds the broker Role to a ServiceAccount that lives in
// Spec.PlatformNamespace, not in the tenant namespace.
//
// This is the important part. Because ServiceAccount references are
// namespace-local, a pod in the tenant namespace cannot reference this identity
// at all — so even a tenant that somehow obtained pod-create rights could not
// assume the broker's permissions.
func (s Spec) brokerRoleBinding() *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: s.meta(NameBrokerBind, ComponentBroker),
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     NameBrokerRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      s.BrokerServiceAccountName(),
			Namespace: s.PlatformNamespace,
		}},
	}
}

// brokerServiceAccount is the broker's identity, in the PLATFORM namespace.
//
// This placement is the whole point of the design. ServiceAccount references are
// namespace-local, so a pod in the tenant namespace cannot reference this
// identity even if it somehow obtained pod-create rights. A tenant therefore
// cannot assume the broker's permissions.
func (s Spec) brokerServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:                     metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta:                   s.platformMeta(s.BrokerServiceAccountName()),
		AutomountServiceAccountToken: boolPtr(false),
	}
}

// brokerService gives the root agent a stable address to dial.
//
// The selector carries the workspace label because the tenant's
// `allow-broker-egress` network policy matches the broker pod by exactly that
// label, in exactly this namespace. If the label or the port changes, the tenant
// silently loses the ability to reach its broker.
func (s Spec) brokerService() *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: s.platformMeta(s.BrokerServiceName()),
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				LabelWorkspace: s.Slug,
				LabelComponent: ComponentBroker,
			},
			Ports: []corev1.ServicePort{{
				Name:       "broker",
				Port:       s.BrokerPort,
				TargetPort: intstr.FromInt32(s.BrokerPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// pvc creates a workspace volume. It is deliberately RWO and node-local: the
// cluster is a single node today, so every pod in the workspace can mount it.
// Adding a second node would break that, because the volume cannot follow a pod
// to another node.
func (s Spec) pvc(name string, size resource.Quantity) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: s.meta(name, ComponentStore),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: strPtr(StorageClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
}

func memoryPVCSize(s Spec) resource.Quantity {
	q := s.Limits.Storage.DeepCopy()
	q.Set(int64(5 * 1024 * 1024 * 1024))
	return q
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// rootService fronts the workspace HTTP endpoint. The Ingress that publishes it
// is created separately, because the endpoint's shape is still undecided; this
// Service is inert until something selects it.
func (s Spec) rootService() *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: s.meta(NameRootService, ComponentRoot),
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				LabelWorkspace: s.Slug,
				LabelComponent: ComponentRoot,
			},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       s.HTTPPort,
				TargetPort: intstr.FromInt32(s.HTTPPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}
