package tenant

import (
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Interface constants. These mirror the canonical list in
// scarab/docs/architecture.md Appendix A. A value must not be typed twice, so they
// live here as named constants rather than as literals at their use sites.
//
// The scarab handoff proposes extracting these into a package that pestilence
// vendors (§17.1), so that a rename on either side fails the other side's build.
// Until that exists, every change here must be made in the same change there.
const (
	// Object names.
	NameRootDeployment  = "root-agent"
	NameRootTokenSecret = "pi-root-token"

	// DefaultControlPlaneNamespace is where pestilence keeps per-workspace
	// signing keys. Declared in my-opps alongside its RBAC.
	DefaultControlPlaneNamespace = "pestilence"

	// Container names.
	ContainerBroker = "broker"
	ContainerBridge = "bridge"

	// Ports.
	DefaultBrokerPort = 8443
	DefaultHTTPPort   = 8000

	// Filesystem layout. Shared by the root agent and every worker, so these are
	// a contract rather than an implementation detail.
	MountWorkspace = "/workspace"
	MountMemory    = "/memory"
	MountScratch   = "/scratch"
	MountToken     = "/var/run/scarab/token"
	ResultRoot     = "/workspace/.agents"
	SessionDir     = "/workspace/.pi/sessions"
	// AgentDir is Pi's agent directory, which is also its credential store:
	// <AgentDir>/auth.json holds the model key the user supplies in the session.
	//
	// It is deliberately ON the workspace PVC. The tenant brings its own key, the
	// tenant namespace is the unit of compromise, and workers must read the same
	// store — so persistence to the shared volume is the mechanism, not an
	// accident. The consequence is accepted rather than mitigated: anything in
	// the workspace can read it, including any command the agent chooses to run.
	AgentDir = "/workspace/.pi/agent"

	// Token public-key plumbing for the broker. The directory is mounted from a
	// ConfigMap and the path is what the broker reads.
	tokenPubkeyDir  = "/var/run/scarab/token-pubkey"
	tokenPubkeyPath = tokenPubkeyDir + "/" + TokenPublicKeyKey

	// Town's assertion public key, published into every tenant namespace.
	//
	// A DIFFERENT key from the broker's above, and worth keeping distinct: that one
	// verifies a workspace's own capability token, this one verifies the platform's
	// identity headers. The bridge needs this one, and it cannot read the original --
	// a ConfigMap cannot be mounted across namespaces -- so it gets a copy in its own
	// namespace, under the same name the control plane uses for its own.
	assertionPubkeyDir  = "/var/run/scarab/assertion"
	assertionPubkeyPath = assertionPubkeyDir + "/" + AssertionPublicKeyKey

	// AssertionPublicKeyKey is the data key inside the published ConfigMap. It is
	// exported because the operator's key generator writes the same key into the
	// control plane's own copy, and the two must stay identical.
	AssertionPublicKeyKey = "assertion-key.pub"

	// nameAssertionPubkey is the ConfigMap's name in the tenant namespace. The same
	// name is used for the control plane's own copy in its own namespace, so the two
	// are greppable as a pair.
	nameAssertionPubkey = "town-assertion-pubkey"

	// DefaultIngressClass matches the cluster's default IngressClass.
	DefaultIngressClass = "traefik"

	// AnnotationTraefikMiddlewares attaches Traefik middlewares to an Ingress.
	AnnotationTraefikMiddlewares = "traefik.ingress.kubernetes.io/router.middlewares"

	// TraefikWorkspaceMiddlewares is the edge policy for every workspace endpoint.
	// The middlewares live in my-opps, in the traefik namespace, and are referenced
	// as <namespace>-<name>@kubernetescrd.
	//
	// ORDER IS SECURITY-RELEVANT, not cosmetic. The chain wraps the backend, so the
	// LAST entry is the innermost and runs closest to the bridge:
	//
	//   strip-untrusted-user removes any client-supplied X-Auth-User. It MUST come
	//                        before forwardauth -- after it, it would delete the
	//                        header ForwardAuth had just set, and the bridge would
	//                        silently see nobody
	//   security-headers     HSTS and the standard headers
	//   forwardauth          asks town, and copies X-Auth-User and
	//                        X-Scarab-Assertion onto the upstream request
	//
	// There is deliberately NO IP allowlist. One used to sit first, admitting only the
	// LAN, and it was removed once ForwardAuth could carry the load: the endpoint is
	// meant to be reachable from anywhere, and the gate is meant to be WHO you are
	// rather than WHERE you are.
	//
	// That makes forwardauth the ONLY gate, which is worth stating because of how it
	// fails. These are referenced by NAME, and a name that does not resolve is served
	// WITHOUT the middleware rather than refused -- Traefik logs an error and carries
	// on. A rename on either side therefore produces a fully open endpoint with no
	// functional symptom. The bridge verifying X-Scarab-Assertion is what restores a
	// second, independent check; until that lands, this annotation is load-bearing
	// alone.
	//
	// The names are a CONTRACT with my-opps, referenced by literal name rather than
	// built from a variable, to keep the string greppable across both repositories.
	TraefikWorkspaceMiddlewares = "traefik-strip-untrusted-user@kubernetescrd," +
		"traefik-security-headers@kubernetescrd," +
		"traefik-forwardauth@kubernetescrd"

	// ScratchSizeLimit bounds the ephemeral volumes so a runaway build cannot
	// fill the node's disk.
	scratchSizeLimit = "1Gi"
	tmpSizeLimit     = "256Mi"
)

// ---------------------------------------------------------------------------
// Shared building blocks
// ---------------------------------------------------------------------------

// Numeric uids, matching each image's numeric USER.
//
// runAsNonRoot alone is NOT enough: the kubelet must be able to verify the user
// is non-root, and it cannot resolve a NAMED user from the image config. With a
// named USER the pod fails with "image has non-numeric user (node), cannot verify
// user is non-root". So the uid is pinned numerically here as well as in the
// image, and the pod no longer depends on the image config.
const (
	UIDAgent  int64 = 1000
	UIDBroker int64 = 65532
)

// restrictedSecurityContext satisfies Pod Security Admission `restricted`, which
// is enforced on both the tenant namespaces and the scarab namespace.
func restrictedSecurityContext(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		RunAsNonRoot:             boolPtr(true),
		RunAsUser:                int64Ptr(uid),
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// restrictedPodSecurityContext satisfies Pod Security Admission `restricted`.
//
// fsGroup is load-bearing, and its absence caused the worst of the first red-team
// review's permission findings. The volumes the kubelet mounts -- the workspace PVC,
// the scratch and tmp emptyDirs, and the capability-token Secret -- arrive owned by
// root. Without a fsGroup the kubelet chowns nothing, so a pod running as uid 1000
// could not write to its own workspace unless the volume was world-writable, and the
// token Secret had to be 0444 to be readable at all.
//
// What fsGroup actually does, verified rather than assumed: it chowns the volumes to
// root:<fsGroup> and ORs in the group permission bits. It does NOT clear the world
// bits, whatever the documentation implies -- a 0777 emptyDir becomes 2777, not 2770.
// That is why the workspace volume is still world-writable and why the token's mode
// is set explicitly rather than left to fsGroup.
//
// For the token the pair works: defaultMode 0400 is ORed to an effective 0440, so the
// pod's group can read it and the world cannot. That is the whole of the fix for a
// finding that was previously solved by widening the mode to 0444.
//
// fsGroup == the pod's uid is deliberate. A distinct group would work equally well and
// is arguably tidier, but the tenant images already run as a single uid and inventing
// a second number is one more thing to keep in sync across two repositories.
func restrictedPodSecurityContext(uid int64) *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   boolPtr(true),
		RunAsUser:      int64Ptr(uid),
		RunAsGroup:     int64Ptr(uid),
		FSGroup:        int64Ptr(uid),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// sizedEmptyDir returns an emptyDir with a sizeLimit, so a runaway process in the
// workspace cannot fill the node's disk.
func sizedEmptyDir(size string) corev1.VolumeSource {
	q := resource.MustParse(size)
	return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &q}}
}

// ---------------------------------------------------------------------------
// §5.5 The broker (platform namespace)
// ---------------------------------------------------------------------------

// brokerDeployment runs the per-workspace broker.
//
// The broker is the ONLY tenant-side component that talks to the Kubernetes API,
// and it needs a credential to do so. The ServiceAccount deliberately has
// automountServiceAccountToken: false, so the token is supplied explicitly as a
// projected serviceAccountToken volume with a bounded lifetime. A pod that merely
// names that ServiceAccount would get no token and the broker could not function.
//
// Class: platform (reconciled). A deleted broker must come back, because the
// tenant cannot write the scarab namespace and would otherwise be left silently
// unable to spawn agents.
func (s Spec) brokerDeployment() *appsv1.Deployment {
	// The workspace label on the POD is load-bearing: the tenant's
	// allow-broker-egress policy matches the broker pod by it, and there is no
	// admission-time error if it is missing -- the tenant simply cannot reach
	// its broker.
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: s.platformMeta(s.BrokerServiceName()),
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			// The broker holds a single-workspace lease conceptually; a second
			// replica would be a second broker for the same workspace.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				LabelWorkspace: s.Slug,
				LabelComponent: ComponentBroker,
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: s.labels(ComponentBroker)},
				Spec:       s.brokerPodSpec(),
			},
		},
	}
}

func (s Spec) brokerPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		ServiceAccountName:           s.BrokerServiceAccountName(),
		AutomountServiceAccountToken: boolPtr(false),
		SecurityContext:              restrictedPodSecurityContext(UIDBroker),
		Containers:                   []corev1.Container{s.brokerContainer()},
		Volumes:                      s.brokerVolumes(),
	}
}

func (s Spec) brokerContainer() corev1.Container {
	container := corev1.Container{
		Name:            ContainerBroker,
		Image:           s.BrokerImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{{
			Name:          "broker",
			ContainerPort: s.BrokerPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env:             s.brokerEnv(),
		SecurityContext: restrictedSecurityContext(UIDBroker),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				// The STANDARD service-account path. cmd/broker builds its client with
				// rest.InClusterConfig(), which reads <mount>/token and <mount>/ca.crt.
				// A custom path leaves the broker unable to reach the API server.
				Name:      "api-token",
				MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
				ReadOnly:  true,
			},
			{Name: "token-pubkey", MountPath: tokenPubkeyDir, ReadOnly: true},
			{Name: "report-token", MountPath: MountReportToken, ReadOnly: true},
		},
	}
	if s.BrokerTLS {
		container.VolumeMounts = append(container.VolumeMounts,
			corev1.VolumeMount{Name: nameBrokerTLS, MountPath: MountBrokerTLS, ReadOnly: true})
	}
	return container
}

// brokerVolumes projects the broker's own API credential, and publishes the token
// verification key it refuses to start without.
func (s Spec) brokerVolumes() []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "api-token",
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Path:              "token",
							ExpirationSeconds: int64Ptr(3600),
						}},
						{ConfigMap: &corev1.ConfigMapProjection{
							// The cluster CA, so TLS to the API server verifies.
							LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
							Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
						}},
						{DownwardAPI: &corev1.DownwardAPIProjection{
							Items: []corev1.DownwardAPIVolumeFile{{
								Path: "namespace",
								FieldRef: &corev1.ObjectFieldSelector{
									APIVersion: "v1",
									FieldPath:  "metadata.namespace",
								},
							}},
						}},
					},
				},
			},
		},
		{
			Name: "token-pubkey",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: s.TokenPubkeyConfigMapName()},
				},
			},
		},
		// The broker's reporting token, beside it in the platform namespace. Always
		// mounted: the Secret is always emitted, and a broker without it cannot report at
		// all, which is a failure that would appear only as missing notifications.
		{
			Name: "report-token",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: s.ReportTokenSecretName()},
			},
		},
	}
	// Gated on BrokerTLS, so the Secret's mere existence changes nothing. Turning this
	// on without the matching switch on the agent side would leave the broker serving
	// TLS to a bridge that speaks http.
	if s.BrokerTLS {
		volumes = append(volumes, corev1.Volume{
			Name: nameBrokerTLS,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: s.BrokerTLSSecretName()},
			},
		})
	}
	return volumes
}

// PlatformURL is where the broker reports a run's state.
//
// It is the control plane's in-cluster Service. A constant and not a parameter, because
// the Service name and its namespace are fixed by the chart, and a value that must agree
// on both sides is better spelled once than configured in two places.
const PlatformURL = "http://pestilence." + DefaultControlPlaneNamespace + ".svc:8080"

// MountReportToken is where the broker mounts its reporting token. The file inside it is
// TokenDataKey.
const MountReportToken = "/var/run/scarab/report-token"

// brokerEnv mirrors §6.4. The budget values are derived from the same Limits the
// ResourceQuota is built from, because the broker cannot read the quota itself:
// if these drift, the broker's accounting silently diverges from what the API
// server will actually admit.
//
// There is deliberately no model-credential variable here. The model key is
// supplied by the user in the session and persisted to <AgentDir>/auth.json on the
// workspace volume (§8.4), which workers read too, so the broker needs no Secret
// name to build worker PodSpecs.
func (s Spec) brokerEnv() []corev1.EnvVar {
	l := s.Limits
	env := []corev1.EnvVar{
		{Name: "SCARAB_WORKSPACE_SLUG", Value: s.Slug},
		{Name: "SCARAB_WORKSPACE_NAMESPACE", Value: s.Namespace()},
		{Name: "SCARAB_BROKER_PORT", Value: strconv.Itoa(int(s.BrokerPort))},
		{Name: "SCARAB_AGENT_IMAGE", Value: s.AgentImage},
		// NOT SCARAB_AGENT_SA_ROOT / _WORKER. The broker holds the tenant
		// ServiceAccount names in its own contract package and does not read them
		// from the environment, so passing them here would create a second source
		// of truth for a value that must be identical on both sides.
		{Name: "SCARAB_RESULT_ROOT", Value: ResultRoot},
		{Name: "SCARAB_POD_BUDGET", Value: strconv.Itoa(int(l.Pods))},
		{Name: "SCARAB_MEMORY_BUDGET", Value: l.RequestsMemory.String()},
		{Name: "SCARAB_CONTAINER_CPU_MAX", Value: l.ContainerCPUMax.String()},
		{Name: "SCARAB_CONTAINER_MEM_MAX", Value: l.ContainerMemoryMax.String()},
		{Name: "SCARAB_TOKEN_AUDIENCE", Value: s.BrokerServiceName()},
		// The token VERIFICATION key, which is a different thing from the
		// broker's own API token. The broker refuses to start without it.
		{Name: "SCARAB_TOKEN_PUBLIC_KEY", Value: tokenPubkeyPath},
		// Reporting. The broker is the only component that reports run state, so it is the
		// only one that needs to know where the control plane is and to hold a credential
		// for it.
		{Name: "SCARAB_PLATFORM_URL", Value: PlatformURL},
		{Name: "SCARAB_REPORT_TOKEN_PATH", Value: MountReportToken + "/" + TokenDataKey},
	}
	if s.BrokerTLS {
		env = append(env,
			corev1.EnvVar{Name: EnvBrokerTLSCert, Value: MountBrokerTLS + "/" + TLSKeyCert},
			corev1.EnvVar{Name: EnvBrokerTLSKey, Value: MountBrokerTLS + "/" + TLSKeyKey},
		)
	}
	return env
}

// ---------------------------------------------------------------------------
// §5.6 The root agent runtime (tenant namespace)
// ---------------------------------------------------------------------------

// rootAgentDeployment runs the bridge, which spawns `pi --mode rpc` as a child.
//
// Class: runtime. Created once at provisioning and then owned by the tenant, so a
// tenant that replaces the default landing page is not fought by the reconciler.
func (s Spec) rootAgentDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: s.meta(NameRootDeployment, ComponentRoot),
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			// Recreate is REQUIRED, not a preference: openebs-hostpath is RWO, so
			// the default RollingUpdate would start a second pod that cannot
			// attach the volume while the first holds it, and the rollout would
			// hang indefinitely.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				LabelWorkspace: s.Slug,
				LabelComponent: ComponentRoot,
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: s.labels(ComponentRoot)},
				Spec:       s.rootAgentPodSpec(),
			},
		},
	}
}

func (s Spec) rootAgentPodSpec() corev1.PodSpec {
	volumes, mounts := s.rootAgentStorage()
	return corev1.PodSpec{
		// Required by the ValidatingAdmissionPolicy: a pod that omits
		// serviceAccountName would default to `default`, which mounts an API token.
		ServiceAccountName:            SARoot,
		AutomountServiceAccountToken:  boolPtr(false),
		SecurityContext:               restrictedPodSecurityContext(UIDAgent),
		TerminationGracePeriodSeconds: int64Ptr(30),
		Containers:                    []corev1.Container{s.rootAgentContainer(mounts)},
		Volumes:                       volumes,
	}
}

// rootAgentStorage returns the volumes and their mounts TOGETHER, because they
// must stay in step: a volume with no mount is dead weight, and a mount with no
// volume fails at admission.
func (s Spec) rootAgentStorage() ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := []corev1.Volume{
		{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: NameStorage},
			},
		},
		{Name: "scratch", VolumeSource: sizedEmptyDir(scratchSizeLimit)},
		{Name: "tmp", VolumeSource: sizedEmptyDir(tmpSizeLimit)},
		{
			Name: "cap-token",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: s.TokenSecretName,
					// 0400. The 0444 this replaces was a workaround for a missing fsGroup,
					// not a requirement.
					//
					// The original problem was real: the kubelet leaves Secret files owned
					// by root, the container runs as a non-root uid, and 0400 made the
					// token unreadable, so agents_spawn failed with "capability token is
					// not available". Widening it to 0444 fixed that by making the token
					// readable by ANY process in the pod -- which the red team flagged,
					// correctly: this token authorizes broker calls, and "any process in
					// the pod" includes a worker.
					//
					// fsGroup on the pod is the actual fix, and the mechanism is narrower
					// than it sounds. fsGroup ORs in the GROUP bits and does not clear the
					// world bits, so 0400 becomes an effective 0440 -- owner and group can
					// read, the world cannot. Verified live: the mounted file reads
					// 440 root:<pod group>. 0400 is therefore doing real work here and
					// must not be raised to 0444 again to fix a readability problem; the
					// fsGroup is what grants readability.
					DefaultMode: int32Ptr(0o400),
				},
			},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: MountWorkspace},
		{Name: "scratch", MountPath: MountScratch},
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "cap-token", MountPath: MountToken, ReadOnly: true},
	}
	if s.AssertionPublicKeyPEM != "" {
		volumes = append(volumes, s.assertionPubkeyVolume())
		mounts = append(mounts, corev1.VolumeMount{Name: nameAssertionPubkey, MountPath: assertionPubkeyDir, ReadOnly: true})
	}
	// The broker's certificate, for the bridge to verify the broker with. Items
	// restricts the mount to ca.crt ALONE, so the agent never has the broker's private
	// key even though both live in the same Secret -- the mount is the boundary here,
	// not the Secret.
	if s.BrokerTLS {
		// A ConfigMap in THIS namespace, not the Secret in the platform one. A Secret
		// volume reference is namespace-local, so pointing at that Secret does not fail
		// to verify -- it fails to mount, and the pod waits in ContainerCreating.
		volumes = append(volumes, corev1.Volume{
			Name: nameBrokerCA,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: s.BrokerCAName()},
					Items:                []corev1.KeyToPath{{Key: TLSKeyCA, Path: TLSKeyCA}},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: nameBrokerCA, MountPath: MountBrokerCA, ReadOnly: true})
	}
	if s.WithMemoryPVC {
		volumes = append(volumes, corev1.Volume{
			Name: "memory",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: NameMemory},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "memory", MountPath: MountMemory})
	}
	return volumes, mounts
}

// assertionPubkeyVolume publishes town's public key into the workspace's namespace.
//
// Read-only from a ConfigMap, and published rather than fetched: the bridge verifies an
// Ed25519 signature on every request, and a network round trip to fetch a key that never
// changes would add a dependency and a cache to a path that has neither.
func (s Spec) assertionPubkeyVolume() corev1.Volume {
	return corev1.Volume{
		Name: nameAssertionPubkey,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: nameAssertionPubkey},
			},
		},
	}
}

// assertionPubkeyConfigMap is the object the volume above mounts.
//
// ClassBoundary, not ClassPlatform: it lives in the TENANT namespace and is reconciled,
// even though what it carries belongs to the platform.
func (s Spec) assertionPubkeyConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: s.meta(nameAssertionPubkey, ComponentRoot),
		Data:       map[string]string{AssertionPublicKeyKey: s.AssertionPublicKeyPEM},
	}
}

func (s Spec) rootAgentContainer(mounts []corev1.VolumeMount) corev1.Container {
	return corev1.Container{
		Name:            ContainerBridge,
		Image:           s.AgentImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/usr/local/bin/scarab-bridge"},
		Args: []string{
			"--port", strconv.Itoa(int(s.HTTPPort)),
			"--session-dir", SessionDir,
		},
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: s.HTTPPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env:             s.bridgeEnv(),
		SecurityContext: restrictedSecurityContext(UIDAgent),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(s.HTTPPort)},
			},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(s.HTTPPort)},
			},
		},
		VolumeMounts: mounts,
	}
}

// bridgeEnv mirrors §6.4.
func (s Spec) bridgeEnv() []corev1.EnvVar {
	// The scheme follows Spec.BrokerTLS, and the trust anchor below follows it too, so
	// the two cannot disagree. A bridge told to use https without a CA would fail every
	// call; a bridge left on http against a TLS broker would never connect.
	scheme := "http"
	if s.BrokerTLS {
		scheme = "https"
	}
	env := []corev1.EnvVar{
		{Name: "SCARAB_BRIDGE_PORT", Value: strconv.Itoa(int(s.HTTPPort))},
		{Name: "SCARAB_BROKER_URL", Value: scheme + "://" + s.BrokerServiceName() + "." + s.PlatformNamespace + ".svc.cluster.local:" + strconv.Itoa(int(s.BrokerPort))},
		{Name: "SCARAB_WORKSPACE_SLUG", Value: s.Slug},
		{Name: "SCARAB_WORKSPACE_DIR", Value: MountWorkspace},
		{Name: "SCARAB_MEMORY_DIR", Value: MountMemory},
		{Name: "SCARAB_SCRATCH_DIR", Value: MountScratch},
		{Name: "SCARAB_TOKEN_PATH", Value: MountToken + "/token"},
		{Name: "SCARAB_RESULT_ROOT", Value: ResultRoot},
		// The workspace's public hostname. The bridge rejects a request whose Host is
		// not this, and a WebSocket Origin outside it (scarab §8.8). Leaving it unset
		// does not fail closed -- it disables both checks and logs a warning -- so an
		// omission here is a silently weaker edge rather than a broken one.
		{Name: "SCARAB_HOSTNAME", Value: s.Hostname},
		// Where the bridge finds town's public assertion key. Published into this
		// namespace because a ConfigMap cannot be mounted across namespaces, and the
		// control plane is the only party that may distribute it -- town holds no
		// Kubernetes credential at all, by design.
		{Name: "SCARAB_ASSERTION_PUBKEY", Value: assertionPubkeyPath},
		{Name: "PI_SESSION_DIR", Value: SessionDir},
		{Name: "PI_CODING_AGENT_DIR", Value: AgentDir},
	}
	if s.BrokerTLS {
		env = append(env, corev1.EnvVar{Name: EnvBrokerCA, Value: MountBrokerCA + "/" + TLSKeyCA})
	}
	return env
}

// ---------------------------------------------------------------------------
// §8.2 The Ingress
// ---------------------------------------------------------------------------

// ingress publishes the workspace endpoint.
//
// Class: endpoint. Reconciled, because the platform guarantees the workspace has
// a reachable HTTPS endpoint.
//
// There is deliberately NO secretName and NO cert-manager annotation. The
// wildcard certificate is Traefik's default certificate, and naming a secret here
// would require replicating the wildcard private key into the tenant namespace,
// where any agent process could read it -- and a leaked wildcard key lets its
// holder impersonate every workspace on the domain.
//
// Auth: unauthenticated for the MVP (§8.8). ForwardAuth -> pestilence is the
// target gate; the object is written so it can be added later as one annotation.
//
// The edge is Traefik middlewares owned by my-opps and referenced by name here:
//
//	strip-untrusted-user  removes any client-supplied X-Auth-User before ForwardAuth
//	                     sets it (see TraefikWorkspaceMiddlewares for the ordering)
//	security-headers     HSTS and the standard headers. Without HSTS a browser will
//	                     silently downgrade to http:// on a later visit.
//	forwardauth          asks town, and copies the identity onto the upstream request
//
// There was briefly an IP allowlist here, admitting only the LAN. It was a stopgap for
// the exposure the first red-team review found -- the design claimed an allowlist
// existed and none ever had, so the bridge was reachable from the public internet -- and
// it was removed once ForwardAuth could carry the load, because a workspace should be
// reachable from anywhere by the member who owns it.
//
// Referenced by annotation rather than inlined so the values live in one place. If
// a middleware is renamed, Traefik fails closed; it does not silently stop
// applying it.
//
// Neither is authentication, and neither should be mistaken for it. They reduce who
// can reach the bridge to the LAN; the bridge still accepts anyone on the LAN, and
// closing that is ForwardAuth plus a token gate in the bridge itself (scarab).
func (s Spec) ingress() *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	meta := s.meta(s.Slug, ComponentRoot)
	meta.Annotations = map[string]string{
		AnnotationTraefikMiddlewares: TraefikWorkspaceMiddlewares,
	}
	return &networkingv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: meta,
		Spec: networkingv1.IngressSpec{
			IngressClassName: strPtr(DefaultIngressClass),
			Rules: []networkingv1.IngressRule{{
				Host: s.Hostname,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: NameRootService,
									Port: networkingv1.ServiceBackendPort{Number: s.HTTPPort},
								},
							},
						}},
					},
				},
			}},
			TLS: []networkingv1.IngressTLS{{
				Hosts: []string{s.Hostname},
			}},
		},
	}
}

func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
