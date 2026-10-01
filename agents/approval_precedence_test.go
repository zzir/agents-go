package agents

import "testing"

func approvalItem(tool, callID string) *ToolApprovalItem {
	return &ToolApprovalItem{ToolName: tool, CallID: callID}
}

func approvedCall(s *ApprovalStore, callID string) bool {
	d, ok := s.decisionFor("t", callID)
	return ok && d.approved
}

func rejectedCall(s *ApprovalStore, callID string) bool {
	d, ok := s.decisionFor("t", callID)
	return ok && !d.approved
}

// "Always approve" on one call and "reject" on another resolve the same way
// whichever was recorded first: the rejected call stays rejected.
func TestApprovalExactDecisionOutranksStandingInEitherOrder(t *testing.T) {
	standingFirst := NewApprovalStore()
	standingFirst.Approve(approvalItem("t", "c1"), true)
	standingFirst.Reject(approvalItem("t", "c2"), false, "no")

	exactFirst := NewApprovalStore()
	exactFirst.Reject(approvalItem("t", "c2"), false, "no")
	exactFirst.Approve(approvalItem("t", "c1"), true)

	for name, s := range map[string]*ApprovalStore{"standing first": standingFirst, "exact first": exactFirst} {
		if !approvedCall(s, "c1") || !rejectedCall(s, "c2") || !approvedCall(s, "c3") {
			t.Errorf("%s: c1 approved=%v, c2 rejected=%v, c3 approved=%v; want all true",
				name, approvedCall(s, "c1"), rejectedCall(s, "c2"), approvedCall(s, "c3"))
		}
	}
}

// An "always" decision made through a call replaces that call's own earlier
// decision: the person's last word on it stands.
func TestApprovalStandingDecisionReplacesItsOwnCallsEarlierOne(t *testing.T) {
	s := NewApprovalStore()
	s.Approve(approvalItem("t", "c2"), false)
	s.Reject(approvalItem("t", "c2"), true, "never")
	if !rejectedCall(s, "c2") {
		t.Error("approve once, then always reject, on the same call: want rejected")
	}

	s = NewApprovalStore()
	s.Reject(approvalItem("t", "c2"), false, "no")
	s.Approve(approvalItem("t", "c2"), true)
	if !approvedCall(s, "c2") {
		t.Error("reject once, then always approve, on the same call: want approved")
	}
}

// A nested run's store gets both decisions, so it resolves them the same way.
func TestApprovalMirrorKeepsStandingAndExactDecisions(t *testing.T) {
	src := NewApprovalStore()
	src.Approve(approvalItem("t", "c1"), true)
	src.Reject(approvalItem("t", "c2"), false, "no")

	dst := NewApprovalStore()
	src.mirrorInto(dst, []*ToolApprovalItem{approvalItem("t", "c1"), approvalItem("t", "c2")})
	if !approvedCall(dst, "c1") || !rejectedCall(dst, "c2") || !approvedCall(dst, "c9") {
		t.Errorf("mirrored: c1 approved=%v, c2 rejected=%v, c9 approved=%v; want all true",
			approvedCall(dst, "c1"), rejectedCall(dst, "c2"), approvedCall(dst, "c9"))
	}
	if d, _ := dst.decisionFor("t", "c2"); d.message != "no" {
		t.Errorf("mirrored rejection message = %q, want it carried", d.message)
	}
}
