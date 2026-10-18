// Command persistedgen builds the trusted-documents manifest that
// PERSISTED_QUERIES=allowlist serves from: it reads every *.graphql file in a
// directory, validates each against the GraphQL schema, canonicalises it and
// writes a JSON object of SHA-256 → document. With -check it only compares
// the result with the committed manifest and fails when that is stale.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "persistedgen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("persistedgen", flag.ContinueOnError)
	docs := fs.String("docs", "persisted/documents", "directory of *.graphql trusted documents")
	schema := fs.String("schema", "internal/adapter/graphql/schema", "directory of *.graphqls schema files")
	out := fs.String("out", "persisted/queries.json", "manifest to write")
	check := fs.Bool("check", false, "fail if the manifest is not up to date instead of writing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := generate(*docs, *schema)
	if err != nil {
		return err
	}
	if !*check {
		return os.WriteFile(*out, manifest, 0o644) //nolint:gosec // a public manifest, not a secret
	}
	current, err := os.ReadFile(*out) //nolint:gosec // a path from the command line of a dev tool
	if err != nil {
		return err
	}
	if !bytes.Equal(current, manifest) {
		return fmt.Errorf("%s is stale: run `make persisted-queries` and commit it", *out)
	}
	return nil
}

// generate returns the manifest for the documents in docsDir.
func generate(docsDir, schemaDir string) ([]byte, error) {
	schema, err := loadSchema(schemaDir)
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(docsDir, "*.graphql"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no *.graphql documents in %s", docsDir)
	}
	sort.Strings(files)
	manifest := map[string]string{}
	source := map[string]string{} // hash → file, to name duplicates
	for _, file := range files {
		body, err := os.ReadFile(file) //nolint:gosec // documents from the repository
		if err != nil {
			return nil, err
		}
		if _, errs := gqlparser.LoadQueryWithRules(schema, string(body), nil); len(errs) > 0 {
			return nil, fmt.Errorf("%s: %w", filepath.Base(file), errs)
		}
		doc, hash, err := graphql.CanonicalDocument(string(body))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(file), err)
		}
		if first, dup := source[hash]; dup {
			return nil, fmt.Errorf("%s and %s are the same document", first, filepath.Base(file))
		}
		source[hash] = filepath.Base(file)
		manifest[hash] = doc
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ") // map keys are written sorted
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func loadSchema(dir string) (*ast.Schema, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.graphqls"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("no *.graphqls schema files in " + dir)
	}
	sources := make([]*ast.Source, 0, len(files))
	for _, f := range files {
		body, err := os.ReadFile(f) //nolint:gosec // schema files from the repository
		if err != nil {
			return nil, err
		}
		sources = append(sources, &ast.Source{Name: f, Input: string(body)})
	}
	return gqlparser.LoadSchema(sources...)
}
