// Package auth verifies the assertions town presents to the control plane.
//
// The control plane has no login flow and no session store. town runs the GitHub
// OAuth flow, and for every API request it mints a short-lived Ed25519 assertion
// naming the workspace owner. This package checks that assertion and nothing else.
//
// WHY TOWN AND NOT GITHUB: GitHub is OAuth 2.0, not OpenID Connect, and issues no
// ID token -- so there was never anything for this side to verify for itself. The
// alternative, calling GitHub to validate each token, would open the trusted
// plane's egress to the internet and make its availability depend on GitHub. See
// docs/town-interface.md for the full reasoning.
package auth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Contract values, agreed with town in docs/town-interface.md. Changing one here
// without changing it there stops every request, and the failure looks like a
// signature problem rather than a mismatch.
const (
	// Issuer must be exactly this.
	Issuer = "town"

	// Audience must be exactly this: a dedicated audience rather than town's
	// client id, so the assertion is explicitly scoped to this API.
	Audience = "pestilence-api"

	// Algorithm is pinned. See the note on WithValidMethods below.
	Algorithm = "EdDSA"

	// DefaultLeeway absorbs clock skew between town and the control plane.
	DefaultLeeway = 60 * time.Second
)

// ErrUnauthorized is returned for every reason an assertion is not acceptable.
//
// Deliberately one error rather than a taxonomy: the caller's only possible
// response is 401, and distinguishing "expired" from "wrong audience" in a response
// tells an attacker which half of a forged token to fix next.
var ErrUnauthorized = errors.New("unauthorized")

// Verifier checks assertions signed by town.
type Verifier struct {
	key      ed25519.PublicKey
	issuer   string
	audience string
	leeway   time.Duration
}

// NewVerifier returns a Verifier. Empty issuer, audience or leeway take the
// contract defaults, so a caller cannot accidentally accept any issuer by
// forgetting to set one.
func NewVerifier(key ed25519.PublicKey, issuer, audience string, leeway time.Duration) (*Verifier, error) {
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("assertion public key is %d bytes, want %d", len(key), ed25519.PublicKeySize)
	}
	if issuer == "" {
		issuer = Issuer
	}
	if audience == "" {
		audience = Audience
	}
	if leeway == 0 {
		leeway = DefaultLeeway
	}
	return &Verifier{key: key, issuer: issuer, audience: audience, leeway: leeway}, nil
}

// Verify returns the owner id carried by the assertion, or ErrUnauthorized.
//
// The owner id is the `sub` claim verbatim. It is NOT validated as a GitHub id:
// this side has no business knowing where the identity came from, and a check on
// the format would couple the control plane to one identity provider.
func (v *Verifier) Verify(assertion string) (string, error) {
	if assertion == "" {
		return "", ErrUnauthorized
	}

	var claims jwt.RegisteredClaims
	parsed, err := jwt.ParseWithClaims(assertion, &claims, func(*jwt.Token) (any, error) {
		return v.key, nil
	},
		// The important one. Without it the parser will accept whatever algorithm
		// the token claims, which is the shape of the classic HS256-for-RS256
		// confusion attack -- and with a public key as the HMAC secret, a forged
		// token verifies. Pinning it means any other algorithm is simply refused.
		jwt.WithValidMethods([]string{Algorithm}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		// An assertion with no expiry would be valid forever.
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(v.leeway),
	)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if !parsed.Valid || claims.Subject == "" {
		return "", ErrUnauthorized
	}
	return claims.Subject, nil
}

// LoadPublicKeyPEM reads the PKIX PEM Ed25519 public key from a path, and returns the
// PEM alongside it.
//
// Read once at startup rather than per request: the key is a deployment artefact, and
// re-reading it in the request path would add a file read and a parse to every call for
// no benefit. Rotating the key therefore needs a restart, which is what town's key
// generator prints instructions for.
//
// The BYTES matter as much as the key. This key is not only verified against -- it is
// PUBLISHED into every tenant namespace, because the bridge there verifies town for
// itself. The control plane is the only party that may distribute it: town holds no
// Kubernetes credential at all by design, so it cannot write the copy its own consumer
// needs.
func LoadPublicKeyPEM(path string) (ed25519.PublicKey, string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator configuration, not user input
	if err != nil {
		return nil, "", fmt.Errorf("read assertion public key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, "", fmt.Errorf("assertion public key at %s is not PEM", path)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("parse assertion public key: %w", err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		// A plausible mistake is deploying the RSA key from somewhere else, and
		// saying so beats a signature failure on every request.
		return nil, "", fmt.Errorf("assertion public key at %s is %T, want ed25519.PublicKey", path, parsed)
	}
	return key, string(raw), nil
}
