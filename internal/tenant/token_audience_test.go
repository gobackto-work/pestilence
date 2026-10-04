package tenant

import (
	"crypto/ed25519"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// There are two tokens and they are deliberately not one.
//
// The capability token belongs to the root agent and names the broker. The reporting token
// belongs to the broker and names the control plane. Neither is accepted where the other
// belongs, so a compromise of either is not a compromise of both, and the two are held in
// different namespaces by different principals.

// audiences reads the audience claim, which is a list or a single string depending on how
// the token was minted.
func audiences(t *testing.T, claims jwt.MapClaims) []string {
	t.Helper()
	raw, ok := claims["aud"]
	if !ok {
		t.Fatal("the token carries no audience claim")
	}
	switch v := raw.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				t.Fatalf("an audience element is %T, want a string", item)
			}
			out = append(out, s)
		}
		return out
	default:
		t.Fatalf("the audience claim is %T, want a string or a list", raw)
		return nil
	}
}

func specForTest() Spec {
	return Spec{Slug: "amber-shrew-uucs", OwnerID: "owner-1"}.Normalized()
}

// mintBoth signs one capability token and one reporting token with the same key, which is
// what a workspace's material holds.
func mintBoth(t *testing.T, spec Spec, now time.Time) (capability, report string, pub ed25519.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	capability, err = MintToken(spec, priv, now)
	if err != nil {
		t.Fatalf("mint the capability token: %v", err)
	}
	report, err = MintReportToken(spec, priv, now)
	if err != nil {
		t.Fatalf("mint the reporting token: %v", err)
	}
	return capability, report, priv.Public().(ed25519.PublicKey)
}

// claimsOf decodes a token without asserting its contract, so a test can look at the
// claims directly. It pins EdDSA the way the broker does.
func claimsOf(t *testing.T, token string, pub ed25519.PublicKey, now time.Time) jwt.MapClaims {
	t.Helper()
	claims, err := verifyTokenClaims(token, pub, jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("decode the token: %v", err)
	}
	return claims
}

func TestTheCapabilityTokenNamesTheBrokerAlone(t *testing.T) {
	now := time.Now()
	spec := specForTest()
	capability, _, pub := mintBoth(t, spec, now)

	claims := claimsOf(t, capability, pub, now)
	if got := audiences(t, claims); !slices.Contains(got, spec.BrokerServiceName()) {
		t.Errorf("the capability token does not name the broker %q. audiences: %v",
			spec.BrokerServiceName(), got)
	}
	if got := audiences(t, claims); slices.Contains(got, TokenAudienceIngest) {
		t.Error("the capability token names the reporting endpoint, so an agent's token is usable there")
	}
	if claims["role"] != TokenRoleRoot {
		t.Errorf("role = %v, want %q", claims["role"], TokenRoleRoot)
	}
}

func TestTheReportTokenNamesTheControlPlaneAlone(t *testing.T) {
	now := time.Now()
	spec := specForTest()
	_, report, pub := mintBoth(t, spec, now)

	claims := claimsOf(t, report, pub, now)
	if got := audiences(t, claims); !slices.Contains(got, TokenAudienceIngest) {
		t.Errorf("the reporting token does not name the reporting endpoint. audiences: %v", got)
	}
	if got := audiences(t, claims); slices.Contains(got, spec.BrokerServiceName()) {
		t.Error("the reporting token names the broker, so it would be accepted for orchestration")
	}
	if claims["role"] != TokenRoleBroker {
		t.Errorf("role = %v, want %q", claims["role"], TokenRoleBroker)
	}
	if claims["workspace"] != spec.Slug || claims["namespace"] != spec.Namespace() {
		t.Errorf("the reporting token lost its workspace: %v / %v",
			claims["workspace"], claims["namespace"])
	}
}

// Each token is refused where the other belongs. This is the property that makes two
// tokens worth the extra object.
func TestEachTokenIsRefusedAtTheOthersEndpoint(t *testing.T) {
	now := time.Now()
	spec := specForTest()
	capability, report, pub := mintBoth(t, spec, now)

	cases := []struct {
		name      string
		token     string
		audience  string
		role      string
		wantValid bool
	}{
		{"capability at the broker", capability, spec.BrokerServiceName(), TokenRoleRoot, true},
		{"report at the control plane", report, TokenAudienceIngest, TokenRoleBroker, true},
		{"capability at the control plane", capability, TokenAudienceIngest, TokenRoleBroker, false},
		{"report at the broker", report, spec.BrokerServiceName(), TokenRoleRoot, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := VerifyToken(c.token, pub, c.audience, spec.Slug, spec.Namespace(), c.role, now)
			if c.wantValid && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !c.wantValid && err == nil {
				t.Error("accepted")
			}
		})
	}
}

// A token minted for one workspace is not accepted for another, whichever role it carries.
func TestATokenForAnotherWorkspaceIsRefused(t *testing.T) {
	now := time.Now()
	_, report, pub := mintBoth(t, specForTest(), now)
	other := Spec{Slug: "eager-crane-ngun", OwnerID: "owner-1"}.Normalized()

	if _, err := VerifyToken(report, pub, TokenAudienceIngest, other.Slug, other.Namespace(), TokenRoleBroker, now); err == nil {
		t.Fatal("a token was accepted for a workspace it was not minted for")
	}
}

func TestAnExpiredReportTokenIsRefused(t *testing.T) {
	spec := specForTest()
	_, report, pub := mintBoth(t, spec, time.Now().Add(-2*DefaultTokenTTL))

	if _, err := VerifyToken(report, pub, TokenAudienceIngest, spec.Slug, spec.Namespace(), TokenRoleBroker, time.Now()); err == nil {
		t.Fatal("an expired reporting token verified")
	}
}

func TestASignedTokenForAnotherKeyIsRefused(t *testing.T) {
	now := time.Now()
	spec := specForTest()
	_, report, _ := mintBoth(t, spec, now)

	_, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	otherPub := otherPriv.Public().(ed25519.PublicKey)

	if _, err := VerifyToken(report, otherPub, TokenAudienceIngest, spec.Slug, spec.Namespace(), TokenRoleBroker, now); err == nil {
		t.Fatal("a token verified against a key that did not sign it")
	}
}
