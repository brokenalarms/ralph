package git

import "testing"

func TestCollectStackFromPRs_BottomUpOrder(t *testing.T) {
	allPRs := []PRInfo{
		{Number: 452, Head: "feature/a", Base: "main", State: "OPEN"},
		{Number: 459, Head: "feature/b", Base: "feature/a", State: "OPEN"},
		{Number: 460, Head: "feature/c", Base: "feature/b", State: "OPEN"},
	}

	result := collectStackFromPRs(allPRs, "460")

	if len(result.prs) != 3 {
		t.Fatalf("expected 3 PRs, got %d", len(result.prs))
	}
	if result.prs[0].number != 452 {
		t.Errorf("expected prs[0]=452 (bottom), got %d", result.prs[0].number)
	}
	if result.prs[1].number != 459 {
		t.Errorf("expected prs[1]=459, got %d", result.prs[1].number)
	}
	if result.prs[2].number != 460 {
		t.Errorf("expected prs[2]=460 (top), got %d", result.prs[2].number)
	}
	if result.baseBranch != "main" {
		t.Errorf("expected baseBranch=main, got %s", result.baseBranch)
	}
}

func TestCollectStackFromPRs_SkipsClosedPRs(t *testing.T) {
	allPRs := []PRInfo{
		{Number: 452, Head: "feature/a", Base: "main", State: "OPEN"},
		{Number: 459, Head: "feature/b", Base: "feature/a", State: "CLOSED"},
		{Number: 460, Head: "feature/c", Base: "feature/b", State: "OPEN"},
	}

	result := collectStackFromPRs(allPRs, "460")

	if len(result.prs) != 2 {
		t.Fatalf("expected 2 PRs (CLOSED skipped), got %d", len(result.prs))
	}
	if result.prs[0].number != 452 {
		t.Errorf("expected prs[0]=452, got %d", result.prs[0].number)
	}
	if result.prs[1].number != 460 {
		t.Errorf("expected prs[1]=460, got %d", result.prs[1].number)
	}
}

func TestCollectStackFromPRs_NonMainBaseBranch(t *testing.T) {
	allPRs := []PRInfo{
		{Number: 100, Head: "feature/x", Base: "develop", State: "OPEN"},
	}

	result := collectStackFromPRs(allPRs, "100")

	if result.baseBranch != "develop" {
		t.Errorf("expected baseBranch=develop, got %s", result.baseBranch)
	}
	if len(result.prs) != 1 || result.prs[0].number != 100 {
		t.Errorf("unexpected prs: %+v", result.prs)
	}
}
