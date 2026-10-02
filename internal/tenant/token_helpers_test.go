package tenant

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// Helpers for verifying what pestilence mints. They live in the test package on
// purpose: nothing in the control plane verifies its own tokens -- the broker
// does, in another repository -- so as production code they were unreachable, and
// the gate said so.

// VerifyToken parses and verifies a token against a workspace public key, pinning
// EdDSA exactly as the broker does.
//
// Parser options are accepted so a caller can control the clock. Without that, a
// token minted at a fixed time in a test reads as "not valid yet" or "expired"
// depending only on when the test happens to run.
func VerifyToken(token string, pub ed25519.PublicKey, opts ...jwt.ParserOption) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	opts = append(opts, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}))
	_, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) {
		return pub, nil
	}, opts...)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// ParsePublicKey decodes a PKIX PEM Ed25519 public key. It is the inverse of
// MarshalPublicKey, which production code does use: pestilence derives the public
// key from the private key it already holds.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return nil, fmt.Errorf("public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is %T, want ed25519.PublicKey", key)
	}
	return pub, nil
}

// pemBlockType returns a decoded PEM block's type, or a description of why it
// could not be decoded. Used by the format test so it can assert on the block
// rather than on a string.
func pemBlockType(encoded string) string {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return "not PEM"
	}
	return block.Type
}
