package tenant

import (
	"crypto/ed25519"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The token carries every audience it is accepted at. It is accepted at the broker's
// service name, which the broker in another repository checks, and at the ingest
// audience, which the control plane checks for its own endpoint.
//
// Both are pinned here. Dropping either one stops a running service accepting the
// token, and the symptom appears in the other repository.
func mintForTest(t *testing.T, spec Spec, now time.Time) (string, ed25519.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	token, err := MintToken(spec, priv, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return token, priv.Public().(ed25519.PublicKey)
}

// audiences reads the audience claim, which is a list or a single string depending on
// how a token was minted.
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

func TestTheTokenNamesTheBrokerAndTheIngest(t *testing.T) {
	now := time.Now()
	spec := Spec{Slug: "amber-shrew-uucs", OwnerID: "owner-1"}.Normalized()
	token, pub := mintForTest(t, spec, now)

	claims, err := VerifyToken(token, pub, jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	got := audiences(t, claims)
	for _, want := range []string{spec.BrokerServiceName(), TokenAudienceIngest} {
		if !slices.Contains(got, want) {
			t.Errorf("the token does not name %q, so that service would refuse it. audiences: %v", want, got)
		}
	}
}

func TestTheTokenStillCarriesTheWorkspaceAndTheRole(t *testing.T) {
	now := time.Now()
	spec := Spec{Slug: "amber-shrew-uucs", OwnerID: "owner-1"}.Normalized()
	token, pub := mintForTest(t, spec, now)

	claims, err := VerifyToken(token, pub, jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims["workspace"] != spec.Slug {
		t.Errorf("workspace = %v, want %q", claims["workspace"], spec.Slug)
	}
	if claims["namespace"] != spec.Namespace() {
		t.Errorf("namespace = %v, want %q", claims["namespace"], spec.Namespace())
	}
	if claims["role"] != TokenRoleRoot {
		t.Errorf("role = %v, want %q", claims["role"], TokenRoleRoot)
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	spec := Spec{Slug: "amber-shrew-uucs", OwnerID: "owner-1"}.Normalized()
	token, pub := mintForTest(t, spec, time.Now().Add(-2*DefaultTokenTTL))

	if _, err := VerifyToken(token, pub); err == nil {
		t.Fatal("an expired token verified")
	}
}
