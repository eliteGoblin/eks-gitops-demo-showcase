package app

import (
	"context"
	"errors"
)

func indexBy[T any](items []T, key func(T) string) map[string]T {
	m := make(map[string]T, len(items))
	for _, it := range items {
		m[key(it)] = it
	}
	return m
}

// visibleOf authorizes each found record (audited like a single read) and
// returns, aligned to found, the record or nil where the caller may not read it.
func visibleOf[T any](ctx context.Context, g guard, found []T, access func(T) (Action, Resource)) ([]*T, error) {
	out := make([]*T, len(found))
	for i := range found {
		a, res := access(found[i])
		_, err := g.authorize(ctx, a, res)
		if errors.Is(err, ErrForbidden) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[i] = &found[i]
	}
	return out, nil
}

// alignTo orders shown (aligned to found) by ids: one entry per id, nil where
// the id was not found or its record may not be read.
func alignTo[T any](ids []string, found []T, shown []*T, key func(T) string) []*T {
	byID := make(map[string]*T, len(found))
	for i, v := range found {
		byID[key(v)] = shown[i]
	}
	out := make([]*T, len(ids))
	for i, id := range ids {
		out[i] = byID[id]
	}
	return out
}
