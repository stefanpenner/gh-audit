package report

import (
	"context"
	"fmt"
	"sort"

	"github.com/xuri/excelize/v2"
)

// GenerateXLSX creates a multi-sheet XLSX workbook with audit report data.
func (r *Reporter) GenerateXLSX(ctx context.Context, opts ReportOpts, outputPath string) error {
	summary, err := r.GetSummary(ctx, opts)
	if err != nil {
		return fmt.Errorf("getting summary: %w", err)
	}

	details, err := r.GetDetails(ctx, opts)
	if err != nil {
		return fmt.Errorf("getting details: %w", err)
	}

	// Get non-compliant and exemptions
	failureOpts := opts
	failureOpts.OnlyFailures = true
	nonCompliant, err := r.GetDetails(ctx, failureOpts)
	if err != nil {
		return fmt.Errorf("getting non-compliant: %w", err)
	}

	multiplePRs, err := r.GetMultiplePRDetails(ctx, opts)
	if err != nil {
		return fmt.Errorf("getting multiple PR details: %w", err)
	}

	var exemptions []DetailRow
	var selfApproved []DetailRow
	var staleApprovals []DetailRow
	for _, d := range details {
		if d.IsExemptAuthor || d.IsBot || d.IsEmptyCommit {
			exemptions = append(exemptions, d)
		}
		if d.IsSelfApproved {
			selfApproved = append(selfApproved, d)
		}
		if d.HasStaleApproval {
			staleApprovals = append(staleApprovals, d)
		}
	}

	f := excelize.NewFile()
	defer f.Close()

	linkStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "0563C1", Underline: "single"},
	})

	// --- Sheet 1: Summary ---
	summarySheet := "Summary"
	f.SetSheetName("Sheet1", summarySheet)

	if err := writeSummarySheet(f, summarySheet, summary, opts); err != nil {
		return fmt.Errorf("writing summary sheet: %w", err)
	}

	// --- Sheet 2: All Commits (StreamWriter for large datasets) ---
	allSheet := "All Commits"
	if _, err := f.NewSheet(allSheet); err != nil {
		return fmt.Errorf("creating all commits sheet: %w", err)
	}
	if err := writeAllCommitsSheet(f, allSheet, details, linkStyle); err != nil {
		return fmt.Errorf("writing all commits sheet: %w", err)
	}

	// --- Sheet 3: Non-Compliant (normal writer for hyperlinks) ---
	ncSheet := "Non-Compliant"
	if _, err := f.NewSheet(ncSheet); err != nil {
		return fmt.Errorf("creating non-compliant sheet: %w", err)
	}
	// Sort non-compliant by date descending (already ordered from query, but ensure)
	sort.Slice(nonCompliant, func(i, j int) bool {
		return nonCompliant[i].CommittedAt.After(nonCompliant[j].CommittedAt)
	})
	if err := writeNonCompliantSheet(f, ncSheet, nonCompliant, linkStyle); err != nil {
		return fmt.Errorf("writing non-compliant sheet: %w", err)
	}

	// --- Sheet 4: Exemptions (normal writer for hyperlinks) ---
	exSheet := "Exemptions"
	if _, err := f.NewSheet(exSheet); err != nil {
		return fmt.Errorf("creating exemptions sheet: %w", err)
	}
	if err := writeExemptionsSheet(f, exSheet, exemptions, linkStyle); err != nil {
		return fmt.Errorf("writing exemptions sheet: %w", err)
	}

	// --- Sheet 5: Self-Approved (normal writer for hyperlinks) ---
	saSheet := "Self-Approved"
	if _, err := f.NewSheet(saSheet); err != nil {
		return fmt.Errorf("creating self-approved sheet: %w", err)
	}
	if err := writeSelfApprovedSheet(f, saSheet, selfApproved, linkStyle); err != nil {
		return fmt.Errorf("writing self-approved sheet: %w", err)
	}

	// --- Sheet 6: Stale Approvals ---
	staleSheet := "Stale Approvals"
	if _, err := f.NewSheet(staleSheet); err != nil {
		return fmt.Errorf("creating stale approvals sheet: %w", err)
	}
	if err := writeStaleApprovalsSheet(f, staleSheet, staleApprovals, linkStyle); err != nil {
		return fmt.Errorf("writing stale approvals sheet: %w", err)
	}

	// --- Sheet 7: Multiple PRs ---
	mpSheet := "Multiple PRs"
	if _, err := f.NewSheet(mpSheet); err != nil {
		return fmt.Errorf("creating multiple PRs sheet: %w", err)
	}
	if err := writeMultiplePRsSheet(f, mpSheet, multiplePRs, linkStyle); err != nil {
		return fmt.Errorf("writing multiple PRs sheet: %w", err)
	}

	return f.SaveAs(outputPath)
}

func writeSummarySheet(f *excelize.File, sheet string, summary []SummaryRow, opts ReportOpts) error {
	headers := []string{"Org", "Repo", "Total Commits", "Compliant", "Non-Compliant", "Compliance %", "Bots", "Exempt", "Empty", "Self-Approved", "Stale Approvals", "Multiple PRs"}

	// Date range subtitle in row 1
	dateRange := "Report Period: All Time"
	if !opts.Since.IsZero() || !opts.Until.IsZero() {
		since := "beginning"
		until := "present"
		if !opts.Since.IsZero() {
			since = opts.Since.Format("2006-01-02")
		}
		if !opts.Until.IsZero() {
			until = opts.Until.Format("2006-01-02")
		}
		dateRange = fmt.Sprintf("Report Period: %s to %s", since, until)
	}

	subtitleStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 12},
		Alignment: &excelize.Alignment{Horizontal: "left"},
	})
	if err != nil {
		return err
	}

	f.SetCellValue(sheet, "A1", dateRange)
	f.SetCellStyle(sheet, "A1", "A1", subtitleStyle)

	// Header style
	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"4472C4"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return err
	}

	// Write headers in row 2
	headerRow := 2
	for col, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(col+1, headerRow)
		f.SetCellValue(sheet, cell, h)
		f.SetCellStyle(sheet, cell, cell, headerStyle)
	}

	// Conditional formatting styles
	pctFmt := "0.0"
	greenStyle, _ := f.NewStyle(&excelize.Style{
		Fill:         excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"C6EFCE"}},
		Font:         &excelize.Font{Color: "006100"},
		CustomNumFmt: &pctFmt,
	})
	yellowStyle, _ := f.NewStyle(&excelize.Style{
		Fill:         excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFEB9C"}},
		Font:         &excelize.Font{Color: "9C5700"},
		CustomNumFmt: &pctFmt,
	})
	redStyle, _ := f.NewStyle(&excelize.Style{
		Fill:         excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFC7CE"}},
		Font:         &excelize.Font{Color: "9C0006"},
		CustomNumFmt: &pctFmt,
	})

	// Write data rows (starting at row 3)
	// Columns: Org, Repo, Total, Compliant, Non-Compliant, Compliance%,
	//          then informational tags: Bots, Exempt, Empty, Self-Approved
	for i, s := range summary {
		row := i + 3
		f.SetCellValue(sheet, cellName(1, row), s.Org)
		f.SetCellValue(sheet, cellName(2, row), s.Repo)
		f.SetCellValue(sheet, cellName(3, row), s.TotalCommits)
		f.SetCellValue(sheet, cellName(4, row), s.CompliantCount)
		f.SetCellValue(sheet, cellName(5, row), s.NonCompliantCount)

		pctCell := cellName(6, row)
		f.SetCellValue(sheet, pctCell, s.CompliancePct)

		f.SetCellValue(sheet, cellName(7, row), s.BotCount)
		f.SetCellValue(sheet, cellName(8, row), s.ExemptCount)
		f.SetCellValue(sheet, cellName(9, row), s.EmptyCount)
		f.SetCellValue(sheet, cellName(10, row), s.SelfApprovedCount)
		f.SetCellValue(sheet, cellName(11, row), s.StaleApprovalCount)
		f.SetCellValue(sheet, cellName(12, row), s.MultiplePRCount)

		switch {
		case s.CompliancePct >= 100:
			f.SetCellStyle(sheet, pctCell, pctCell, greenStyle)
		case s.CompliancePct >= 90:
			f.SetCellStyle(sheet, pctCell, pctCell, yellowStyle)
		default:
			f.SetCellStyle(sheet, pctCell, pctCell, redStyle)
		}
	}

	// Totals row with SUM formulas
	if len(summary) > 0 {
		totalsRow := len(summary) + 3
		totalStyle, _ := f.NewStyle(&excelize.Style{
			Font: &excelize.Font{Bold: true},
		})

		f.SetCellValue(sheet, cellName(1, totalsRow), "TOTAL")

		// SUM formulas for count columns (3-5 = Total/Compliant/Non-Compliant, 7-12 = tags)
		for _, col := range []int{3, 4, 5, 7, 8, 9, 10, 11, 12} {
			colLetter, _ := excelize.ColumnNumberToName(col)
			formula := fmt.Sprintf("SUM(%s%d:%s%d)", colLetter, 3, colLetter, totalsRow-1)
			f.SetCellFormula(sheet, cellName(col, totalsRow), formula)
		}

		// Compliance % in column 6: Compliant / Total * 100
		colD, _ := excelize.ColumnNumberToName(4) // Compliant
		colC, _ := excelize.ColumnNumberToName(3) // Total
		pctFormula := fmt.Sprintf("IF(%s%d>0,ROUND(%s%d/%s%d*100,1),0)", colC, totalsRow, colD, totalsRow, colC, totalsRow)
		f.SetCellFormula(sheet, cellName(6, totalsRow), pctFormula)

		totalPctStyle, _ := f.NewStyle(&excelize.Style{
			Font:         &excelize.Font{Bold: true},
			CustomNumFmt: &pctFmt,
		})
		for col := 1; col <= len(headers); col++ {
			c := cellName(col, totalsRow)
			if col == 6 {
				f.SetCellStyle(sheet, c, c, totalPctStyle)
			} else {
				f.SetCellStyle(sheet, c, c, totalStyle)
			}
		}
	}

	// Freeze header rows (row 1 is subtitle, row 2 is header)
	f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      2,
		TopLeftCell: "A3",
		ActivePane:  "bottomLeft",
	})

	// Column widths
	widths := []float64{15, 30, 15, 12, 15, 15, 10, 10, 10, 15, 16, 14}
	for i, w := range widths {
		colName, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, colName, colName, w)
	}

	return nil
}

// detailHeaders returns the standard column headers for detail sheets.
func detailHeaders() []string {
	return []string{
		"Org", "Repo", "SHA", "PR #",
		"Author", "Committer", "Merged By", "Approver",
		"Approved?", "Self-Approved?", "Owner Approval",
		"Compliant?", "Reasons", "Merge Strategy", "PR Commit Authors",
		"Date", "Branch", "Message",
		"No PR", "Stale Approval", "Self-Approved", "No Approval",
	}
}

// writeAllCommitsSheet writes all commits with clickable hyperlinks on SHA and PR columns.
func writeAllCommitsSheet(f *excelize.File, sheet string, details []DetailRow, linkStyle int) error {
	headers := detailHeaders()

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"4472C4"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return err
	}

	// Write headers
	for col, h := range headers {
		cell := cellName(col+1, 1)
		f.SetCellValue(sheet, cell, h)
		f.SetCellStyle(sheet, cell, cell, headerStyle)
	}

	// Write data rows with hyperlinks
	for i, d := range details {
		writeDetailRowWithHyperlinks(f, sheet, i+2, d, linkStyle)
	}

	// Auto-filter
	lastCell := cellName(len(headers), max(len(details)+1, 1))
	f.AutoFilter(sheet, "A1:"+lastCell, nil)

	// Freeze header row
	f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})

	setDetailColumnWidths(f, sheet, len(headers))

	return nil
}

// writeNonCompliantSheet writes non-compliant rows with red header and a Resolution column.
func writeNonCompliantSheet(f *excelize.File, sheet string, details []DetailRow, linkStyle int) error {
	headers := append(detailHeaders(), "Resolution")

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"CC4444"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return err
	}

	// Write headers
	for col, h := range headers {
		cell := cellName(col+1, 1)
		f.SetCellValue(sheet, cell, h)
		f.SetCellStyle(sheet, cell, cell, headerStyle)
	}

	// Write data rows with hyperlinks
	for i, d := range details {
		row := i + 2
		writeDetailRowWithHyperlinks(f, sheet, row, d, linkStyle)
		// Resolution column is empty (for auditor notes)
		f.SetCellValue(sheet, cellName(len(headers), row), "")
	}

	// Auto-filter
	lastCell := cellName(len(headers), max(len(details)+1, 1))
	f.AutoFilter(sheet, "A1:"+lastCell, nil)

	// Freeze header row
	f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})

	setDetailColumnWidths(f, sheet, len(headers))

	return nil
}

// writeExemptionsSheet writes exempted rows (bots, empty commits) with green header.
func writeExemptionsSheet(f *excelize.File, sheet string, details []DetailRow, linkStyle int) error {
	headers := append(detailHeaders(), "Exemption Reason")

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"228B22"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return err
	}

	// Write headers
	for col, h := range headers {
		cell := cellName(col+1, 1)
		f.SetCellValue(sheet, cell, h)
		f.SetCellStyle(sheet, cell, cell, headerStyle)
	}

	// Write data rows
	for i, d := range details {
		row := i + 2
		writeDetailRowWithHyperlinks(f, sheet, row, d, linkStyle)

		// Exemption reason
		reason := ""
		if d.IsExemptAuthor {
			reason = "Exempt author: " + d.AuthorLogin
		} else if d.IsBot {
			reason = "Bot author: " + d.AuthorLogin
		} else if d.IsEmptyCommit {
			reason = "Empty commit"
		}
		f.SetCellValue(sheet, cellName(len(headers), row), reason)
	}

	// Auto-filter
	lastCell := cellName(len(headers), max(len(details)+1, 1))
	f.AutoFilter(sheet, "A1:"+lastCell, nil)

	// Freeze header row
	f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})

	setDetailColumnWidths(f, sheet, len(headers))

	return nil
}

// writeSelfApprovedSheet writes self-approved rows with orange/amber header.
func writeSelfApprovedSheet(f *excelize.File, sheet string, details []DetailRow, linkStyle int) error {
	headers := []string{
		"Org", "Repo", "SHA", "Author", "Reviewer", "Date", "PR #",
		"Branch", "Message",
	}

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"E67E00"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return err
	}

	// Write headers
	for col, h := range headers {
		cell := cellName(col+1, 1)
		f.SetCellValue(sheet, cell, h)
		f.SetCellStyle(sheet, cell, cell, headerStyle)
	}

	// Write data rows
	for i, d := range details {
		row := i + 2

		shaCell := cellName(3, row)
		shaDisplay := d.SHA
		if len(shaDisplay) > 8 {
			shaDisplay = shaDisplay[:8]
		}
		if d.CommitHref != "" {
			f.SetCellFormula(sheet, shaCell, fmt.Sprintf(`HYPERLINK("%s","%s")`, d.CommitHref, shaDisplay))
			f.SetCellStyle(sheet, shaCell, shaCell, linkStyle)
		} else {
			f.SetCellValue(sheet, shaCell, shaDisplay)
		}

		f.SetCellValue(sheet, cellName(1, row), d.Org)
		f.SetCellValue(sheet, cellName(2, row), d.Repo)
		f.SetCellValue(sheet, cellName(4, row), d.AuthorLogin)
		f.SetCellValue(sheet, cellName(5, row), d.ApproverLogins)
		f.SetCellValue(sheet, cellName(6, row), d.CommittedAt.Format("2006-01-02 15:04"))

		// PR # with hyperlink
		prCell := cellName(7, row)
		if d.PRNumber > 0 {
			if d.PRHref != "" {
				f.SetCellFormula(sheet, prCell, fmt.Sprintf(`HYPERLINK("%s","#%d")`, d.PRHref, d.PRNumber))
				f.SetCellStyle(sheet, prCell, prCell, linkStyle)
			} else {
				f.SetCellValue(sheet, prCell, fmt.Sprintf("#%d", d.PRNumber))
			}
		}

		f.SetCellValue(sheet, cellName(8, row), d.BranchName)
		f.SetCellValue(sheet, cellName(9, row), truncate(d.Message, 80))
	}

	// Auto-filter
	lastCell := cellName(len(headers), max(len(details)+1, 1))
	f.AutoFilter(sheet, "A1:"+lastCell, nil)

	// Freeze header row
	f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})

	widths := []float64{12, 25, 12, 15, 15, 18, 8, 20, 40}
	for i, w := range widths {
		colName, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, colName, colName, w)
	}

	return nil
}

// writeDetailRowWithHyperlinks writes a single detail row using the normal (non-streaming)
// writer, enabling hyperlinks on SHA and PR columns.
func writeDetailRowWithHyperlinks(f *excelize.File, sheet string, row int, d DetailRow, linkStyle int) {
	shaDisplay := d.SHA
	if len(shaDisplay) > 8 {
		shaDisplay = shaDisplay[:8]
	}

	msg := truncate(d.Message, 80)
	dateStr := d.CommittedAt.Format("2006-01-02 15:04")

	approvedStr := "No"
	if d.HasFinalApproval {
		approvedStr = "Yes"
	}
	compliantStr := "No"
	if d.IsCompliant {
		compliantStr = "Yes"
	}
	selfApprovedStr := "No"
	if d.IsSelfApproved {
		selfApprovedStr = "Yes"
	}

	// Location: Org, Repo, SHA, PR #
	f.SetCellValue(sheet, cellName(1, row), d.Org)
	f.SetCellValue(sheet, cellName(2, row), d.Repo)
	shaCell := cellName(3, row)
	if d.CommitHref != "" {
		f.SetCellFormula(sheet, shaCell, fmt.Sprintf(`HYPERLINK("%s","%s")`, d.CommitHref, shaDisplay))
		f.SetCellStyle(sheet, shaCell, shaCell, linkStyle)
	} else {
		f.SetCellValue(sheet, shaCell, shaDisplay)
	}
	prCell := cellName(4, row)
	if d.PRNumber > 0 {
		if d.PRHref != "" {
			f.SetCellFormula(sheet, prCell, fmt.Sprintf(`HYPERLINK("%s","#%d")`, d.PRHref, d.PRNumber))
			f.SetCellStyle(sheet, prCell, prCell, linkStyle)
		} else {
			f.SetCellValue(sheet, prCell, fmt.Sprintf("#%d", d.PRNumber))
		}
	}

	// People: Author, Committer, Merged By, Approver
	f.SetCellValue(sheet, cellName(5, row), d.AuthorLogin)
	f.SetCellValue(sheet, cellName(6, row), d.CommitterLogin)
	f.SetCellValue(sheet, cellName(7, row), d.MergedByLogin)
	f.SetCellValue(sheet, cellName(8, row), d.ApproverLogins)

	// Approval
	f.SetCellValue(sheet, cellName(9, row), approvedStr)
	f.SetCellValue(sheet, cellName(10, row), selfApprovedStr)
	f.SetCellValue(sheet, cellName(11, row), d.OwnerApprovalCheck)

	// Result
	f.SetCellValue(sheet, cellName(12, row), compliantStr)
	f.SetCellValue(sheet, cellName(13, row), d.Reasons)
	f.SetCellValue(sheet, cellName(14, row), d.MergeStrategy)
	f.SetCellValue(sheet, cellName(15, row), d.PRCommitAuthorLogins)

	// Context: Date, Branch, Message
	f.SetCellValue(sheet, cellName(16, row), dateStr)
	f.SetCellValue(sheet, cellName(17, row), d.BranchName)
	f.SetCellValue(sheet, cellName(18, row), msg)

	// Binary reason columns for filtering/sorting
	f.SetCellValue(sheet, cellName(19, row), boolToYesNo(!d.HasPR))
	f.SetCellValue(sheet, cellName(20, row), boolToYesNo(d.HasStaleApproval))
	f.SetCellValue(sheet, cellName(21, row), boolToYesNo(d.IsSelfApproved))
	f.SetCellValue(sheet, cellName(22, row), boolToYesNo(!d.HasFinalApproval && !d.IsSelfApproved))
}

// writeStaleApprovalsSheet writes commits where approval existed but was stale (pre-force-push).
func writeStaleApprovalsSheet(f *excelize.File, sheet string, details []DetailRow, linkStyle int) error {
	headers := []string{
		"Org", "Repo", "SHA", "Author", "Date", "PR #",
		"Branch", "Approvers", "Compliant?", "Reasons", "Message",
	}

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"B85C00"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return err
	}

	for col, h := range headers {
		cell := cellName(col+1, 1)
		f.SetCellValue(sheet, cell, h)
		f.SetCellStyle(sheet, cell, cell, headerStyle)
	}

	for i, d := range details {
		row := i + 2

		shaCell := cellName(3, row)
		shaDisplay := d.SHA
		if len(shaDisplay) > 8 {
			shaDisplay = shaDisplay[:8]
		}
		if d.CommitHref != "" {
			f.SetCellFormula(sheet, shaCell, fmt.Sprintf(`HYPERLINK("%s","%s")`, d.CommitHref, shaDisplay))
			f.SetCellStyle(sheet, shaCell, shaCell, linkStyle)
		} else {
			f.SetCellValue(sheet, shaCell, shaDisplay)
		}

		f.SetCellValue(sheet, cellName(1, row), d.Org)
		f.SetCellValue(sheet, cellName(2, row), d.Repo)
		f.SetCellValue(sheet, cellName(4, row), d.AuthorLogin)
		f.SetCellValue(sheet, cellName(5, row), d.CommittedAt.Format("2006-01-02 15:04"))

		prCell := cellName(6, row)
		if d.PRNumber > 0 {
			if d.PRHref != "" {
				f.SetCellFormula(sheet, prCell, fmt.Sprintf(`HYPERLINK("%s","#%d")`, d.PRHref, d.PRNumber))
				f.SetCellStyle(sheet, prCell, prCell, linkStyle)
			} else {
				f.SetCellValue(sheet, prCell, fmt.Sprintf("#%d", d.PRNumber))
			}
		}

		f.SetCellValue(sheet, cellName(7, row), d.BranchName)
		f.SetCellValue(sheet, cellName(8, row), d.ApproverLogins)
		compliantStr := "No"
		if d.IsCompliant {
			compliantStr = "Yes"
		}
		f.SetCellValue(sheet, cellName(9, row), compliantStr)
		f.SetCellValue(sheet, cellName(10, row), d.Reasons)
		f.SetCellValue(sheet, cellName(11, row), truncate(d.Message, 80))
	}

	lastCell := cellName(len(headers), max(len(details)+1, 1))
	f.AutoFilter(sheet, "A1:"+lastCell, nil)

	f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, XSplit: 0, YSplit: 1,
		TopLeftCell: "A2", ActivePane: "bottomLeft",
	})

	widths := []float64{12, 25, 12, 15, 18, 8, 20, 20, 10, 40, 40}
	for i, w := range widths {
		colName, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, colName, colName, w)
	}

	return nil
}

// writeMultiplePRsSheet writes commits that have more than one associated PR.
func writeMultiplePRsSheet(f *excelize.File, sheet string, rows []MultiplePRRow, linkStyle int) error {
	headers := []string{
		"Org", "Repo", "SHA", "Commit Author", "Date",
		"PR Count", "PR #", "PR Title", "PR Author", "Merged By", "Audited PR?",
	}

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"7030A0"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return err
	}

	for col, h := range headers {
		cell := cellName(col+1, 1)
		f.SetCellValue(sheet, cell, h)
		f.SetCellStyle(sheet, cell, cell, headerStyle)
	}

	for i, m := range rows {
		row := i + 2

		shaCell := cellName(3, row)
		shaDisplay := m.SHA
		if len(shaDisplay) > 8 {
			shaDisplay = shaDisplay[:8]
		}
		if m.CommitHref != "" {
			f.SetCellFormula(sheet, shaCell, fmt.Sprintf(`HYPERLINK("%s","%s")`, m.CommitHref, shaDisplay))
			f.SetCellStyle(sheet, shaCell, shaCell, linkStyle)
		} else {
			f.SetCellValue(sheet, shaCell, shaDisplay)
		}

		f.SetCellValue(sheet, cellName(1, row), m.Org)
		f.SetCellValue(sheet, cellName(2, row), m.Repo)
		f.SetCellValue(sheet, cellName(4, row), m.AuthorLogin)
		f.SetCellValue(sheet, cellName(5, row), m.CommittedAt.Format("2006-01-02 15:04"))
		f.SetCellValue(sheet, cellName(6, row), m.PRCount)

		prCell := cellName(7, row)
		if m.PRHref != "" {
			f.SetCellFormula(sheet, prCell, fmt.Sprintf(`HYPERLINK("%s","#%d")`, m.PRHref, m.PRNumber))
			f.SetCellStyle(sheet, prCell, prCell, linkStyle)
		} else {
			f.SetCellValue(sheet, prCell, fmt.Sprintf("#%d", m.PRNumber))
		}

		f.SetCellValue(sheet, cellName(8, row), truncate(m.PRTitle, 60))
		f.SetCellValue(sheet, cellName(9, row), m.PRAuthorLogin)
		f.SetCellValue(sheet, cellName(10, row), m.PRMergedBy)
		auditedStr := "No"
		if m.IsAuditedPR {
			auditedStr = "Yes"
		}
		f.SetCellValue(sheet, cellName(11, row), auditedStr)
	}

	lastCell := cellName(len(headers), max(len(rows)+1, 1))
	f.AutoFilter(sheet, "A1:"+lastCell, nil)

	f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, XSplit: 0, YSplit: 1,
		TopLeftCell: "A2", ActivePane: "bottomLeft",
	})

	widths := []float64{12, 25, 12, 15, 18, 10, 8, 40, 15, 15, 12}
	for i, w := range widths {
		colName, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, colName, colName, w)
	}

	return nil
}

func setDetailColumnWidths(f *excelize.File, sheet string, numCols int) {
	widths := []float64{12, 25, 12, 10, 15, 15, 15, 20, 10, 14, 15, 10, 40, 14, 25, 18, 20, 40, 8, 14, 14, 13}
	for i := 0; i < numCols && i < len(widths); i++ {
		colName, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, colName, colName, widths[i])
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
