package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// IdempotencyKeyReusedError is returned when a caller sends an idempotency
// key it already used with a request of different content.
type IdempotencyKeyReusedError struct{}

func (*IdempotencyKeyReusedError) Error() string {
	return "idempotency key already used for a different request"
}

// maxIdempotencyKeyLen bounds a key; clients usually send a UUID.
const maxIdempotencyKeyLen = 128

// validateIdempotencyKey accepts an empty key (none sent) or 1 to 128 of
// letters, digits, '.', '_', ':' and '-'.
func validateIdempotencyKey(k string) error {
	if len(k) > maxIdempotencyKeyLen {
		return &domain.ValidationError{Field: "idempotencyKey", Message: fmt.Sprintf("at most %d characters", maxIdempotencyKeyLen)}
	}
	for _, r := range k {
		if !isKeyRune(r) {
			return &domain.ValidationError{Field: "idempotencyKey", Message: "only letters, digits, '.', '_', ':' and '-'"}
		}
	}
	return nil
}

func isKeyRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return r == '.' || r == '_' || r == ':' || r == '-'
}

// fingerprint identifies the request's content, so a reused key with
// different input can be told apart from a retry.
func (in SubmitClaimInput) fingerprint() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x1f%s\x1f%d\x1f%s", in.PlanID, in.Category, in.Amount, in.ServiceDate))
	return hex.EncodeToString(sum[:])
}
