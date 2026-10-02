package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

func newVerifier(t *testing.T, pub ed25519.PublicKey) *Verifier {
	t.Helper()
	v, err := NewVerifier(pub, "", "", 0)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// mint signs a well-formed assertion for town, with knobs for the parts a test
// needs to get wrong.
type claims struct {
	issuer   string
	audience string
	subject  string
	expiry   time.Duration
	skipExp  bool
	method   jwt.SigningMethod
}

func mint(t *testing.T, key any, c claims) string {
	t.Helper()
	if c.issuer == "" {
		c.issuer = Issuer
	}
	if c.audience == "" {
		c.audience = Audience
	}
	if c.subject == "" {
		c.subject = "github#583231"
	}
	if c.expiry == 0 {
		c.expiry = time.Minute
	}
	if c.method == nil {
		c.method = jwt.SigningMethodEdDSA
	}
	now := time.Now()
	registered := jwt.RegisteredClaims{
		Issuer:   c.issuer,
		Audience: jwt.ClaimStrings{c.audience},
		Subject:  c.subject,
		IssuedAt: jwt.NewNumericDate(now),
	}
	if !c.skipExp {
		registered.ExpiresAt = jwt.NewNumericDate(now.Add(c.expiry))
	}
	signed, err := jwt.NewWithClaims(c.method, registered).SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestVerifyAcceptsTownsAssertion(t *testing.T) {
	pub, priv := newKey(t)
	owner, err := newVerifier(t, pub).Verify(mint(t, priv, claims{}))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if owner != "github#583231" {
		t.Errorf("owner = %q, want the subject verbatim", owner)
	}
}

// The classic JWT confusion attack: sign with HMAC, using the PUBLIC key as the
// shared secret. It only works if the verifier accepts whatever algorithm the
// token claims. This is what WithValidMethods is for, and the test exists so that
// removing it fails loudly rather than silently.
func TestVerifyRejectsAlgorithmConfusion(t *testing.T) {
	pub, _ := newKey(t)
	// The public key bytes, which an attacker has, as the HMAC secret.
	forged := mint(t, []byte(pub), claims{method: jwt.SigningMethodHS256})

	if _, err := newVerifier(t, pub).Verify(forged); err == nil {
		t.Fatal("an HS256 token signed with the public key was accepted")
	}
}

func TestVerifyRejectsTheNoneAlgorithm(t *testing.T) {
	pub, _ := newKey(t)
	if _, err := newVerifier(t, pub).Verify(mint(t, jwt.UnsafeAllowNoneSignatureType, claims{method: jwt.SigningMethodNone})); err == nil {
		t.Fatal("an unsigned token was accepted")
	}
}

func TestVerifyRejectsAnotherKey(t *testing.T) {
	pub, _ := newKey(t)
	_, otherPriv := newKey(t)
	if _, err := newVerifier(t, pub).Verify(mint(t, otherPriv, claims{})); err == nil {
		t.Fatal("a token signed by a different key was accepted")
	}
}

func TestVerifyPinsTheIssuerAndAudience(t *testing.T) {
	pub, priv := newKey(t)
	v := newVerifier(t, pub)

	for name, c := range map[string]claims{
		"wrong issuer":   {issuer: "somebody-else"},
		"wrong audience": {audience: "some-other-api"},
	} {
		if _, err := v.Verify(mint(t, priv, c)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestVerifyRequiresAFreshExpiry(t *testing.T) {
	pub, priv := newKey(t)
	v := newVerifier(t, pub)

	if _, err := v.Verify(mint(t, priv, claims{expiry: -10 * time.Minute})); err == nil {
		t.Error("an expired assertion was accepted")
	}
	// No expiry at all would be valid forever.
	if _, err := v.Verify(mint(t, priv, claims{skipExp: true})); err == nil {
		t.Error("an assertion with no expiry was accepted")
	}
}

func TestVerifyAllowsForClockSkew(t *testing.T) {
	pub, priv := newKey(t)
	// Just inside the 60s leeway: town's clock may be marginally ahead of ours.
	if _, err := newVerifier(t, pub).Verify(mint(t, priv, claims{expiry: -30 * time.Second})); err != nil {
		t.Errorf("an assertion 30s past expiry should be inside the leeway: %v", err)
	}
}

func TestVerifyRejectsRubbish(t *testing.T) {
	pub, _ := newKey(t)
	v := newVerifier(t, pub)
	for _, assertion := range []string{"", "not.a.token", "a.b", strings.Repeat("x", 200)} {
		if _, err := v.Verify(assertion); err == nil {
			t.Errorf("%q was accepted", assertion)
		}
	}
}

func TestVerifyRejectsAnEmptySubject(t *testing.T) {
	// A valid signature is not enough. An assertion naming nobody must not become an
	// owner of "", which would be a shared identity.
	pub, priv := newKey(t)
	now := time.Now()
	registered := jwt.RegisteredClaims{
		Issuer:    Issuer,
		Audience:  jwt.ClaimStrings{Audience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, registered).SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := newVerifier(t, pub).Verify(signed); err == nil {
		t.Error("an assertion with no subject was accepted")
	}
}

func TestNewVerifierRefusesAKeyOfTheWrongSize(t *testing.T) {
	if _, err := NewVerifier(make([]byte, 5), "", "", 0); err == nil {
		t.Fatal("a short key was accepted")
	}
}

func TestLoadPublicKeyPEM(t *testing.T) {
	pub, _ := newKey(t)
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dir := t.TempDir()

	good := filepath.Join(dir, "good.pub")
	if err := os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, _, err := LoadPublicKeyPEM(good)
	if err != nil {
		t.Fatalf("LoadPublicKeyPEM: %v", err)
	}
	if !loaded.Equal(pub) {
		t.Error("the loaded key is not the one written")
	}

	// The plausible mistakes, each of which must name itself rather than fail later
	// as a signature error on every request.
	notPEM := filepath.Join(dir, "raw.pub")
	if err := os.WriteFile(notPEM, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := LoadPublicKeyPEM(notPEM); err == nil || !strings.Contains(err.Error(), "not PEM") {
		t.Errorf("a non-PEM file gave %v, want a 'not PEM' error", err)
	}

	if _, _, err := LoadPublicKeyPEM(filepath.Join(dir, "missing.pub")); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestLoadPublicKeyRejectsADifferentAlgorithm(t *testing.T) {
	// An RSA public key is the realistic mix-up, and the error should name what it
	// found rather than leaving a signature failure on every request to be puzzled
	// over. Generated rather than pasted: the point is a key of the wrong TYPE, and
	// a made-up PEM block would not even decode.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ed25519.pub")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err = LoadPublicKeyPEM(path)
	if err == nil || !strings.Contains(err.Error(), "want ed25519") {
		t.Errorf("error = %v, want one naming the expected type", err)
	}
}
