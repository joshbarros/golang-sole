// Package orders is the HTTP side of the saga and the handler that moves an order forward as events arrive.
package orders

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Order rules.
const (
	MaxPairsPerOrder      = 6
	MaxReferenceLength    = 40
	ServiceFeeBasisPoints = 1_200
)

// ErrOverflow is returned instead of wrapping.
var ErrOverflow = errors.New("arithmetic overflow")

// Validate returns an error message, or "" when the request is acceptable. The pair count is compared as int64 on purpose:
// narrowing 4294967297 to int32 would wrap to 1 and pass.
func Validate(reference string, pairs int64) string {
	trimmed := strings.TrimSpace(reference)
	switch {
	case trimmed == "":
		return "Entrant reference is required."
	case len([]rune(trimmed)) > MaxReferenceLength:
		return fmt.Sprintf("Entrant reference must be at most %d characters.", MaxReferenceLength)
	case pairs < 1:
		return "An order needs at least 1 pair."
	case pairs > MaxPairsPerOrder:
		return fmt.Sprintf("An order takes at most %d pairs.", MaxPairsPerOrder)
	}
	return ""
}

// TotalCents is face value plus a 12% service fee truncated to a whole cent. Overflow is an error, not a wrap.
func TotalCents(priceCents int64, pairs int) (int64, error) {
	if priceCents < 0 || pairs < 0 {
		return 0, ErrOverflow
	}
	if pairs != 0 && priceCents > math.MaxInt64/int64(pairs) {
		return 0, ErrOverflow
	}
	face := priceCents * int64(pairs)
	if face > math.MaxInt64/ServiceFeeBasisPoints {
		return 0, ErrOverflow
	}
	fee := face * ServiceFeeBasisPoints / 10_000
	if face > math.MaxInt64-fee {
		return 0, ErrOverflow
	}
	return face + fee, nil
}
