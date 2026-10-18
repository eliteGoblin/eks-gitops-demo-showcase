package graphql

import (
	"strings"
	"testing"

	gql "github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql/generated"
)

// Every field behind @auth must name the use-case action it attempts, so a
// refusal at the gate is audited in the app's vocabulary, not the fallback.
func TestEveryGatedFieldNamesItsAction(t *testing.T) {
	schema := generated.NewExecutableSchema(generated.Config{}).Schema()
	gated := 0
	for _, def := range schema.Types {
		for _, f := range def.Fields {
			if f.Directives.ForName("auth") == nil {
				continue
			}
			gated++
			fc := &gql.FieldContext{Object: def.Name, Field: gql.CollectedField{Field: &ast.Field{Name: f.Name}}, Args: map[string]any{}}
			// App actions read "noun:verb" (or "search"); only the fallback has a dot.
			if a, _ := attempt(fc); a == "" || strings.Contains(string(a), ".") {
				t.Errorf("%s.%s is behind @auth, but attempt does not name its action (got %q)", def.Name, f.Name, a)
			}
		}
	}
	if gated == 0 {
		t.Fatal("no field carries @auth: the schema was not read")
	}
}
