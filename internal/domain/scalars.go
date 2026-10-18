package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Date is a calendar date in YYYY-MM-DD form.
type Date string

const dateLayout = "2006-01-02"

// ParseDate validates s as YYYY-MM-DD.
func ParseDate(s string) (Date, error) {
	if _, err := time.Parse(dateLayout, s); err != nil {
		return "", fmt.Errorf("%w: date must be YYYY-MM-DD", ErrInvalidInput)
	}
	return Date(s), nil
}

// Money is an amount in integer cents; it is written as a decimal "123.45".
type Money int64

// ParseMoney accepts "123", "123.4" or "123.45" with an optional leading
// minus. Anything else (signs inside, exponents, separators) is rejected.
func ParseMoney(s string) (Money, error) {
	s = strings.TrimSpace(s)
	digits, neg := strings.CutPrefix(s, "-")
	whole, frac, hasDot := strings.Cut(digits, ".")
	if !isDigits(whole) || len(frac) > 2 || (hasDot && !isDigits(frac)) {
		return 0, fmt.Errorf("%w: money must look like 123.45", ErrInvalidInput)
	}
	frac += strings.Repeat("0", 2-len(frac))
	cents, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: money is out of range", ErrInvalidInput)
	}
	if neg {
		cents = -cents
	}
	return Money(cents), nil
}

// isDigits reports whether s is non-empty and only ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (m Money) String() string {
	sign := ""
	cents := int64(m)
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}
