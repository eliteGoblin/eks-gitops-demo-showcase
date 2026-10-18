package graphql

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// A cursor is an opaque keyset position that names the order it belongs to.
// The default order (oldest submission first) keeps the original form,
//
//	"claim:" + submittedAt + "|" + id
//
// so cursors issued before ordering existed still work; the other orders are
//
//	"claim2:" + field + ":" + direction + ":" + key + "|" + id
//
// with the key an RFC 3339 time or an amount in cents.
const (
	cursorPrefix        = "claim:"
	orderedCursorPrefix = "claim2:"
)

var defaultOrder = app.ClaimOrder{}.Normalized()

func encodeCursor(c app.ClaimCursor) string {
	order := c.Order.Normalized()
	var raw string
	if order == defaultOrder {
		raw = cursorPrefix + c.SubmittedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	} else {
		raw = fmt.Sprintf("%s%s:%s:%s|%s", orderedCursorPrefix, order.Field, direction(order), cursorKey(c, order), c.ID)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func direction(o app.ClaimOrder) string {
	if o.Descending {
		return "DESC"
	}
	return "ASC"
}

func cursorKey(c app.ClaimCursor, o app.ClaimOrder) string {
	if o.Field == app.OrderByAmount {
		return strconv.FormatInt(int64(c.Amount), 10)
	}
	return c.SubmittedAt.UTC().Format(time.RFC3339Nano)
}

func decodeCursor(s string) (*app.ClaimCursor, error) {
	malformed := fmt.Errorf("%w: malformed cursor", domain.ErrInvalidInput)
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, malformed
	}
	raw := string(b)
	order, rest := defaultOrder, ""
	switch {
	case strings.HasPrefix(raw, orderedCursorPrefix):
		var ok bool
		if order, rest, ok = parseOrder(strings.TrimPrefix(raw, orderedCursorPrefix)); !ok {
			return nil, malformed
		}
	case strings.HasPrefix(raw, cursorPrefix):
		rest = strings.TrimPrefix(raw, cursorPrefix)
	default:
		return nil, malformed
	}
	key, id, ok := strings.Cut(rest, "|")
	if !ok || id == "" {
		return nil, malformed
	}
	c := &app.ClaimCursor{Order: order, ID: id}
	if order.Field == app.OrderByAmount {
		cents, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return nil, malformed
		}
		c.Amount = domain.Money(cents)
		return c, nil
	}
	if c.SubmittedAt, err = time.Parse(time.RFC3339Nano, key); err != nil {
		return nil, malformed
	}
	return c, nil
}

// parseOrder reads "field:direction:" off an ordered cursor.
func parseOrder(s string) (app.ClaimOrder, string, bool) {
	field, s, ok1 := strings.Cut(s, ":")
	dir, rest, ok2 := strings.Cut(s, ":")
	o := app.ClaimOrder{Field: app.ClaimOrderField(field), Descending: dir == "DESC"}
	valid := ok1 && ok2 && (dir == "ASC" || dir == "DESC") &&
		(o.Field == app.OrderBySubmittedAt || o.Field == app.OrderByAmount)
	return o, rest, valid
}
