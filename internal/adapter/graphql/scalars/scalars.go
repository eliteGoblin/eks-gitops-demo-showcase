// Package scalars marshals the domain value types for GraphQL, keeping
// transport code out of the domain package.
package scalars

import (
	"fmt"
	"io"
	"strconv"

	"github.com/99designs/gqlgen/graphql"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

func quoted(s string) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) { _, _ = w.Write(strconv.AppendQuote(nil, s)) })
}

// MarshalDate writes a Date as "YYYY-MM-DD".
func MarshalDate(d domain.Date) graphql.Marshaler { return quoted(string(d)) }

// UnmarshalDate validates a "YYYY-MM-DD" string.
func UnmarshalDate(v any) (domain.Date, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: date must be a string", domain.ErrInvalidInput)
	}
	return domain.ParseDate(s)
}

// MarshalMoney writes Money as a decimal string "123.45" so no precision is lost.
func MarshalMoney(m domain.Money) graphql.Marshaler { return quoted(m.String()) }

// UnmarshalMoney accepts "123.45" (preferred), or an integer or float literal.
func UnmarshalMoney(v any) (domain.Money, error) {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case int64:
		s = strconv.FormatInt(t, 10)
	case int:
		s = strconv.Itoa(t)
	case float64:
		s = strconv.FormatFloat(t, 'f', 2, 64)
	default:
		return 0, fmt.Errorf("%w: money must be a decimal string", domain.ErrInvalidInput)
	}
	return domain.ParseMoney(s)
}
