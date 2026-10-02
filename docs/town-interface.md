# The town ↔ pestilence interface

town is the management UI, its BFF, and **the platform's identity provider**. This file is
the contract between the two: a value both sides must agree on appears here and nowhere
else, because a value invented on one side is how the two sides drift apart.

**Status: built and live on both sides.** town signs the assertion; pestilence verifies it
on every `/api` route, and the workspace edge carries the ForwardAuth chain plus the
bridge's own signed-assertion gate.

---

## The decision, and why it is shaped this way

town authenticates users with **GitHub OAuth**, limited to members of one organisation.

The original plan — town forwards the IdP's access token and pestilence verifies it against
a JWKS — **is impossible**, because GitHub is not an OpenID Connect provider:

> GitHub does not currently implement OpenID Connect in its OAuth flows and does not issue
> ID tokens for users or apps. — docs.github.com/en/apps/github-authentication-discovery-endpoints

(There is a discovery document at `github.com/login/oauth/.well-known/openid-configuration`.
GitHub says it is published only for MCP clients, is in public preview, and contains
incorrect information. It is a decoy.)

A GitHub access token is an opaque `gho_…` string that only GitHub can validate. So the
options were:

| Option | Verdict |
|---|---|
| pestilence calls GitHub to validate each token | Rejected. Opens the trusted plane's egress to the internet, makes its availability depend on GitHub, and hands it a `read:org` credential on the whole org when all it needs is one opaque owner id. |
| **town signs an assertion pestilence verifies locally** | **Chosen.** No external dependency in the trusted plane, egress stays closed, and the GitHub token never leaves town. |
| town asserts an identity over the network, unsigned | Rejected. The only gate would be a NetworkPolicy, which is not a cryptographic control. |

So **town is the identity provider to pestilence.** pestilence trusts town, which is
consistent with both being in the same cluster scope and equally trusted — and unlike an
unsigned header, it survives a mistake in the network policy.

The GitHub access token is used once, at login, and dropped. It is never forwarded and is
deliberately absent from the session cookie.

---

## Flow 1: town → pestilence API

```
browser ──session cookie──► town ──Authorization: Bearer <assertion>──► pestilence
```

For **every** API request, town mints a fresh assertion for the signed-in user and attaches
it. pestilence verifies it locally and trusts nothing else about the request — in particular
it does **not** accept a header naming the user, because anything that can reach it could
forge one.

### The assertion

An Ed25519 JWT, signed with town's private key and verified with its public key.

| Claim | Value |
|---|---|
| `alg` (header) | `EdDSA` |
| `iss` | `town` |
| `aud` | `pestilence-api` |
| `sub` | `github#<numeric id>` — the workspace owner |
| `iat`, `exp` | 120 seconds apart |

Ed25519 rather than a shared secret so that the private key exists in exactly one place,
and so pestilence's copy of the public key can live in a **ConfigMap** — a public key is
not sensitive.

It is short-lived and minted per request rather than per session: it is a few bytes and a
signature, and a per-session token would need storing, refreshing and revoking on both
sides. A leaked assertion is worth under two minutes.

### What pestilence must validate

| Check | Detail |
|---|---|
| algorithm | pinned to `EdDSA`. Not inferred: accepting whatever the token claims is the classic JWT confusion bug |
| signature | against town's public key |
| `iss` | exactly `town` |
| `aud` | exactly `pestilence-api` |
| `exp`, `iat` | with ~60s leeway for clock skew |
| `sub` | becomes the workspace owner |

### Status codes

| Situation | Response |
|---|---|
| no `Authorization` header | `401` + `WWW-Authenticate: Bearer` |
| bad signature, expired, wrong `iss`/`aud` | `401` |
| valid assertion, caller does not own the target | `403` |
| unknown workspace | `404` |
| `GET /healthz` | **unauthenticated**, always `200` |

`/healthz` must stay unauthenticated, and not only for probes: town's `/readyz` calls it to
decide whether it can serve, and readiness has no user to mint an assertion for.

Error bodies keep the existing shape, which town parses as-is:

```json
{"error": {"code": "unauthorized", "message": "..."}}
```

### The owner identifier

```
ownerId = github#<github numeric user id>
```

**Not the login.** GitHub usernames are mutable and the freed name is claimable:

> After changing your username, your old username becomes available for anyone else to
> claim. — docs.github.com/account-and-profile/concepts/username-changes

Keying workspaces on the login would mean a rename loses you your workspaces, and whoever
claims the freed name **inherits them**. The numeric id is immutable.

This re-owns nothing: existing records say `ownerId: "local"` and will not match any
assertion. That is deliberate — a silent re-owning would be worse.

---

## Flow 2: tenant endpoints, via Traefik ForwardAuth

The tenant-facing endpoint is `https://<slug>.gobackto.work`, served by that workspace's
Pi agent. town is the ForwardAuth service; pestilence answers the ownership question:

```
POST /api/authorize          Authorization: Bearer <town assertion>
{"hostname": "golden-vole-6w4q.gobackto.work"}

200 {"user": "<ownerId>", "slug": "...", "namespace": "ws-..."}
404 no workspace serves that hostname, or the workspace is not RUNNING
401 no assertion at all
```

**The org is the boundary, not the individual.** Any authenticated member may open any
workspace; there is no ownership check on this route. Authentication still is one -- an
anonymous request never reaches the decision.

Stated plainly, because "read visibility" undersells it: **there is no read-only view of a
workspace.** Opening one reaches its agent, its files, and the model credential on its
volume, so a member who opens a colleague's workspace spends that colleague's model credits
against their key. Anyone who may open it may use it.

What is NOT shared: **deletion and retry stay owner-only.** Opening a colleague's workspace
is collaboration; destroying it is not. The list is unfiltered for the same reason the
authorize route is unowned -- every member sees every workspace, and `ownerId` travels with
each record so the UI knows which ones offer actions. Offering Delete on someone else's row
would be a button that answers 403.

A workspace that is not `RUNNING` returns 404: the endpoint genuinely does not exist yet,
and 403 would imply the caller could be granted something that is not there.

The hostname is ATTACKER-CONTROLLED even though it arrives from town, because Traefik
passes the ORIGINAL request's `Host` through. So the mapping is strict -- strip a numeric
port, require the configured suffix, validate the remaining slug -- and every rejection
returns the same 404 as an unknown workspace, so a probe cannot tell the two apart.

### Why the obvious design does not work

The obvious design is for ForwardAuth to send the original request to town, which reads
`town_session` and answers. **It cannot.** That cookie is host-only for
`town.gobackto.work`, so a browser requesting `<slug>.gobackto.work` never sends it, and
Traefik arrives at town with no cookie and no way to say who the user is. Attaching the
middleware without solving this denies every workspace request -- correct fail-closed, and
a total outage.

The obvious fix -- widening the session cookie to `Domain=.gobackto.work` -- is worse than
it looks. Tenant workspaces are SIBLING subdomains serving tenant-controlled content, so
widening hands every tenant a cookie they can shadow, and one tenant can then log out every
other user. It also makes a `__Host-` prefix impossible, and that prefix is the attribute
designed to prevent exactly this.

### What Flow 2 actually does

The workspace edge gets its **own credential, bound to one hostname**:

```
1. browser -> <slug>.gobackto.work          no edge cookie
2. ForwardAuth -> town /authz              no cookie, no handoff
3. town 302s the BROWSER to its own hostname, where the session cookie IS sent
4. town /workspaces/open calls /api/authorize, mints a 2-minute grant with
   aud = the hostname, and 302s back to the workspace with it in the query
5. ForwardAuth -> town /authz              handoff present: verified, exchanged for a
                                           HOST-SCOPED cookie, and 302d to the same URL
                                           with the handoff stripped
6. browser -> the workspace again, now carrying that cookie
7. ForwardAuth -> town /authz              cookie valid -> 2xx + the headers below
```

Step 5 is what makes this work with **no new route on the workspace host**, and it is a
behaviour that had to be verified rather than assumed: Traefik passes a 3xx from the auth
server through to the browser *together with its `Set-Cookie`*, and a cookie with no
`Domain` attribute is scoped by the browser to the host that was asked for. That is how
town sets a cookie on a host it does not serve.

### The headers town sets

| Header | Meaning |
|---|---|
| `X-Scarab-Assertion` | **The authorization input.** An Ed25519 JWT, minted per request, `aud` = the workspace hostname, `sub` = the owner id, `iss` = `town`. Verified by the bridge against `ConfigMap/town-assertion-pubkey` |
| `X-Auth-User` | The owner id, for display. **Unsigned, and not an authorization input** -- the contract says scarab does not consume it, and it is stripped from client input before ForwardAuth runs so it cannot be forged |

**Two signature schemes, deliberately.** The handoff and the edge cookie travel through the
BROWSER, so town mints and verifies them itself and HS256 is sufficient. The assertion is
verified by a THIRD PARTY and never passes through the browser, so it is Ed25519 with the
key town already uses for pestilence. Only the audience differs, so one public key verifies
both.

### The middleware chain, in order

```
strip-untrusted-user, security-headers, forwardauth
```

**There is no IP allowlist**, and that is a deliberate decision rather than an omission: the
gate is who you are, not where you are. `my-opps/cluster/manifests/traefik-middlewares.yaml`
records why, and the consequence -- `forwardauth` is now the only gate, and Traefik serves a
router *without* a middleware whose name does not resolve.

**Order is security-relevant.** `strip-untrusted-user` must run *before* `forwardauth`:
after it, it would delete the `X-Auth-User` ForwardAuth had just set and the bridge would
silently see nobody. Asserted as a whole ordered string in pestilence's tests for that
reason -- a set-membership check would pass with the two swapped.

### Status

Built and verified together on the reference cluster: `/api/authorize` and the Ingress
middleware chain on pestilence's side; `/authz`, the handoff, the host-scoped cookie and
the assertion on town's. Both workspace Ingresses carry
`strip-untrusted-user,security-headers,forwardauth` in that order.

---

## Contract values

| Value | Who sets it | Notes |
|---|---|---|
| `iss` | town | exactly `town` |
| `aud` | both, via configuration | `pestilence-api`; town: `ASSERTION_AUDIENCE`, pestilence: its own flag |
| `sub` | town | `github#<numeric id>` |
| assertion TTL | town | 120s; pestilence allows ~60s of skew |
| the bearer header | town sends, pestilence reads | `Authorization: Bearer <assertion>` |
| signing algorithm | town | `EdDSA`; pestilence pins it |
| public key | operator | `ConfigMap/town-assertion-pubkey` in the `pestilence` namespace, made by town's `cluster-setup-scripts/generate-keys.sh` |
| error body shape | pestilence | `{"error":{"code","message"}}` |

The key pair is generated once and both halves are deployed. **Replacing one without the
other stops every request** with a signature failure.

---

## What each side implements

**town — done:** the OAuth flow with a CSRF nonce; the org membership check (requiring
`state == "active"`, so a *pending* invitation is refused); a signed stateless session
cookie; the Ed25519 assertion, minted per request and per audience; the key generator; and
the whole of Flow 2's edge — `/authz`, the handoff, the host-scoped cookie, and both
headers.

**pestilence — done:** the public key loaded from the ConfigMap; bearer validation on every
`/api` route except `/healthz`; `ownerId` taken from `sub`, replacing the `-owner` flag;
ownership checks on get/delete/retry; `POST /api/authorize` for Flow 2; the workspace
Ingress's middleware chain; and `SCARAB_HOSTNAME` on the root agent, without which the
bridge's Host and Origin checks are disabled.

**Both gates are live.** The bridge verifies `X-Scarab-Assertion` for itself, using the key
pestilence publishes into the tenant namespace, so `forwardauth` is no longer the only gate
on a workspace endpoint. The full chain is specified in
[`scarab/docs/architecture.md`](https://github.com/gobackto-work/scarab/blob/main/docs/architecture.md) §8.8.
