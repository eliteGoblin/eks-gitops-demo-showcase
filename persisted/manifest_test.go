package persisted_test

import (
	"regexp"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
	"github.com/eliteGoblin/eks-gitops-demo/persisted"
	"github.com/eliteGoblin/eks-gitops-demo/web"
)

func TestManifestLoads(t *testing.T) {
	if _, err := graphql.ParseAllowlist(persisted.Manifest); err != nil {
		t.Fatal(err)
	}
}

// Every document the demo pages send is trusted, so the pages keep working
// in allowlist mode. A page edit without `make persisted-queries` fails here.
func TestThePagesDocumentsAreTrusted(t *testing.T) {
	list, err := graphql.ParseAllowlist(persisted.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	constant := regexp.MustCompile("(?s)const ([A-Z_]+_QUERY) = `(.*?)`;")
	found := 0
	for _, file := range []string{"index.js", "playground.js"} {
		src, err := web.FS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range constant.FindAllStringSubmatch(string(src), -1) {
			found++
			_, hash, err := graphql.CanonicalDocument(m[2])
			if err != nil {
				t.Fatalf("%s %s: %v", file, m[1], err)
			}
			if _, ok := list[hash]; !ok {
				t.Errorf("%s %s is not in persisted/queries.json (run `make persisted-queries`)", file, m[1])
			}
		}
	}
	if found < 3 {
		t.Fatalf("expected the pages' query constants, found %d", found)
	}
}
