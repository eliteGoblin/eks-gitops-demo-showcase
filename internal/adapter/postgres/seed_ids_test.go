package postgres_test

import (
	"context"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/pgtest"
)

// node(id) finds a record's type by looking its id up in each table, so ids
// must be unique across types. Generated ids are UUIDv7 (unique by
// construction); the seed's reference data keeps distinct prefixes. This
// guards the seed the database is deployed with, as the memory store's test
// guards its own.
func TestSeedIDsAreUniqueAcrossTypes(t *testing.T) {
	pool := pgtest.NewSchemaPool(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, postgres.MigrateOptions{Seed: true}); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `
		SELECT id, string_agg(kind, ', ' ORDER BY kind) FROM (
			SELECT id, 'participant' AS kind FROM participants
			UNION ALL SELECT id, 'plan' FROM plans
			UNION ALL SELECT id, 'provider' FROM providers
			UNION ALL SELECT id, 'claim' FROM claims
			UNION ALL SELECT id, 'support request' FROM support_requests
		) ids GROUP BY id HAVING count(*) > 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kinds string
		if err := rows.Scan(&id, &kinds); err != nil {
			t.Fatal(err)
		}
		t.Errorf("id %q is used by more than one type: %s", id, kinds)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
