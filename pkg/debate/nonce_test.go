package debate

import (
	"regexp"
	"testing"
)

// nonceShapeRE matches the required session-nonce format: 16 lowercase hex
// chars. Anchored on both ends so trailing whitespace or extra bytes fail.
var nonceShapeRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestGenerateNonce_Shape(t *testing.T) {
	n, err := GenerateNonce()
	if err != nil {
		t.Fatalf("GenerateNonce: %v", err)
	}
	if !nonceShapeRE.MatchString(n) {
		t.Fatalf("nonce %q does not match %s", n, nonceShapeRE)
	}
}

func TestGenerateNonce_Unique(t *testing.T) {
	// 8 bytes of entropy — collision probability for 100 draws is ~5e-15,
	// well below the 1e-9 flake threshold. If this fails, crypto/rand is
	// broken or GenerateNonce is not reading fresh bytes.
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		n, err := GenerateNonce()
		if err != nil {
			t.Fatalf("GenerateNonce[%d]: %v", i, err)
		}
		if _, dup := seen[n]; dup {
			t.Fatalf("duplicate nonce %q at iteration %d", n, i)
		}
		seen[n] = struct{}{}
	}
}
