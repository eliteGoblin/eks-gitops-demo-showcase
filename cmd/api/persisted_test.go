package main

import (
	"context"
	"fmt"
	"testing"

	gqlgen "github.com/99designs/gqlgen/graphql"
)

func TestLoadConfigPersistedQueries(t *testing.T) {
	base := map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo"}
	cfg, err := loadConfig(envOf(base))
	if err != nil || cfg.PersistedQueries != persistedAPQ || cfg.PersistedQueryCache != 1000 {
		t.Fatalf("defaults: %q %d %v", cfg.PersistedQueries, cfg.PersistedQueryCache, err)
	}
	for _, bad := range []map[string]string{
		{"PERSISTED_QUERIES": "sometimes"},
		{"PERSISTED_QUERIES_CACHE": "0"},
	} {
		vars := map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo"}
		for k, v := range bad {
			vars[k] = v
		}
		if _, err := loadConfig(envOf(vars)); err == nil {
			t.Errorf("%v must be rejected", bad)
		}
	}
}

func TestPersistedQueryPolicyPerMode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string // extension name, "" for none
	}{
		{persistedOff, ""},
		{persistedAPQ, "AutomaticPersistedQuery"},
		{persistedAllowlist, "TrustedDocuments"}, // from the manifest embedded in the binary
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ext, err := persistedQueryPolicy(config{PersistedQueries: tc.mode, PersistedQueryCache: 10})
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if ext != nil {
				got = ext.ExtensionName()
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// In allowlist mode introspection-only documents run only when both the
// playground and introspection are switched on: the playground alone must not
// open the schema.
func TestAllowlistAdmitsIntrospectionOnlyWithPlaygroundAndIntrospection(t *testing.T) {
	const introspection = "{ __schema { queryType { name } } }"
	for _, tc := range []struct {
		playground, introspection, admitted bool
	}{
		{false, false, false},
		{true, false, false},
		{false, true, false},
		{true, true, true},
	} {
		ext, err := persistedQueryPolicy(config{PersistedQueries: persistedAllowlist, Playground: tc.playground, Introspection: tc.introspection})
		if err != nil {
			t.Fatal(err)
		}
		mutator, ok := ext.(gqlgen.OperationParameterMutator)
		if !ok {
			t.Fatal("trusted documents must check the operation parameters")
		}
		gerr := mutator.MutateOperationParameters(context.Background(), &gqlgen.RawParams{Query: introspection})
		if admitted := gerr == nil; admitted != tc.admitted {
			t.Errorf("playground=%v introspection=%v: admitted=%v, want %v (%v)", tc.playground, tc.introspection, admitted, tc.admitted, gerr)
		}
	}
}

func TestLoadConfigMaxDocumentBytes(t *testing.T) {
	base := map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo"}
	cfg, err := loadConfig(envOf(base))
	if err != nil || cfg.MaxDocumentBytes != 32<<10 {
		t.Fatalf("default: %d %v", cfg.MaxDocumentBytes, err)
	}
	for _, bad := range []string{"0", "512", fmt.Sprint(maxRequestBytes + 1)} {
		vars := map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo", "GRAPHQL_MAX_DOCUMENT_BYTES": bad}
		if _, err := loadConfig(envOf(vars)); err == nil {
			t.Errorf("GRAPHQL_MAX_DOCUMENT_BYTES=%s must be rejected", bad)
		}
	}
}
