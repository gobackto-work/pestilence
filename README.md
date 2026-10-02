# pestilence

**Trusted platform plane** for a self-hosted Pi agent platform on k0s.

Provisions temporary, authenticated Pi agent workspaces on an existing
Kubernetes cluster: one namespace per workspace, one root Pi agent, bounded
resources, persistent storage, and a per-workspace broker through which the
agent requests additional subagents.

## The security model

> **The tenant namespace is the unit of compromise.**

Anything running inside a tenant workspace is assumed able to attack everything
else inside that same workspace. Compromise of a tenant must **not** imply:

- Kubernetes administrative access
- host access
- access to another tenant
- access to the platform control plane
- access to arbitrary home-LAN services
- the ability to alter its own quota, policy, or authorization
- persistent access after workspace deletion

The design consequence is that **no Kubernetes credential ever enters a Pi
container**. The root agent and its workers reach Kubernetes only through a
per-workspace broker (`scarab`), which exposes intent-level operations
(`spawnAgent`) rather than anything resembling `kubectl`.

## Repository roles

| Repo | Role |
|---|---|
| **pestilence** (this repo) | Trusted platform plane: workspace lifecycle, provisioner, authentication, audit. The only component with cross-namespace Kubernetes privilege. |
| [`town`](https://github.com/gobackto-work/town) | Management UI for the control plane: sign-in, workspace list/create/delete, inspection. |
| [`scarab`](https://github.com/gobackto-work/scarab) | Tenant/agent side: agent broker, Pi agent image, Pi `platform-tools` extension, and the workspace UI. Namespace-scoped privilege only. |
| [`my-opps`](https://github.com/gobackto-work/my-opps) | Cluster build and cluster-level configuration. |

## Current state

The first slice is the **workspace security bundle** — the objects that must
exist *before* any tenant workload starts. It is the load-bearing part: if this
is wrong, nothing above it can be secure.

```
internal/tenant/
  slug.go         slug validation and generation (a security boundary, not formatting)
  spec.go         per-workspace specification and resource limits
  class.go        boundary / endpoint / runtime — how the provisioner treats each object
  bundle.go       renders Namespace, ResourceQuota, LimitRange, NetworkPolicies,
                  ServiceAccounts, broker Role/RoleBinding, PVCs, Services
  bundle_test.go  the security invariants as executable checks
internal/provisioner/
  reconciler.go   idempotent Ensure, ordered Delete, drift inspection
cmd/render-bundle  print a bundle as YAML for review or live validation
```

### The provisioner

`Ensure` server-side-applies boundary and endpoint objects, so drift is
corrected, and creates runtime objects only when absent — never resurrecting one
the tenant replaced. `Delete` follows a documented order that revokes authority
before destroying compute, and it is idempotent.

The deletion order is exported (`DeletePlan`) and asserted in tests, because it is
a security property rather than an implementation detail. One step is easy to
miss and is called out explicitly: the broker's identity lives in the `scarab`
namespace, so deleting the tenant namespace does **not** remove it.

Implemented: the control-plane API and its SQLite store, the provisioner and the
reconcile loop (including a drift sweep), GitHub authentication, ingress
provisioning for each workspace's endpoint, and the ForwardAuth middleware chain
that gates every tenant endpoint.

Not yet implemented: audit logging. Nothing records who did what; the API is
otherwise complete.

## Invariants are tests

Each test in `internal/tenant/bundle_test.go` corresponds to a claim the
platform makes. They are the reason this package is worth reviewing closely.

| Test | Claim |
|---|---|
| `TestBundleRejectsInvalidSlug` | A slug can never inject a path or an invalid hostname into a namespace or URL |
| `TestNamespaceEnforcesRestrictedPSA` | Pod Security Admission `restricted` is enforced by namespace label |
| `TestServiceAccountsNeverMountTokens` | No tenant identity holds a Kubernetes API token |
| `TestEveryObjectIsNamespacedToTheTenant` | The bundle cannot leak objects into another namespace |
| `TestNamespaceIsTheOnlyClusterScopedObject` | No ClusterRole or ClusterRoleBinding is ever emitted |
| `TestEgressCannotReachPrivateRanges` | Tenants cannot reach RFC1918, loopback, or link-local ranges |
| `TestHomeLANIsExplicitlyExcluded` | The home LAN specifically is unreachable |
| `TestNoRuleMatchesEveryPeer` | No policy rule matches every source or destination |
| `TestEgressNamespaceAllowlist` | Egress targets only `kube-system` and the broker namespace |
| `TestDefaultDenyCoversBothDirections` | Both directions are denied before anything is permitted |
| `TestBrokerRoleExcludesSecurityControls` | The broker cannot touch quotas, policies, RBAC, or ServiceAccounts |
| `TestBrokerRoleBindingUsesAnIdentityOutsideTheTenant` | The tenant cannot assume the broker identity |
| `TestQuotaBoundsEveryDimension` | Every resource dimension is bounded |
| `TestPodCountIsBounded` | A workspace cannot run unbounded agents |
| `TestLimitRangeSetsDefaultsAndCeiling` | Defaults exist and one container cannot eat the workspace |

### Validated live, not only in unit tests

The bundle was applied to the real cluster and checked against the API server:

- All 15 objects were accepted, and the `ipBlock` for internet egress retained
  all six excluded private ranges.
- The broker `RoleBinding` subject resolved to `scarab/<slug>-broker` — outside
  the tenant namespace, so the tenant cannot reference it.
- A compliant pod was **accepted**, confirming the namespace is actually usable
  rather than merely locked down.
- These were **rejected by admission**: `privileged: true`, `hostNetwork: true`,
  a `hostPath` volume, a container with no security context (implicitly root),
  and a pod requesting 4 CPUs against a 2-CPU quota.

The last group matters most. It shows the boundary is enforced by the API
server, so it holds *even if the broker is bypassed entirely* — which is exactly
what makes it a defence-in-depth layer rather than a convention.

## Build and test

```bash
go test ./...
go vet ./...
```

## Validate against a live cluster

`render-bundle` emits exactly what the provisioner will apply, so it can be
checked against the real API server:

```bash
go run ./cmd/render-bundle -slug demo | kubectl apply --dry-run=server -f -
```

## Platform placement

`pestilence` holds only `bind` on the pre-created `ClusterRole/pestilence-tenant`
and `ClusterRole/pestilence-platform`, plus the cluster-scoped rights to create
namespaces and RoleBindings. Its reach into a tenant namespace follows the
RoleBinding it creates there, so it reaches only the namespaces it provisioned. It
must never hold `cluster-admin` or wildcard verbs. The broker's identity lives in
`scarab`, not in the tenant namespace, so a tenant cannot reference it at all —
Kubernetes ServiceAccount references are namespace-local.

## Related documentation

- [`docs/town-interface.md`](docs/town-interface.md) — the town ↔ pestilence contract: town holds the identity, pestilence verifies the token, and the values both sides must agree on
- [`docs/verification.md`](docs/verification.md) — what `hack/verify.sh` checks, and why each check earns its place
- [`docs/security-hardening.md`](docs/security-hardening.md) — the privilege reduction, and the real weaknesses deliberately deferred, each with enough detail to pick it up without re-deriving it
- [`scarab/docs/architecture.md`](https://github.com/gobackto-work/scarab/blob/main/docs/architecture.md) — the normative pestilence ↔ scarab interface: every name, label, port, mount path, environment variable and PodSpec template that crosses the boundary
- [`my-opps/cluster/README.md`](https://github.com/gobackto-work/my-opps/blob/main/cluster/README.md) — cluster build and the two CNI traps
