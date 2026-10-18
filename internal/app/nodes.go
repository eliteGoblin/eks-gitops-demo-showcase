package app

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// MaxNodes bounds one GetMany (nodes(ids)) lookup.
const MaxNodes = 50

// maxIDLength bounds an id a client may send; generated ids are 36 characters.
const maxIDLength = 128

// Record is one record found by id; exactly one field is set.
type Record struct {
	Participant    *domain.Participant
	Plan           *domain.Plan
	Provider       *domain.Provider
	Claim          *domain.Claim
	SupportRequest *domain.SupportRequest
}

// NodeService finds records of any type by id (Relay's node and nodes). Ids
// are unique across types, so each type's table is read once per call, with
// one batch read of the ids not found in an earlier type, in a fixed order:
// a call costs at most one read per type however many ids it is given. The
// records found then pass the authorization, audit and field masking of the
// typed queries, so node is never a way around them. A miss and a denial
// both return nil.
type NodeService struct{ kinds []nodeKind }

// nodeKind loads the records of one type among ids: for each id that is one
// of its records, the record as the caller may see it, or nil when the
// caller may not read it. Ids it does not return are not of this type.
type nodeKind func(ctx context.Context, ids []string) (map[string]*Record, error)

func newNodeService(d Deps, svc *Services) *NodeService {
	return &NodeService{kinds: []nodeKind{
		kindOf(d.Claims.GetClaims, func(c domain.Claim) string { return c.ID }, svc.Claims.visible,
			func(c *domain.Claim) *Record { return &Record{Claim: c} }),
		kindOf(d.SupportRequests.GetSupportRequests, func(r domain.SupportRequest) string { return r.ID }, svc.SupportRequests.visible,
			func(r *domain.SupportRequest) *Record { return &Record{SupportRequest: r} }),
		kindOf(d.Plans.GetPlans, planID, svc.Plans.visible,
			func(p *domain.Plan) *Record { return &Record{Plan: p} }),
		kindOf(d.Participants.GetParticipants, participantID, svc.Participants.visible,
			func(p *domain.Participant) *Record { return &Record{Participant: p} }),
		kindOf(d.Providers.GetProviders, providerID, svc.Providers.visible,
			func(p *domain.Provider) *Record { return &Record{Provider: p} }),
	}}
}

// Get returns the record with id, or nil when there is none or the caller
// may not read it.
func (s *NodeService) Get(ctx context.Context, id string) (*Record, error) {
	recs, err := s.GetMany(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	return recs[0], nil
}

// GetMany returns the records for ids in the same order, nil where Get would
// return nil. At most MaxNodes ids; one malformed id fails the whole call
// with domain.ErrInvalidInput, as any malformed argument does.
func (s *NodeService) GetMany(ctx context.Context, ids []string) ([]*Record, error) {
	if _, ok := PrincipalFrom(ctx); !ok {
		return nil, ErrUnauthenticated
	}
	if len(ids) > MaxNodes {
		return nil, fmt.Errorf("%w: at most %d ids", domain.ErrInvalidInput, MaxNodes)
	}
	for _, id := range ids {
		if err := validID(id); err != nil {
			return nil, err
		}
	}
	found := map[string]*Record{} // a nil value: the record exists, the caller may not read it
	pending := distinctNonEmpty(ids)
	for _, load := range s.kinds {
		if len(pending) == 0 {
			break
		}
		recs, err := load(ctx, pending)
		if err != nil {
			return nil, err
		}
		maps.Copy(found, recs)
		pending = slices.DeleteFunc(pending, func(id string) bool { _, ok := recs[id]; return ok })
	}
	out := make([]*Record, len(ids))
	for i, id := range ids {
		out[i] = found[id]
	}
	return out, nil
}

// validID rejects ids no record can have: longer than maxIDLength, not valid
// UTF-8, or holding a NUL byte (which Postgres refuses in text).
func validID(id string) error {
	if len(id) > maxIDLength || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		return fmt.Errorf("%w: malformed id", domain.ErrInvalidInput)
	}
	return nil
}

func distinctNonEmpty(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// kindOf builds a nodeKind from a batch repository read, a record's id, the
// use case's rule for what the caller may see, and the Record wrapper.
func kindOf[T any](find func(context.Context, []string) ([]T, error), id func(T) string,
	visible func(context.Context, []T) ([]*T, error), wrap func(*T) *Record,
) nodeKind {
	return func(ctx context.Context, ids []string) (map[string]*Record, error) {
		found, err := find(ctx, ids)
		if err != nil || len(found) == 0 {
			return nil, err
		}
		shown, err := visible(ctx, found)
		if err != nil {
			return nil, err
		}
		out := make(map[string]*Record, len(found))
		for i, v := range found {
			out[id(v)] = nil
			if shown[i] != nil {
				out[id(v)] = wrap(shown[i])
			}
		}
		return out, nil
	}
}
