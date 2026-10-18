package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/audit"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

// Each line names the record it concerns, so the trail answers "who did what
// to which claim (or support request)" on its own.
func TestRecordNamesTheResource(t *testing.T) {
	var buf bytes.Buffer
	audit.New(slog.New(slog.NewJSONHandler(&buf, nil))).Record(context.Background(), app.AuditEvent{
		Actor: "sub-1", Action: app.ActionApproveClaim, Outcome: "paid", Detail: "pay_1",
		Resource: app.Resource{PlanID: "PL1", ClaimID: "C9", SupportRequestID: "SR3"},
	})
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not one JSON line: %v (%q)", err, buf.String())
	}
	for key, want := range map[string]string{"action": "claim:approve", "outcome": "paid", "claim_id": "C9", "support_request_id": "SR3", "plan_id": "PL1"} {
		if line[key] != want {
			t.Errorf("%s = %v, want %q", key, line[key], want)
		}
	}
}
