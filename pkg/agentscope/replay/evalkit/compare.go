package evalkit

import (
	"fmt"
	"strings"
)

// CompareReport contrasts two suite runs — the A/B deliverable for model /
// prompt / config changes (HARNESS_DESIGN C4).
type CompareReport struct {
	BaseSuite string
	CandSuite string
	FlippedUp []string // failing in base → passing in candidate
	FlippedDn []string // passing in base → failing in candidate
	BaseCost  float64
	CandCost  float64
	Rows      []CompareRow
}

// CompareRow is one task's side-by-side summary.
type CompareRow struct {
	TaskID    string
	BasePass  bool
	CandPass  bool
	BaseScore float64
	CandScore float64
	InDelta   int
	OutDelta  int
}

// Compare aligns two reports by (task, repeat) and contrasts outcomes.
func Compare(base, cand *SuiteReport) *CompareReport {
	type key struct {
		id     string
		repeat int
	}
	idx := map[key]TaskResult{}
	for i := range base.Results {
		r := &base.Results[i]
		idx[key{r.TaskID, r.Repeat}] = *r
	}
	out := &CompareReport{BaseSuite: base.Suite, CandSuite: cand.Suite}
	for i := range cand.Results {
		cr := &cand.Results[i]
		br, ok := idx[key{cr.TaskID, cr.Repeat}]
		if !ok {
			continue // new task in candidate; not a flip
		}
		row := CompareRow{
			TaskID:    cr.TaskID,
			BasePass:  br.Pass,
			CandPass:  cr.Pass,
			BaseScore: br.Score,
			CandScore: cr.Score,
			InDelta:   cr.InputTokens - br.InputTokens,
			OutDelta:  cr.OutputTokens - br.OutputTokens,
		}
		out.Rows = append(out.Rows, row)
		out.BaseCost += br.CostUSD
		out.CandCost += cr.CostUSD
		switch {
		case !br.Pass && cr.Pass:
			out.FlippedUp = append(out.FlippedUp, cr.TaskID)
		case br.Pass && !cr.Pass:
			out.FlippedDn = append(out.FlippedDn, cr.TaskID)
		}
		delete(idx, key{cr.TaskID, cr.Repeat})
	}
	return out
}

// Verdict summarizes the comparison.
func (c *CompareReport) Verdict() string {
	switch {
	case len(c.FlippedDn) == 0 && len(c.FlippedUp) > 0:
		return "candidate strictly better (gains, no regressions)"
	case len(c.FlippedDn) == 0 && len(c.FlippedUp) == 0:
		return "no behavioral difference"
	default:
		return fmt.Sprintf("%d regression(s), %d gain(s)", len(c.FlippedDn), len(c.FlippedUp))
	}
}

// Markdown renders the comparison report.
func (c *CompareReport) Markdown() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# A/B Compare: %s (base) vs %s (candidate)\n\n", c.BaseSuite, c.CandSuite)
	fmt.Fprintf(&sb, "**Verdict:** %s\n\n", c.Verdict())
	if c.BaseCost > 0 || c.CandCost > 0 {
		fmt.Fprintf(&sb, "- Cost: base $%.4f → candidate $%.4f (Δ $%+.4f)\n", c.BaseCost, c.CandCost, c.CandCost-c.BaseCost)
	}
	if len(c.FlippedUp) > 0 {
		fmt.Fprintf(&sb, "- ⬆️ flipped to pass: %s\n", strings.Join(c.FlippedUp, ", "))
	}
	if len(c.FlippedDn) > 0 {
		fmt.Fprintf(&sb, "- ⬇️ flipped to fail: %s\n", strings.Join(c.FlippedDn, ", "))
	}
	sb.WriteString("\n| Task | Base | Candidate | ΔScore | ΔIn/Out tokens |\n")
	sb.WriteString("|------|------|-----------|--------|----------------|\n")
	for _, r := range c.Rows {
		bp, cp := "❌", "❌"
		if r.BasePass {
			bp = "✅"
		}
		if r.CandPass {
			cp = "✅"
		}
		fmt.Fprintf(&sb, "| %s | %s %.2f | %s %.2f | %+.2f | %+d/%+d |\n",
			r.TaskID, bp, r.BaseScore, cp, r.CandScore,
			r.CandScore-r.BaseScore, r.InDelta, r.OutDelta)
	}
	return sb.String()
}
