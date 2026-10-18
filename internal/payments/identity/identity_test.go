package identity

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn/authntest"
)

type jsonClaims string

func (j jsonClaims) Claims(dst any) error { return json.Unmarshal([]byte(j), dst) }

func TestCallerPermissionsComeFromTheResourceClientRoles(t *testing.T) {
	claims := jsonClaims(`{"resource_access":{
		"payments-svc":{"roles":["payments:write","payments:admin"]},
		"other-api":{"roles":["payments:read"]}}}`)
	c, err := caller("sa-1", "demo-api-svc", claims, "payments-svc")
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "sa-1" || c.ClientID != "demo-api-svc" || !slices.Equal(c.Permissions, []payments.Permission{payments.PermWrite}) {
		t.Fatalf("caller = %+v (unknown roles and other clients' roles must be ignored)", c)
	}
	none, _ := caller("sa-2", "spa", jsonClaims(`{}`), "payments-svc")
	if len(none.Permissions) != 0 {
		t.Fatalf("no resource_access means no permissions, got %+v", none)
	}
	if _, err := caller("s", "c", jsonClaims(`not json`), "payments-svc"); err == nil {
		t.Fatal("undecodable claims must be an error")
	}
	if _, err := caller("s", "", jsonClaims(`{}`), "payments-svc"); !errors.Is(err, ErrNoClient) {
		t.Fatalf("a token without a client id must be rejected, got %v", err)
	}
}

func TestMapperOnAVerifiedToken(t *testing.T) {
	idp := authntest.NewIdP(t)
	v := authn.New(authn.Config{Issuer: idp.URL, Audience: "payments-svc"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := v.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	claims := idp.AccessToken("payments-svc", "demo-api-svc")
	claims["resource_access"] = map[string]any{"payments-svc": map[string]any{"roles": []string{"payments:write"}}}

	got, err := v.Authenticator(Mapper("payments-svc")).Authenticate(context.Background(), idp.Sign(t, claims))
	if err != nil {
		t.Fatal(err)
	}
	// With payments:write, Create gets past authorization and fails on the empty
	// idempotency key; without it, it would return ErrForbidden.
	svc := payments.NewService(nil, time.Now, func() string { return "p" })
	if _, _, err := svc.Create(got, "", payments.Request{}); err == nil || errors.Is(err, payments.ErrForbidden) {
		t.Fatalf("the mapped caller must carry payments:write (want a validation error, got %v)", err)
	}
}
