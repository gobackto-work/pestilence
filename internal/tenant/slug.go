package tenant

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
)

// NamespacePrefix is prepended to a workspace slug to form its Kubernetes
// namespace. The namespace is the tenant security boundary: everything running
// inside it is assumed to be able to attack everything else inside it, and
// nothing outside it.
const NamespacePrefix = "ws-"

// MaxSlugLen bounds the slug so that "<slug>.<domain>" stays a legal DNS name
// and "ws-<slug>" stays a legal namespace name (both cap at 63 characters for a
// single label).
const MaxSlugLen = 40

// slugRE enforces a DNS-1123 label: lowercase alphanumerics and '-', never
// starting or ending with '-'. This matters because the slug is interpolated
// into a namespace name, a Service name, and a public hostname.
var slugRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidateSlug rejects anything that is not a safe DNS label.
//
// This is a security boundary, not a formatting nicety: the slug is embedded in
// a namespace name and a hostname, so a value containing "../", "/", or uppercase
// characters must never reach the provisioner.
func ValidateSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("slug is empty")
	}
	if len(slug) > MaxSlugLen {
		return fmt.Errorf("slug %q is %d characters, maximum is %d", slug, len(slug), MaxSlugLen)
	}
	if !slugRE.MatchString(slug) {
		return fmt.Errorf("slug %q is not a valid DNS-1123 label (lowercase alphanumerics and '-' only, no leading or trailing '-')", slug)
	}
	return nil
}

// Namespace returns the tenant namespace for a slug.
func Namespace(slug string) string {
	return NamespacePrefix + slug
}

var (
	adjectives = []string{
		"amber", "brisk", "calm", "clever", "crimson", "dapper", "eager", "fuzzy",
		"gentle", "golden", "hidden", "jolly", "keen", "lively", "mellow", "nimble",
		"placid", "quiet", "rapid", "rustic", "silent", "spry", "steady", "vivid",
	}
	animals = []string{
		"badger", "beetle", "bison", "crane", "falcon", "ferret", "gecko", "heron",
		"ibex", "jackal", "koala", "lemur", "lynx", "marmot", "moth", "newt",
		"otter", "panda", "quail", "raven", "shrew", "tapir", "vole", "wombat",
	}
)

// GenerateSlug produces a human-pronounceable workspace slug such as
// "fuzzy-wombat-x7q3".
//
// The suffix is drawn from crypto/rand and is what makes the resulting hostname
// unguessable. That is a defence-in-depth measure only: an unguessable hostname
// must never be treated as authentication.
func GenerateSlug() (string, error) {
	a, err := pick(adjectives)
	if err != nil {
		return "", err
	}
	b, err := pick(animals)
	if err != nil {
		return "", err
	}
	suffix, err := randomSuffix(4)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%s", a, b, suffix), nil
}

func pick(words []string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return "", err
	}
	return words[n.Int64()], nil
}

const suffixAlphabet = "abcdefghijkmnpqrstuvwxyz23456789" // no l/o/0/1

func randomSuffix(n int) (string, error) {
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(suffixAlphabet))))
		if err != nil {
			return "", err
		}
		out[i] = suffixAlphabet[idx.Int64()]
	}
	return string(out), nil
}
