// Package audit is the Auditor adapter: one structured log line per decision.
// A durable audit table can replace it without touching the use cases.
package audit

import (
	"context"
	"log/slog"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

// Logger writes audit events as structured log lines.
type Logger struct{ log *slog.Logger }

// New returns an audit logger that writes to log.
func New(log *slog.Logger) *Logger { return &Logger{log: log} }

// Record writes one line: actor, action, resource (the claim or support
// request it concerns, with its plan and parties once read), outcome and
// detail.
func (l *Logger) Record(ctx context.Context, e app.AuditEvent) {
	l.log.InfoContext(ctx, "audit",
		"actor", e.Actor, "username", e.Username, "roles", e.Roles,
		"action", e.Action, "outcome", e.Outcome, "detail", e.Detail,
		"participant_id", e.Resource.ParticipantID, "plan_id", e.Resource.PlanID,
		"provider_id", e.Resource.ProviderID, "category", e.Resource.Category,
		"claim_id", e.Resource.ClaimID, "support_request_id", e.Resource.SupportRequestID)
}
