package model

import "github.com/eliteGoblin/eks-gitops-demo/internal/domain"

// Claim wraps the domain claim so it can join the payload unions without the
// domain type carrying GraphQL marker methods.
type Claim struct{ domain.Claim }

func (Claim) IsSubmitClaimPayload()  {}
func (Claim) IsApproveClaimPayload() {}

// WrapClaims converts domain claims for the GraphQL layer.
func WrapClaims(in []domain.Claim) []*Claim {
	out := make([]*Claim, len(in))
	for i := range in {
		out[i] = &Claim{in[i]}
	}
	return out
}

// SupportRequest wraps the domain request for the payload unions.
type SupportRequest struct{ domain.SupportRequest }

func (SupportRequest) IsRaiseSupportRequestPayload()  {}
func (SupportRequest) IsDecideSupportRequestPayload() {}

// WrapSupportRequests converts domain requests for the GraphQL layer.
func WrapSupportRequests(in []domain.SupportRequest) []*SupportRequest {
	out := make([]*SupportRequest, len(in))
	for i := range in {
		out[i] = &SupportRequest{in[i]}
	}
	return out
}
