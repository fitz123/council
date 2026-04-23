package debate

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// GenerateNonce returns a fresh 16-character lowercase hex string derived
// from 8 bytes of crypto/rand entropy. The nonce is embedded in every
// expert-output fence (=== EXPERT: A [nonce-<hex>] ===) so forged-fence
// injection across the LLM-output boundary is detectable at parse time
// (ADR-0008 D11). 8 bytes gives 2^64 possibilities — effectively unguessable
// by an adversarial model within a single session's context window.
func GenerateNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("GenerateNonce: read crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
