package risk

import (
	"testing"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

func TestApproval(t *testing.T) {
	if !(Approval{}).IsZero() {
		t.Error("zero Approval is not zero")
	}
	intent := domain.OrderIntent{CycleID: "c1", Side: domain.ActionBuy, Quantity: 1}
	a := ApprovalForTest(t, intent)
	if a.IsZero() || a.Intent() != intent {
		t.Errorf("Approval = %+v, want intent %+v", a, intent)
	}
}
