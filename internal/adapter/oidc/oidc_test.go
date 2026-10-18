package oidc

import (
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

func TestClaimsPrincipal(t *testing.T) {
	c := Claims{Subject: "s1", PreferredUsername: "zhaolin.sun", ParticipantID: "P1", Expiry: 1_800_000_000}
	c.RealmAccess.Roles = []string{"participant", "default-roles-demo", "offline_access"}
	p := c.Principal()
	if p.Subject != "s1" || p.Username != "zhaolin.sun" || p.ParticipantID != "P1" || len(p.Roles) != 1 || !p.HasRole(app.RoleParticipant) {
		t.Fatalf("principal = %+v", p)
	}
	if !p.ExpiresAt.Equal(time.Unix(1_800_000_000, 0)) {
		t.Fatalf("ExpiresAt = %v, want the token's exp", p.ExpiresAt)
	}
}

func TestClaimsWithoutExpiryLeaveExpiresAtZero(t *testing.T) {
	if p := (Claims{Subject: "s1"}).Principal(); !p.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt = %v, want zero", p.ExpiresAt)
	}
}
