package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
)

const schemaDir = "../../internal/adapter/graphql/schema"

func writeDocs(t *testing.T, docs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range docs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGenerateWritesCanonicalDocumentsByHash(t *testing.T) {
	dir := writeDocs(t, map[string]string{"me.graphql": "query Me { me { subject } }", "notes.txt": "ignored"})
	manifest, err := generate(dir, schemaDir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := graphql.ParseAllowlist(manifest)
	if err != nil {
		t.Fatalf("the generated manifest must load: %v", err)
	}
	_, hash, _ := graphql.CanonicalDocument("query Me { me { subject } }")
	if len(list) != 1 || list[hash] == "" {
		t.Fatalf("want exactly the one document, got %v", list)
	}
	if !strings.HasSuffix(string(manifest), "\n") || !json.Valid(manifest) {
		t.Fatal("manifest must be JSON ending in a newline")
	}
}

func TestGenerateRejectsDocumentsTheSchemaDoesNotAccept(t *testing.T) {
	dir := writeDocs(t, map[string]string{"bad.graphql": "query Bad { me { noSuchField } }"})
	_, err := generate(dir, schemaDir)
	if err == nil || !strings.Contains(err.Error(), "bad.graphql") {
		t.Fatalf("want a validation error naming the file, got %v", err)
	}
}

func TestGenerateRejectsDuplicatesAndEmptyInput(t *testing.T) {
	dup := writeDocs(t, map[string]string{"a.graphql": "query Me { me { subject } }", "b.graphql": "query Me{me{subject}}"})
	if _, err := generate(dup, schemaDir); err == nil || !strings.Contains(err.Error(), "same document") {
		t.Fatalf("want a duplicate error, got %v", err)
	}
	if _, err := generate(t.TempDir(), schemaDir); err == nil {
		t.Fatal("no documents must be an error, not an empty allowlist")
	}
}

func TestCheckDetectsAStaleManifest(t *testing.T) {
	dir := writeDocs(t, map[string]string{"me.graphql": "query Me { me { subject } }"})
	out := filepath.Join(t.TempDir(), "queries.json")
	if err := run([]string{"-docs", dir, "-schema", schemaDir, "-out", out}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-docs", dir, "-schema", schemaDir, "-out", out, "-check"}); err != nil {
		t.Fatalf("fresh manifest must pass -check: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roles.graphql"), []byte("query Roles { me { roles } }"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-docs", dir, "-schema", schemaDir, "-out", out, "-check"}); err == nil {
		t.Fatal("a manifest missing a document must fail -check")
	}
}
