package tenant

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func materialSpec() Spec {
	s := fullSpec()
	s.TokenTTL = 24 * time.Hour
	return s
}

// verifyAt verifies with a controlled clock, so a token minted at a fixed time
// does not read as "not valid yet" or "expired" depending on when the test runs.
func verifyAt(t *testing.T, token string, pub ed25519.PublicKey, at time.Time) jwt.MapClaims {
	t.Helper()
	claims, err := VerifyToken(token, pub, jwt.WithTimeFunc(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	return claims
}

func TestMintedTokenCarriesThePinnedClaims(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	spec := materialSpec()

	mat, err := NewSigningMaterial(spec, now)
	if err != nil {
		t.Fatalf("NewSigningMaterial: %v", err)
	}
	pub, err := ParsePublicKey(mat.PublicKeyPEM)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	claims := verifyAt(t, mat.Token, pub, now)

	slug := spec.Normalized().Slug
	want := map[string]string{
		"iss":       TokenIssuer,
		"sub":       "pi-root@" + Namespace(slug),
		"workspace": slug,
		"namespace": Namespace(slug),
		"role":      TokenRoleRoot,
	}
	for k, v := range want {
		if got, _ := claims[k].(string); got != v {
			t.Errorf("claim %q = %q, want %q", k, got, v)
		}
	}

	// The audience is a set and not a string, because the token is accepted at the
	// broker and at the control plane's own ingest endpoint. The broker checks
	// membership (jwt.WithAudience), so membership is what is pinned here. This claim is
	// read by the broker in the other repository, and the shape changed when the second
	// audience was added.
	broker := spec.Normalized().BrokerServiceName()
	named := false
	for _, aud := range audiences(t, claims) {
		if aud == broker {
			named = true
		}
	}
	if !named {
		t.Errorf("the token does not name the broker %q, so it would refuse it. audiences: %v",
			broker, audiences(t, claims))
	}
	if claims["jti"] == "" || claims["jti"] == nil {
		t.Error("jti must be set")
	}

	// Time claims. The broker requires exp.
	exp, ok := claims["exp"].(float64)
	if !ok {
		t.Fatalf("exp is %T, want a number; the broker requires it", claims["exp"])
	}
	if got := time.Unix(int64(exp), 0); !got.Equal(now.Add(spec.TokenTTL)) {
		t.Errorf("exp = %s, want %s", got, now.Add(spec.TokenTTL))
	}
	for _, k := range []string{"iat", "nbf"} {
		v, ok := claims[k].(float64)
		if !ok {
			t.Errorf("%s is %T, want a number", k, claims[k])
			continue
		}
		if got := time.Unix(int64(v), 0); !got.Equal(now) {
			t.Errorf("%s = %s, want %s", k, got, now)
		}
	}
}

// The contract pins EdDSA only. A token signed with anything else must not
// verify, or the HS256/"none" confusion attacks come back.
func TestTokenHeaderPinsEdDSA(t *testing.T) {
	spec := materialSpec()
	mat, err := NewSigningMaterial(spec, time.Now())
	if err != nil {
		t.Fatalf("NewSigningMaterial: %v", err)
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(mat.Token, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	if alg, _ := parsed.Header["alg"].(string); alg != "EdDSA" {
		t.Errorf("alg = %q, want EdDSA", alg)
	}
	if typ, _ := parsed.Header["typ"].(string); typ != "JWT" {
		t.Errorf("typ = %q, want JWT", typ)
	}
}

func TestTokenDoesNotVerifyWithAnotherWorkspacesKey(t *testing.T) {
	now := time.Now()
	a, err := NewSigningMaterial(materialSpec(), now)
	if err != nil {
		t.Fatalf("material a: %v", err)
	}
	other := materialSpec()
	other.Slug = "other-wombat-aaaa"
	b, err := NewSigningMaterial(other, now)
	if err != nil {
		t.Fatalf("material b: %v", err)
	}
	pubB, err := ParsePublicKey(b.PublicKeyPEM)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if _, err := VerifyToken(a.Token, pubB, jwt.WithTimeFunc(func() time.Time { return now })); err == nil {
		t.Error("a token verified against a different workspace's public key")
	}
}

// The keypair must survive a PEM round trip, because the reconciler stores it and
// re-mints from it later.
func TestSigningMaterialRoundTripsThroughPEM(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	spec := materialSpec()
	mat, err := NewSigningMaterial(spec, now)
	if err != nil {
		t.Fatalf("NewSigningMaterial: %v", err)
	}

	priv, err := ParsePrivateKey(mat.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	pub, err := ParsePublicKey(mat.PublicKeyPEM)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if !pub.Equal(priv.Public()) {
		t.Fatal("public key does not match the private key")
	}

	// A token re-minted from the stored key must verify with the stored public
	// key, and must carry the same workspace.
	reminted, err := MintToken(spec, priv, now)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	claims := verifyAt(t, reminted, pub, now)
	if got, _ := claims["workspace"].(string); got != spec.Normalized().Slug {
		t.Errorf("workspace = %q, want %q", got, spec.Normalized().Slug)
	}
}

func TestPEMEncodingIsThePinnedFormat(t *testing.T) {
	mat, err := NewSigningMaterial(materialSpec(), time.Now())
	if err != nil {
		t.Fatalf("NewSigningMaterial: %v", err)
	}
	// Parse rather than string-match the PEM header. Matching the literal
	// "-----BEGIN PRIVATE KEY-----" trips gitleaks' private-key rule on a string
	// that is not a key, and parsing asserts strictly more: the block type AND, in
	// the round-trip test above, that the body decodes.
	//
	// The block types are what distinguish the encodings: PKCS#8 is "PRIVATE KEY"
	// and PKIX is "PUBLIC KEY".
	if got := pemBlockType(mat.PrivateKeyPEM); got != "PRIVATE KEY" {
		t.Errorf("private key block type = %q, want a PKCS#8 PRIVATE KEY", got)
	}
	if got := pemBlockType(mat.PublicKeyPEM); got != "PUBLIC KEY" {
		t.Errorf("public key block type = %q, want a PKIX PUBLIC KEY", got)
	}
}

func TestDefaultTokenTTLIsBounded(t *testing.T) {
	spec := materialSpec()
	spec.TokenTTL = 0 // force the default
	// Truncate, because exp is whole Unix seconds and the comparison is exact.
	now := time.Now().Truncate(time.Second)
	mat, err := NewSigningMaterial(spec, now)
	if err != nil {
		t.Fatalf("NewSigningMaterial: %v", err)
	}
	pub, _ := ParsePublicKey(mat.PublicKeyPEM)
	claims := verifyAt(t, mat.Token, pub, now)
	exp := time.Unix(int64(claims["exp"].(float64)), 0)
	if got := exp.Sub(now); got != DefaultTokenTTL {
		t.Errorf("default TTL = %s, want %s", got, DefaultTokenTTL)
	}
	// An unbounded token would be a long-lived credential with no revocation
	// story beyond deleting the Secret.
	if DefaultTokenTTL > 365*24*time.Hour {
		t.Errorf("default TTL %s is effectively unbounded", DefaultTokenTTL)
	}
}

func TestMintTokenRejectsAnInvalidSlug(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if _, err := MintToken(Spec{Slug: "../escape"}, priv, time.Now()); err == nil {
		t.Error("MintToken accepted an invalid slug")
	}
}

// The token TTL is a leak window, and rotation is what allows it to be short. The
// decision is deliberately separated from the side effect -- nothing in
// TokenNeedsRotation writes anything -- so it can be tested against a controlled
// clock rather than by waiting.
func TestTokenNeedsRotation(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	spec := materialSpec()
	ttl := spec.TokenTTL
	// Truncated to the second because a JWT's exp claim is whole seconds: MintToken
	// stores now.Add(ttl).Unix(). Without this the half-TTL boundary lands a
	// fraction of a second early and the test asserts the truncation rather than
	// the threshold. The sub-second difference is irrelevant in production, where a
	// token is rotated hours before it matters.
	now := time.Now().UTC().Truncate(time.Second)

	token, err := MintToken(spec, priv, now)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}

	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"just minted", now, false},
		{"one second into its life", now.Add(time.Second), false},
		// Rotation is due once LESS than half the TTL remains, so the boundary
		// itself is not yet due. The margin is the point: a control plane that is
		// down for a while must not find every token already expired.
		{"exactly half the TTL left", now.Add(ttl / 2), false},
		{"one second past half", now.Add(ttl/2 + time.Second), true},
		{"a second before expiry", now.Add(ttl - time.Second), true},
		{"expired", now.Add(ttl + time.Second), true},
		{"long expired", now.Add(400 * 24 * time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TokenNeedsRotation(token, tc.at, ttl); got != tc.want {
				t.Errorf("TokenNeedsRotation at %s into its life = %v, want %v",
					tc.at.Sub(now), got, tc.want)
			}
		})
	}
}

// Anything unusable rotates. The alternative is a token that can never be replaced,
// which is worse than one unnecessary mint.
func TestTokenNeedsRotationOnUnusableInput(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"not a jwt at all", "definitely-not-a-token"},
		{"two segments", "aaa.bbb"},
		{"three segments that are not json", "aaa.bbb.ccc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !TokenNeedsRotation(tc.token, now, 24*time.Hour) {
				t.Errorf("TokenNeedsRotation(%q) = false, want true", tc.token)
			}
		})
	}
}

// A token with no exp claim can never be rotated on age, so it must rotate at once
// rather than live forever. This is the shape a bug would take: minting without an
// expiry would silently disable rotation.
func TestTokenNeedsRotationWithoutExpiry(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	unsigned := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{"iss": TokenIssuer})
	token, err := unsigned.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !TokenNeedsRotation(token, time.Now().UTC(), 24*time.Hour) {
		t.Error("a token with no exp claim must need rotation")
	}
}
