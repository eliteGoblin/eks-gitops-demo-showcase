package memory

import "testing"

// node(id) finds a record's type by looking its id up, so ids must be unique
// across types; the seed's reference data keeps distinct prefixes for that.
func TestSeedIDsAreUniqueAcrossTypes(t *testing.T) {
	s := New()
	Seed(s)
	owner := map[string]string{}
	check := func(kind, id string) {
		if other, dup := owner[id]; dup {
			t.Errorf("id %q is both a %s and a %s", id, other, kind)
		}
		owner[id] = kind
	}
	for id := range s.participants {
		check("participant", id)
	}
	for id := range s.plans {
		check("plan", id)
	}
	for id := range s.providers {
		check("provider", id)
	}
	for id := range s.claims {
		check("claim", id)
	}
	for id := range s.requests {
		check("support request", id)
	}
}
