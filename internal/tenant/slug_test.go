package tenant

import (
	"strings"
	"testing"
)

func TestValidateSlug(t *testing.T) {
	valid := []string{
		"a",
		"fuzzy-wombat-x7q3",
		"abc123",
		"a1b2c3",
		strings.Repeat("a", MaxSlugLen),
	}
	for _, s := range valid {
		if err := ValidateSlug(s); err != nil {
			t.Errorf("ValidateSlug(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",
		"-leading",
		"trailing-",
		"Uppercase",
		"has space",
		"has_underscore",
		"has.dot",
		"has/slash",
		"..",
		"../etc",
		"a--", // ends with '-'
		strings.Repeat("a", MaxSlugLen+1),
	}
	for _, s := range invalid {
		if err := ValidateSlug(s); err == nil {
			t.Errorf("ValidateSlug(%q) = nil, want error", s)
		}
	}
}

func TestNamespace(t *testing.T) {
	if got := Namespace("fuzzy-wombat-x7q3"); got != "ws-fuzzy-wombat-x7q3" {
		t.Errorf("Namespace() = %q", got)
	}
}

func TestGenerateSlug(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		slug, err := GenerateSlug()
		if err != nil {
			t.Fatalf("GenerateSlug: %v", err)
		}
		if err := ValidateSlug(slug); err != nil {
			t.Fatalf("GenerateSlug produced an invalid slug %q: %v", slug, err)
		}
		if seen[slug] {
			t.Fatalf("GenerateSlug produced a duplicate %q", slug)
		}
		seen[slug] = true
		if strings.Count(slug, "-") != 2 {
			t.Fatalf("slug %q does not have the expected adjective-animal-suffix shape", slug)
		}
	}
}
