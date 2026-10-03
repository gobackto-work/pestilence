package tenant

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Token constants. The contract pins all of these (scarab/docs/architecture.md §6.2),
// and BOTH repositories use the same JWT library and the same algorithm. The
// broker pins EdDSA via WithValidMethods, which is what closes the HS256/"none"
// confusion attacks.
const (
	TokenIssuer   = "pestilence"
	TokenRoleRoot = "root-agent"

	// Data keys used by the three objects that carry token material.
	TokenPrivateKeyKey = "ed25519.pem"
	TokenPublicKeyKey  = "ed25519.pub"
	TokenDataKey       = "token"

	// DefaultTokenTTL bounds the capability token.
	//
	// This was 90 days, which the first red-team review flagged and was right to:
	// the token authorizes broker calls, it is readable by every process in the
	// workspace, and 90 days is not a bounded exposure.
	//
	// It was long because a short TTL needs a refresh path and there was none, so a
	// token that expired simply stopped working. The refresh path now exists --
	// TokenNeedsRotation plus the reconciler re-minting on the drift sweep -- and the
	// bridge reads the token per call rather than caching it, so a rotation is picked
	// up without a restart.
	//
	// The value trades leak window against outage tolerance: a control plane down for
	// longer than half the TTL lets live tokens expire and breaks running workspaces.
	// 24h gives a 12h margin, which is the balance this platform wants. It is
	// overridable per workspace via Spec.TokenTTL.
	DefaultTokenTTL = 24 * time.Hour
)

// MinimumTokenTTL is the shortest TTL that can work, and it is enforced rather than
// merely documented because the failure it prevents is invisible at the point of
// configuration.
//
// A token reaches the agent through a Kubernetes Secret MOUNT, not directly. The
// kubelet refreshes that mount on its own schedule -- measured on this cluster at
// about 24 seconds, with a 60-second sync period as the documented default. So after
// the control plane rotates a token there is a window where the stored token is new
// and the mounted one is still the old one. That window is fine and intended: the old
// token remains VALID, which is exactly why rotation happens at half the TTL rather
// than at expiry.
//
// But it means the margin -- half the TTL -- must be comfortably longer than the
// propagation delay. Measured: a 90-second TTL rotates with 45 seconds of margin, and
// the mount lag ate roughly half of it. Any slower sync and the mounted token expires
// before it is replaced, so the broker starts rejecting the agent with "invalid
// capability token" -- an error that says nothing about TTLs, on a workspace that
// worked a minute earlier.
//
// 10 minutes makes the margin 5 minutes against a sub-minute delay. The default 24h
// makes it 12 hours, which is where it should be for anything real.
const MinimumTokenTTL = 10 * time.Minute

// SigningMaterial is a workspace's token-signing keypair, the token it minted, and the
// certificate its broker serves TLS with.
//
// The private key never leaves pestilence. It is stored in the control plane's own
// namespace and is never mounted into a tenant pod. The public key is not secret and is
// published as a ConfigMap for the workspace's broker to read.
//
// The TLS fields are deliberately NOT part of Empty(). A material without them is valid
// and simply renders no TLS objects, which is what lets this land on one side of the
// contract first: both sides fall back to plaintext with a warning rather than refusing
// to start.
type SigningMaterial struct {
	PrivateKeyPEM string
	PublicKeyPEM  string
	Token         string
	TLSCertPEM    string
	TLSKeyPEM     string
}

// Empty reports whether the material is absent.
func (m SigningMaterial) Empty() bool {
	return m.PrivateKeyPEM == "" || m.PublicKeyPEM == "" || m.Token == ""
}

// NewSigningMaterial generates a fresh Ed25519 keypair for the workspace and mints
// its capability token.
//
// This is deliberately NOT called from Bundle: a pure renderer that generated a
// key would produce a different key on every call, so the broker's public key
// would change under it. The reconciler creates the material once and stores it.
func NewSigningMaterial(spec Spec, now time.Time) (SigningMaterial, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf("generate ed25519 key: %w", err)
	}
	privPEM, err := MarshalPrivateKey(priv)
	if err != nil {
		return SigningMaterial{}, err
	}
	pubPEM, err := MarshalPublicKey(pub)
	if err != nil {
		return SigningMaterial{}, err
	}
	token, err := MintToken(spec, priv, now)
	if err != nil {
		return SigningMaterial{}, err
	}
	tlsMaterial, err := NewBrokerTLS(spec, now)
	if err != nil {
		return SigningMaterial{}, err
	}
	return SigningMaterial{
		PrivateKeyPEM: privPEM,
		PublicKeyPEM:  pubPEM,
		Token:         token,
		TLSCertPEM:    tlsMaterial.CertPEM,
		TLSKeyPEM:     tlsMaterial.KeyPEM,
	}, nil
}

// TokenAudienceIngest is the audience that the event-record ingest endpoint requires.
//
// It is an extra audience on the same token and not a second token. The root agent
// holds the token and the root agent is the reporter, so a second token would be held
// by the same process and would separate nothing. What this does buy is the rule that
// a token minted for one service is refused at another, which matters as soon as a
// second service mints tokens.
const TokenAudienceIngest = "pestilence-ingest"

// MintToken issues the workspace capability token with the claims pinned in §6.2.
func MintToken(spec Spec, priv ed25519.PrivateKey, now time.Time) (string, error) {
	s := spec.Normalized()
	if err := ValidateSlug(s.Slug); err != nil {
		return "", err
	}
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("signing key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	ttl := s.TokenTTL
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	jti, err := randomID(16)
	if err != nil {
		return "", err
	}
	claims := jwt.MapClaims{
		"iss":       TokenIssuer,
		"sub":       "pi-root@" + s.Namespace(),
		"aud":       jwt.ClaimStrings{s.BrokerServiceName(), TokenAudienceIngest},
		"workspace": s.Slug,
		"namespace": s.Namespace(),
		"role":      TokenRoleRoot,
		"iat":       now.Unix(),
		"nbf":       now.Unix(),
		"exp":       now.Add(ttl).Unix(),
		"jti":       jti,
	}
	return jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(priv)
}

// MarshalPrivateKey encodes an Ed25519 private key as PKCS#8 PEM.
func MarshalPrivateKey(priv ed25519.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", fmt.Errorf("marshal private key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// MarshalPublicKey encodes an Ed25519 public key as PKIX PEM, the format §6.2
// pins for the broker.
func MarshalPublicKey(pub ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// ParsePrivateKey decodes a PKCS#8 PEM Ed25519 private key.
func ParsePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return nil, fmt.Errorf("private key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want ed25519.PrivateKey", key)
	}
	return priv, nil
}

func randomID(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Claims are the workspace capability-token claims.
type Claims struct {
	Workspace string `json:"workspace"`
	Namespace string `json:"namespace"`
	Role      string `json:"role"`

	jwt.RegisteredClaims
}

// VerifyToken checks a workspace capability token and returns its claims.
//
// The caller supplies the public key of the workspace it expects, because the key is per
// workspace and this package holds none of its own. The workspace and the namespace are
// checked against the caller's expectations, so a token minted for one workspace is not
// accepted for another.
//
// Every rejection returns the same shape of error. The caller collapses them into one
// response, so a probing client learns nothing about which check failed.
func VerifyToken(raw string, pub ed25519.PublicKey, audience, workspace, namespace string, now time.Time) (Claims, error) {
	var claims Claims
	if len(pub) != ed25519.PublicKeySize {
		return Claims{}, fmt.Errorf("verification key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	if _, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return pub, nil },
		// Pinned, for the same reason the broker pins it: a parser that accepts the
		// algorithm the token claims accepts an HMAC forged with the public key.
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer(TokenIssuer),
		// An audience is a set. The token is accepted wherever it names.
		jwt.WithAudience(audience),
		// A token with no expiry would be valid for ever.
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	); err != nil {
		return Claims{}, fmt.Errorf("token: %w", err)
	}
	if claims.Workspace != workspace {
		return Claims{}, fmt.Errorf("token: workspace %q is not %q", claims.Workspace, workspace)
	}
	if claims.Namespace != namespace {
		return Claims{}, fmt.Errorf("token: namespace %q is not %q", claims.Namespace, namespace)
	}
	if claims.Role != TokenRoleRoot {
		return Claims{}, fmt.Errorf("token: role %q may not report", claims.Role)
	}
	return claims, nil
}

// TokenRotationDivisor sets how much of the TTL must remain for a token to count as
// fresh. A token is re-minted once less than half its life is left, so rotation
// happens with as much margin as the TTL allows for the control plane to be down.
const TokenRotationDivisor = 2

// TokenNeedsRotation reports whether a stored token should be replaced.
//
// The token is parsed WITHOUT verifying its signature. That is deliberate rather
// than lazy: this is our own token, read back out of our own Secret, and the only
// question being asked is when it expires. Verifying it here would need the public
// key and would answer a different question -- "is this token authentic", which a
// token we minted and stored ourselves always is.
//
// Anything unparseable is rotated. That is the safe direction: the alternative is a
// token that can never be replaced.
func TokenNeedsRotation(token string, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return true
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return true
	}
	return exp.Sub(now) < ttl/TokenRotationDivisor
}
