package report

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// Sheet names — kept as constants so tests and callers stay in sync with
// builder code. Order here is the order they appear in the workbook.
const (
	SheetREADME         = "README"
	SheetActionQueue    = "Action Queue"
	SheetSummary        = "Summary"
	SheetByRule         = "By Rule"
	SheetByAuthor       = "By Author"
	SheetDecisionMatrix = "Decision Matrix"
	SheetWaiversLog     = "Waivers Log"
	SheetMultiplePRs    = "Multiple PRs"
)

// xlsxBuilder bundles the workbook and the shared styles so each per-sheet
// builder doesn't re-create them.
type xlsxBuilder struct {
	f          *excelize.File
	linkStyle  int
	headerBlue int
	headerRed  int
	headerGrey int
	boldStyle  int
	outcomeStyles map[RuleOutcome]int
}

// GenerateXLSX creates the 3-layer audit workbook: Action → Overview → Trace.
func (r *Reporter) GenerateXLSX(ctx context.Context, opts ReportOpts, outputPath string) error {
	summary, err := r.GetSummary(ctx, opts)
	if err != nil {
		return fmt.Errorf("getting summary: %w", err)
	}
	details, err := r.GetDetails(ctx, opts)
	if err != nil {
		return fmt.Errorf("getting details: %w", err)
	}
	byAuthor, err := r.GetByAuthor(ctx, opts)
	if err != nil {
		return fmt.Errorf("getting by-author: %w", err)
	}
	multiplePRs, err := r.GetMultiplePRDetails(ctx, opts)
	if err != nil {
		return fmt.Errorf("getting multiple PR details: %w", err)
	}

	f := excelize.NewFile()
	defer f.Close()

	b, err := newBuilder(f)
	if err != nil {
		return err
	}

	f.SetSheetName("Sheet1", SheetREADME)
	if err := b.writeREADME(opts); err != nil {
		return fmt.Errorf("README: %w", err)
	}

	for _, name := range []string{SheetActionQueue, SheetSummary, SheetByRule, SheetByAuthor, SheetDecisionMatrix, SheetWaiversLog, SheetMultiplePRs} {
		if _, err := f.NewSheet(name); err != nil {
			return fmt.Errorf("creating %s: %w", name, err)
		}
	}

	if err := b.writeActionQueue(details); err != nil {
		return fmt.Errorf("%s: %w", SheetActionQueue, err)
	}
	if err := b.writeSummary(summary, opts); err != nil {
		return fmt.Errorf("%s: %w", SheetSummary, err)
	}
	if err := b.writeByRule(details); err != nil {
		return fmt.Errorf("%s: %w", SheetByRule, err)
	}
	if err := b.writeByAuthor(byAuthor); err != nil {
		return fmt.Errorf("%s: %w", SheetByAuthor, err)
	}
	if err := b.writeDecisionMatrix(details); err != nil {
		return fmt.Errorf("%s: %w", SheetDecisionMatrix, err)
	}
	if err := b.writeWaiversLog(details); err != nil {
		return fmt.Errorf("%s: %w", SheetWaiversLog, err)
	}
	if err := b.writeMultiplePRs(multiplePRs); err != nil {
		return fmt.Errorf("%s: %w", SheetMultiplePRs, err)
	}

	// Open on Action Queue by default — it's the sheet auditors live in.
	if idx, err := f.GetSheetIndex(SheetActionQueue); err == nil {
		f.SetActiveSheet(idx)
	}

	return f.SaveAs(outputPath)
}

func newBuilder(f *excelize.File) (*xlsxBuilder, error) {
	linkStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "0563C1", Underline: "single"},
	})
	if err != nil {
		return nil, err
	}
	blueHeader, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"4472C4"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}
	redHeader, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"CC4444"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}
	greyHeader, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"707070"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}
	bold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return nil, err
	}

	mk := func(bg, fg string) int {
		s, _ := f.NewStyle(&excelize.Style{
			Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{bg}},
			Font:      &excelize.Font{Color: fg},
			Alignment: &excelize.Alignment{Horizontal: "center"},
		})
		return s
	}
	outcomeStyles := map[RuleOutcome]int{
		OutcomePass:    mk("C6EFCE", "006100"),
		OutcomeFail:    mk("FFC7CE", "9C0006"),
		OutcomeMissing: mk("FFE699", "7F5A00"),
		OutcomeWaived:  mk("D9E7F9", "1F4E79"),
		OutcomeSkip:    mk("E7E6E6", "595959"),
		OutcomeNA:      mk("F2F2F2", "7F7F7F"),
	}

	return &xlsxBuilder{
		f:             f,
		linkStyle:     linkStyle,
		headerBlue:    blueHeader,
		headerRed:     redHeader,
		headerGrey:    greyHeader,
		boldStyle:     bold,
		outcomeStyles: outcomeStyles,
	}, nil
}

// --- README ---------------------------------------------------------------

func (b *xlsxBuilder) writeREADME(opts ReportOpts) error {
	sheet := SheetREADME
	f := b.f

	title, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 16}})
	h2, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 12}})
	body, _ := f.NewStyle(&excelize.Style{Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})

	lines := []struct {
		text  string
		style int
	}{
		{"gh-audit compliance report", title},
		{fmt.Sprintf("Generated: %s", time.Now().UTC().Format("2006-01-02 15:04 MST")), 0},
		{reportPeriod(opts), 0},
		{"", 0},
		{"How to read this workbook", h2},
		{"• Action Queue — prioritized list of commits that need attention. Start here.", body},
		{"• Summary — per-repo totals and compliance %. Filterable overview.", body},
		{"• By Rule — which rules trigger most across the scanned set.", body},
		{"• By Author — per-author rollup to spot patterns.", body},
		{"• Decision Matrix — every commit × every rule (pass/fail/skip/n-a/missing/waived). Autofilter on any rule column to drill in.", body},
		{"• Waivers Log — commits auto-approved by R1/R2/R8 or classified as clean merges. Evidence of what the tool did NOT flag and why.", body},
		{"• Multiple PRs — commits associated with more than one PR.", body},
		{"", 0},
		{"Rule legend", h2},
		{"R1 Exempt — author is in the configured exemption list", body},
		{"R2 Empty — commit has 0 additions and 0 deletions", body},
		{"R3 HasPR — commit is associated with a merged pull request", body},
		{"R4 FinalApproval — a non-self APPROVED review exists on the PR's final commit", body},
		{"R4b Stale — approval exists but only on pre-force-push SHAs", body},
		{"R4c PostMergeConcern — CHANGES_REQUESTED or DISMISSED submitted after merge (informational)", body},
		{"R5 SelfApproval — the only approver is the code author / committer / co-author", body},
		{"R6 OwnerCheck — required status check (e.g. Owner Approval) configured for this repo", body},
		{"R7 Verdict — overall compliance verdict from the audit pipeline", body},
		{"R8 RevertWaiver — clean-revert waiver applied (bot auto-revert or diff-verified manual revert)", body},
		{"", 0},
		{"Cell outcomes", h2},
		{"pass — rule evaluated and passed", body},
		{"fail — rule evaluated and failed (contributes to non-compliance unless waived)", body},
		{"skip — rule did not apply to this commit (e.g. R1/R2 inactive)", body},
		{"n/a — rule could not evaluate (e.g. R4/R5 when there's no PR)", body},
		{"missing — required status check was never reported", body},
		{"waived — rule waived the verdict (R1 exempt author, R2 empty, R8 clean revert)", body},
		{"", 0},
		{"Full rule definitions: Architecture.md § What gh-audit detects.", body},
	}

	for i, line := range lines {
		cell := cellName(1, i+1)
		f.SetCellValue(sheet, cell, line.text)
		if line.style != 0 {
			f.SetCellStyle(sheet, cell, cell, line.style)
		}
	}
	f.SetColWidth(sheet, "A", "A", 110)
	return nil
}

func reportPeriod(opts ReportOpts) string {
	if opts.Since.IsZero() && opts.Until.IsZero() {
		return "Report period: all time"
	}
	since, until := "beginning", "present"
	if !opts.Since.IsZero() {
		since = opts.Since.Format("2006-01-02")
	}
	if !opts.Until.IsZero() {
		until = opts.Until.Format("2006-01-02")
	}
	return fmt.Sprintf("Report period: %s to %s", since, until)
}

// --- Action Queue ---------------------------------------------------------

func (b *xlsxBuilder) writeActionQueue(details []DetailRow) error {
	sheet := SheetActionQueue
	f := b.f

	headers := []string{
		"Priority", "Severity", "Repo", "SHA", "PR #", "Author", "Merged By",
		"Failing Rule", "Prescribed Action", "Days Since Commit", "Resolution", "Notes",
	}

	type queueRow struct {
		d        DetailRow
		severity Severity
		rule     string
		action   string
	}
	var queue []queueRow
	for _, d := range details {
		o := DeriveRuleOutcomes(d)
		if !o.RequiresAction() {
			continue
		}
		sev, rule, act := SynthesizeAction(d, o)
		queue = append(queue, queueRow{d: d, severity: sev, rule: rule, action: act})
	}

	sort.SliceStable(queue, func(i, j int) bool {
		if ri, rj := severityRank(queue[i].severity), severityRank(queue[j].severity); ri != rj {
			return ri > rj
		}
		if queue[i].d.Repo != queue[j].d.Repo {
			return queue[i].d.Org+"/"+queue[i].d.Repo < queue[j].d.Org+"/"+queue[j].d.Repo
		}
		return queue[i].d.CommittedAt.After(queue[j].d.CommittedAt)
	})

	b.writeHeaderRow(sheet, headers, b.headerRed)

	now := time.Now()
	for i, q := range queue {
		row := i + 2
		d := q.d
		f.SetCellValue(sheet, cellName(1, row), i+1)
		f.SetCellValue(sheet, cellName(2, row), string(q.severity))
		f.SetCellValue(sheet, cellName(3, row), d.Org+"/"+d.Repo)
		b.writeSHACell(sheet, cellName(4, row), d.SHA, d.CommitHref)
		b.writePRCell(sheet, cellName(5, row), d.PRNumber, d.PRHref)
		f.SetCellValue(sheet, cellName(6, row), sanitizeCell(d.AuthorLogin))
		f.SetCellValue(sheet, cellName(7, row), sanitizeCell(d.MergedByLogin))
		f.SetCellValue(sheet, cellName(8, row), q.rule)
		f.SetCellValue(sheet, cellName(9, row), sanitizeCell(q.action))
		if !d.CommittedAt.IsZero() {
			f.SetCellValue(sheet, cellName(10, row), int(now.Sub(d.CommittedAt).Hours()/24))
		}
		// Resolution + Notes intentionally blank for auditor use.
	}

	b.finalizeSheet(sheet, headers, len(queue))
	widths := []float64{8, 10, 25, 12, 8, 18, 18, 22, 50, 12, 25, 30}
	applyWidths(f, sheet, widths)
	return nil
}

// --- Summary --------------------------------------------------------------

func (b *xlsxBuilder) writeSummary(summary []SummaryRow, opts ReportOpts) error {
	sheet := SheetSummary
	f := b.f

	headers := []string{
		"Org", "Repo", "Total", "Compliant", "Non-Compliant", "Compliance %",
		"Action Queue", "Waived",
		"R3 NoPR", "R4 NoFinal", "R6 OwnerFail",
		"Self-Approved", "Stale", "Post-Merge", "Clean Reverts", "Clean Merges",
		"Bots", "Exempt", "Empty", "Multiple PRs",
	}

	subtitleStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 12},
		Alignment: &excelize.Alignment{Horizontal: "left"},
	})
	f.SetCellValue(sheet, "A1", reportPeriod(opts))
	f.SetCellStyle(sheet, "A1", "A1", subtitleStyle)

	for col, h := range headers {
		cell := cellName(col+1, 2)
		f.SetCellValue(sheet, cell, h)
		f.SetCellStyle(sheet, cell, cell, b.headerBlue)
	}

	pctFmt := "0.0"
	greenStyle, _ := f.NewStyle(&excelize.Style{
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"C6EFCE"}},
		Font: &excelize.Font{Color: "006100"}, CustomNumFmt: &pctFmt,
	})
	yellowStyle, _ := f.NewStyle(&excelize.Style{
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFEB9C"}},
		Font: &excelize.Font{Color: "9C5700"}, CustomNumFmt: &pctFmt,
	})
	redStyle, _ := f.NewStyle(&excelize.Style{
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFC7CE"}},
		Font: &excelize.Font{Color: "9C0006"}, CustomNumFmt: &pctFmt,
	})

	for i, s := range summary {
		row := i + 3
		vals := []any{
			s.Org, s.Repo, s.TotalCommits, s.CompliantCount, s.NonCompliantCount,
			s.CompliancePct,
			s.ActionQueueCount, s.WaivedCount,
			s.R3NoPRCount, s.R4NoFinalCount, s.R6OwnerFailCount,
			s.SelfApprovedCount, s.StaleApprovalCount, s.PostMergeConcernCount,
			s.CleanRevertCount, s.CleanMergeCount,
			s.BotCount, s.ExemptCount, s.EmptyCount, s.MultiplePRCount,
		}
		for c, v := range vals {
			f.SetCellValue(sheet, cellName(c+1, row), v)
		}
		pctCell := cellName(6, row)
		switch {
		case s.CompliancePct >= 100:
			f.SetCellStyle(sheet, pctCell, pctCell, greenStyle)
		case s.CompliancePct >= 90:
			f.SetCellStyle(sheet, pctCell, pctCell, yellowStyle)
		default:
			f.SetCellStyle(sheet, pctCell, pctCell, redStyle)
		}
	}

	if len(summary) > 0 {
		totalsRow := len(summary) + 3
		f.SetCellValue(sheet, cellName(1, totalsRow), "TOTAL")
		// SUM all numeric columns except Compliance %.
		for _, col := range []int{3, 4, 5, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20} {
			colLetter, _ := excelize.ColumnNumberToName(col)
			f.SetCellFormula(sheet, cellName(col, totalsRow),
				fmt.Sprintf("SUM(%s3:%s%d)", colLetter, colLetter, totalsRow-1))
		}
		colC, _ := excelize.ColumnNumberToName(3)
		colD, _ := excelize.ColumnNumberToName(4)
		f.SetCellFormula(sheet, cellName(6, totalsRow),
			fmt.Sprintf("IF(%s%d>0,ROUND(%s%d/%s%d*100,1),0)", colC, totalsRow, colD, totalsRow, colC, totalsRow))
		pctStyle, _ := f.NewStyle(&excelize.Style{
			Font: &excelize.Font{Bold: true}, CustomNumFmt: &pctFmt,
		})
		for col := 1; col <= len(headers); col++ {
			c := cellName(col, totalsRow)
			if col == 6 {
				f.SetCellStyle(sheet, c, c, pctStyle)
			} else {
				f.SetCellStyle(sheet, c, c, b.boldStyle)
			}
		}
	}

	f.SetPanes(sheet, &excelize.Panes{Freeze: true, YSplit: 2, TopLeftCell: "A3", ActivePane: "bottomLeft"})
	widths := []float64{14, 28, 8, 11, 14, 12, 12, 8, 10, 11, 13, 13, 8, 12, 14, 13, 8, 8, 8, 12}
	applyWidths(f, sheet, widths)
	return nil
}

// --- By Rule --------------------------------------------------------------

type byRuleAgg struct {
	rule          string
	name          string
	fires         int
	compliantOut  int
	nonCompOut    int
	waived        int
	repoCounts    map[string]int
	authorCounts  map[string]int
}

func (b *xlsxBuilder) writeByRule(details []DetailRow) error {
	sheet := SheetByRule
	f := b.f

	rules := []struct {
		id, name string
		fires    func(DetailRow, RuleOutcomes) (fire, waived bool)
	}{
		{"R1 Exempt", "Exempt author waiver", func(_ DetailRow, o RuleOutcomes) (bool, bool) { return o.R1Exempt == OutcomeWaived, o.R1Exempt == OutcomeWaived }},
		{"R2 Empty", "Empty-commit waiver", func(_ DetailRow, o RuleOutcomes) (bool, bool) { return o.R2Empty == OutcomeWaived, o.R2Empty == OutcomeWaived }},
		{"R3 HasPR", "Missing associated PR", func(_ DetailRow, o RuleOutcomes) (bool, bool) { return o.R3HasPR == OutcomeFail, false }},
		{"R4 FinalApproval", "No approval on final commit", func(_ DetailRow, o RuleOutcomes) (bool, bool) { return o.R4FinalApproval == OutcomeFail, false }},
		{"R4b Stale", "Stale approval (pre-force-push)", func(_ DetailRow, o RuleOutcomes) (bool, bool) { return o.R4bStale == OutcomeFail, false }},
		{"R4c PostMergeConcern", "Post-merge reviewer concern", func(_ DetailRow, o RuleOutcomes) (bool, bool) { return o.R4cPostMergeConcern == OutcomeFail, false }},
		{"R5 SelfApproval", "Only approver is a code contributor", func(_ DetailRow, o RuleOutcomes) (bool, bool) { return o.R5SelfApproval == OutcomeFail, false }},
		{"R6 OwnerCheck", "Required status check failing/missing", func(_ DetailRow, o RuleOutcomes) (bool, bool) { return o.R6OwnerCheck == OutcomeFail || o.R6OwnerCheck == OutcomeMissing, false }},
		{"R7 Verdict", "Overall non-compliant verdict", func(_ DetailRow, o RuleOutcomes) (bool, bool) { return o.R7Verdict == OutcomeFail, false }},
		{"R8 RevertWaiver", "Clean-revert waiver", func(_ DetailRow, o RuleOutcomes) (bool, bool) { return o.R8RevertWaiver == OutcomeWaived, o.R8RevertWaiver == OutcomeWaived }},
	}

	aggs := make([]*byRuleAgg, len(rules))
	for i, r := range rules {
		aggs[i] = &byRuleAgg{rule: r.id, name: r.name, repoCounts: map[string]int{}, authorCounts: map[string]int{}}
	}

	for _, d := range details {
		o := DeriveRuleOutcomes(d)
		for i, r := range rules {
			fire, waived := r.fires(d, o)
			if !fire {
				continue
			}
			a := aggs[i]
			a.fires++
			if waived {
				a.waived++
			}
			if d.IsCompliant {
				a.compliantOut++
			} else {
				a.nonCompOut++
			}
			a.repoCounts[d.Org+"/"+d.Repo]++
			a.authorCounts[d.AuthorLogin]++
		}
	}

	headers := []string{"Rule", "Description", "Fires", "Compliant Outcomes", "Non-Compliant Outcomes", "Waived", "Top Repo", "Top Author"}
	b.writeHeaderRow(sheet, headers, b.headerBlue)

	for i, a := range aggs {
		row := i + 2
		f.SetCellValue(sheet, cellName(1, row), a.rule)
		f.SetCellValue(sheet, cellName(2, row), a.name)
		f.SetCellValue(sheet, cellName(3, row), a.fires)
		f.SetCellValue(sheet, cellName(4, row), a.compliantOut)
		f.SetCellValue(sheet, cellName(5, row), a.nonCompOut)
		f.SetCellValue(sheet, cellName(6, row), a.waived)
		f.SetCellValue(sheet, cellName(7, row), topKey(a.repoCounts))
		f.SetCellValue(sheet, cellName(8, row), topKey(a.authorCounts))
	}

	b.finalizeSheet(sheet, headers, len(aggs))
	widths := []float64{22, 38, 8, 18, 22, 10, 28, 22}
	applyWidths(f, sheet, widths)
	return nil
}

func topKey(m map[string]int) string {
	best, bestN := "", 0
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf("%s (%d)", best, bestN)
}

// --- By Author ------------------------------------------------------------

func (b *xlsxBuilder) writeByAuthor(rows []ByAuthorRow) error {
	sheet := SheetByAuthor
	f := b.f

	headers := []string{"Author", "Commits", "Non-Compliant", "Self-Approved", "Stale", "Post-Merge", "Compliance %"}
	b.writeHeaderRow(sheet, headers, b.headerBlue)

	pctFmt := "0.0"
	pctStyle, _ := f.NewStyle(&excelize.Style{CustomNumFmt: &pctFmt})

	for i, r := range rows {
		row := i + 2
		f.SetCellValue(sheet, cellName(1, row), sanitizeCell(r.Author))
		f.SetCellValue(sheet, cellName(2, row), r.Commits)
		f.SetCellValue(sheet, cellName(3, row), r.NonCompliant)
		f.SetCellValue(sheet, cellName(4, row), r.SelfApproved)
		f.SetCellValue(sheet, cellName(5, row), r.StaleApproval)
		f.SetCellValue(sheet, cellName(6, row), r.PostMergeConcern)
		pctCell := cellName(7, row)
		f.SetCellValue(sheet, pctCell, r.CompliancePct)
		f.SetCellStyle(sheet, pctCell, pctCell, pctStyle)
	}

	b.finalizeSheet(sheet, headers, len(rows))
	widths := []float64{22, 10, 14, 14, 10, 12, 13}
	applyWidths(f, sheet, widths)
	return nil
}

// --- Decision Matrix ------------------------------------------------------

func (b *xlsxBuilder) writeDecisionMatrix(details []DetailRow) error {
	sheet := SheetDecisionMatrix
	f := b.f

	headers := []string{
		// Identity
		"Repo", "SHA", "PR #", "PR Count", "Author", "Merged By", "Date", "Branch", "Merge Strategy",
		// Rules
		"R1 Exempt", "R2 Empty", "R3 HasPR", "R4 FinalApproval", "R4b Stale", "R4c PostMerge", "R5 SelfApproval", "R6 OwnerCheck", "R7 Verdict", "R8 RevertWaiver",
		// Evidence
		"Approvers", "Reverted SHA", "Revert Verification", "Merge Verification", "Annotations", "Reasons",
		// Action
		"Severity", "Action",
	}
	b.writeHeaderRow(sheet, headers, b.headerBlue)

	for i, d := range details {
		row := i + 2
		o := DeriveRuleOutcomes(d)
		sev, _, action := SynthesizeAction(d, o)

		f.SetCellValue(sheet, cellName(1, row), d.Org+"/"+d.Repo)
		b.writeSHACell(sheet, cellName(2, row), d.SHA, d.CommitHref)
		b.writePRCell(sheet, cellName(3, row), d.PRNumber, d.PRHref)
		f.SetCellValue(sheet, cellName(4, row), d.PRCount)
		f.SetCellValue(sheet, cellName(5, row), sanitizeCell(d.AuthorLogin))
		f.SetCellValue(sheet, cellName(6, row), sanitizeCell(d.MergedByLogin))
		f.SetCellValue(sheet, cellName(7, row), d.CommittedAt.Format("2006-01-02 15:04"))
		f.SetCellValue(sheet, cellName(8, row), sanitizeCell(d.BranchName))
		f.SetCellValue(sheet, cellName(9, row), d.MergeStrategy)

		// Rule columns with conditional styling.
		outcomes := []RuleOutcome{
			o.R1Exempt, o.R2Empty, o.R3HasPR, o.R4FinalApproval, o.R4bStale, o.R4cPostMergeConcern,
			o.R5SelfApproval, o.R6OwnerCheck, o.R7Verdict, o.R8RevertWaiver,
		}
		for j, oc := range outcomes {
			cell := cellName(10+j, row)
			f.SetCellValue(sheet, cell, string(oc))
			if style, ok := b.outcomeStyles[oc]; ok {
				f.SetCellStyle(sheet, cell, cell, style)
			}
		}

		f.SetCellValue(sheet, cellName(20, row), sanitizeCell(d.ApproverLogins))
		if d.RevertedSHA != "" {
			revShort := d.RevertedSHA
			if len(revShort) > 8 {
				revShort = revShort[:8]
			}
			href := fmt.Sprintf("https://github.com/%s/%s/commit/%s", d.Org, d.Repo, d.RevertedSHA)
			cell := cellName(21, row)
			f.SetCellFormula(sheet, cell, fmt.Sprintf(`HYPERLINK("%s","%s")`, escapeFormulaURL(href), revShort))
			f.SetCellStyle(sheet, cell, cell, b.linkStyle)
		}
		f.SetCellValue(sheet, cellName(22, row), d.RevertVerification)
		f.SetCellValue(sheet, cellName(23, row), d.MergeVerification)
		f.SetCellValue(sheet, cellName(24, row), sanitizeCell(d.Annotations))
		f.SetCellValue(sheet, cellName(25, row), sanitizeCell(d.Reasons))
		f.SetCellValue(sheet, cellName(26, row), string(sev))
		f.SetCellValue(sheet, cellName(27, row), sanitizeCell(action))
	}

	lastCell := cellName(len(headers), max(len(details)+1, 1))
	f.AutoFilter(sheet, "A1:"+lastCell, nil)
	// Freeze header row AND first 3 columns so rule columns scroll
	// horizontally against fixed Repo/SHA/PR identity.
	f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, XSplit: 3, YSplit: 1,
		TopLeftCell: "D2", ActivePane: "bottomRight",
	})
	widths := []float64{22, 12, 8, 8, 16, 16, 18, 18, 14, 10, 10, 10, 16, 10, 14, 14, 14, 10, 14, 24, 14, 18, 18, 22, 40, 10, 40}
	applyWidths(f, sheet, widths)
	return nil
}

// --- Waivers Log ----------------------------------------------------------

func (b *xlsxBuilder) writeWaiversLog(details []DetailRow) error {
	sheet := SheetWaiversLog
	f := b.f

	headers := []string{"Repo", "SHA", "Waiver Type", "Evidence", "Author", "Date", "PR #", "Message"}
	b.writeHeaderRow(sheet, headers, b.headerGrey)

	row := 2
	for _, d := range details {
		// A single commit may carry several waiver tags — emit one row per.
		emit := func(kind, evidence string) {
			f.SetCellValue(sheet, cellName(1, row), d.Org+"/"+d.Repo)
			b.writeSHACell(sheet, cellName(2, row), d.SHA, d.CommitHref)
			f.SetCellValue(sheet, cellName(3, row), kind)
			f.SetCellValue(sheet, cellName(4, row), sanitizeCell(evidence))
			f.SetCellValue(sheet, cellName(5, row), sanitizeCell(d.AuthorLogin))
			f.SetCellValue(sheet, cellName(6, row), d.CommittedAt.Format("2006-01-02 15:04"))
			b.writePRCell(sheet, cellName(7, row), d.PRNumber, d.PRHref)
			f.SetCellValue(sheet, cellName(8, row), sanitizeCell(truncate(d.Message, 80)))
			row++
		}

		if d.IsExemptAuthor {
			emit("exempt-author", "author "+d.AuthorLogin+" in exemptions config")
		}
		if d.IsEmptyCommit {
			emit("empty-commit", "0 additions / 0 deletions")
		}
		if d.IsCleanRevert {
			ev := "clean revert"
			if d.RevertedSHA != "" {
				ev = fmt.Sprintf("reverts %s (%s)", shortSHA(d.RevertedSHA), d.RevertVerification)
			}
			emit("clean-revert", ev)
		}
		if d.IsCleanMerge {
			emit("clean-merge", "2-parent web-flow merge, verified signature")
		}
		if d.IsBot && !d.IsExemptAuthor {
			emit("bot", "author login ends in [bot]")
		}
	}

	dataRows := row - 2
	b.finalizeSheet(sheet, headers, dataRows)
	widths := []float64{22, 12, 16, 48, 16, 18, 8, 50}
	applyWidths(f, sheet, widths)
	return nil
}

// --- Multiple PRs ---------------------------------------------------------

func (b *xlsxBuilder) writeMultiplePRs(rows []MultiplePRRow) error {
	sheet := SheetMultiplePRs
	f := b.f

	headers := []string{"Repo", "SHA", "Commit Author", "Date", "PR Count", "PR #", "PR Title", "PR Author", "Merged By", "Audited PR?"}
	b.writeHeaderRow(sheet, headers, b.headerBlue)

	for i, m := range rows {
		r := i + 2
		f.SetCellValue(sheet, cellName(1, r), m.Org+"/"+m.Repo)
		b.writeSHACell(sheet, cellName(2, r), m.SHA, m.CommitHref)
		f.SetCellValue(sheet, cellName(3, r), sanitizeCell(m.AuthorLogin))
		f.SetCellValue(sheet, cellName(4, r), m.CommittedAt.Format("2006-01-02 15:04"))
		f.SetCellValue(sheet, cellName(5, r), m.PRCount)
		b.writePRCell(sheet, cellName(6, r), m.PRNumber, m.PRHref)
		f.SetCellValue(sheet, cellName(7, r), sanitizeCell(truncate(m.PRTitle, 60)))
		f.SetCellValue(sheet, cellName(8, r), sanitizeCell(m.PRAuthorLogin))
		f.SetCellValue(sheet, cellName(9, r), sanitizeCell(m.PRMergedBy))
		f.SetCellValue(sheet, cellName(10, r), boolToYesNo(m.IsAuditedPR))
	}

	b.finalizeSheet(sheet, headers, len(rows))
	widths := []float64{22, 12, 18, 18, 10, 8, 40, 16, 16, 12}
	applyWidths(f, sheet, widths)
	return nil
}

// --- Shared helpers -------------------------------------------------------

func (b *xlsxBuilder) writeHeaderRow(sheet string, headers []string, style int) {
	for col, h := range headers {
		cell := cellName(col+1, 1)
		b.f.SetCellValue(sheet, cell, h)
		b.f.SetCellStyle(sheet, cell, cell, style)
	}
}

func (b *xlsxBuilder) finalizeSheet(sheet string, headers []string, dataRows int) {
	lastCell := cellName(len(headers), max(dataRows+1, 1))
	b.f.AutoFilter(sheet, "A1:"+lastCell, nil)
	b.f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})
}

func (b *xlsxBuilder) writeSHACell(sheet, cell, sha, href string) {
	display := shortSHA(sha)
	if href == "" {
		b.f.SetCellValue(sheet, cell, display)
		return
	}
	b.f.SetCellFormula(sheet, cell, fmt.Sprintf(`HYPERLINK("%s","%s")`, escapeFormulaURL(href), display))
	b.f.SetCellStyle(sheet, cell, cell, b.linkStyle)
}

func (b *xlsxBuilder) writePRCell(sheet, cell string, number int, href string) {
	if number <= 0 {
		return
	}
	display := fmt.Sprintf("#%d", number)
	if href == "" {
		b.f.SetCellValue(sheet, cell, display)
		return
	}
	b.f.SetCellFormula(sheet, cell, fmt.Sprintf(`HYPERLINK("%s","%s")`, escapeFormulaURL(href), display))
	b.f.SetCellStyle(sheet, cell, cell, b.linkStyle)
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func applyWidths(f *excelize.File, sheet string, widths []float64) {
	for i, w := range widths {
		colName, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, colName, colName, w)
	}
}

func cellName(col, row int) string {
	name, _ := excelize.CoordinatesToCellName(col, row)
	return name
}

func boolToYesNo(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

// escapeFormulaURL escapes double quotes in a URL for use inside HYPERLINK formulas.
func escapeFormulaURL(url string) string {
	return strings.ReplaceAll(url, `"`, `""`)
}

// sanitizeCell prevents formula injection by prefixing dangerous values with
// a single quote, which forces Excel to treat the cell as text.
func sanitizeCell(s string) string {
	if len(s) > 0 && (s[0] == '=' || s[0] == '+' || s[0] == '-' || s[0] == '@') {
		return "'" + s
	}
	return s
}
