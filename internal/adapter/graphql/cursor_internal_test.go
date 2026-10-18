package graphql

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

func TestCursorsRoundTripInEveryOrder(t *testing.T) {
	at := time.Date(2026, 3, 4, 10, 30, 0, 123, time.UTC)
	for _, o := range []app.ClaimOrder{
		{}, {Field: app.OrderBySubmittedAt, Descending: true}, {Field: app.OrderByAmount}, {Field: app.OrderByAmount, Descending: true},
	} {
		want := app.NewClaimCursor(domain.Claim{ID: "C2", SubmittedAt: at, Amount: 180_50}, o)
		got, err := decodeCursor(encodeCursor(want))
		if err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
		if got.Order != want.Order || got.ID != "C2" || (o.Field == app.OrderByAmount && got.Amount != 180_50) ||
			(o.Field != app.OrderByAmount && !got.SubmittedAt.Equal(at)) {
			t.Errorf("%+v: round trip %+v, want %+v", o, got, want)
		}
	}
}

// Cursors handed out before ordering existed must keep working.
func TestDefaultOrderKeepsTheOriginalCursorForm(t *testing.T) {
	at := time.Date(2026, 3, 4, 10, 30, 0, 0, time.UTC)
	legacy := base64.RawURLEncoding.EncodeToString([]byte("claim:" + at.Format(time.RFC3339Nano) + "|C2"))
	if got := encodeCursor(app.NewClaimCursor(domain.Claim{ID: "C2", SubmittedAt: at}, app.ClaimOrder{})); got != legacy {
		t.Fatalf("default-order cursor changed: %s, want %s", got, legacy)
	}
}

func TestMalformedCursorsAreInvalidInput(t *testing.T) {
	for _, raw := range []string{"", "claim:|C2", "claim:not-a-time|C2", "claim2:AMOUNT:UP:1|C2", "claim2:NAME:ASC:1|C2",
		"claim2:AMOUNT:ASC:x|C2", "claim2:AMOUNT:ASC:1|", "other:1|C2"} {
		if _, err := decodeCursor(base64.RawURLEncoding.EncodeToString([]byte(raw))); !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("%q: %v", raw, err)
		}
	}
	if _, err := decodeCursor("!!"); !errors.Is(err, domain.ErrInvalidInput) {
		t.Errorf("not base64: %v", err)
	}
}
