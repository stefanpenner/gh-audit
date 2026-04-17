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

	var exemptions []DetailRow
	var selfApproved []DetailRow
	for _, d := range details {
		if d.IsExemptAuthor || d.IsBot || d.IsEmptyCommit {
			exemptions = append(exemptions, d)
		}
		if d.IsSelfApproved {
			selfApproved = append(selfApproved, d)
		}
	}

	f := excelize.NewFile()
	defer f.Close()

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
	if err := writeAllCommitsSheet(f, allSheet, details); err != nil {
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
	if err := writeNonCompliantSheet(f, ncSheet, nonCompliant); err != nil {
		return fmt.Errorf("writing non-compliant sheet: %w", err)
	}

	// --- Sheet 4: Exemptions (normal writer for hyperlinks) ---
	exSheet := "Exemptions"
	if _, err := f.NewSheet(exSheet); err != nil {
		return fmt.Errorf("creating exemptions sheet: %w", err)
	}
	if err := writeExemptionsSheet(f, exSheet, exemptions); err != nil {
		return fmt.Errorf("writing exemptions sheet: %w", err)
	}

	// --- Sheet 5: Self-Approved (normal writer for hyperlinks) ---
	saSheet := "Self-Approved"
	if _, err := f.NewSheet(saSheet); err != nil {
		return fmt.Errorf("creating self-approved sheet: %w", err)
	}
	if err := writeSelfApprovedSheet(f, saSheet, selfApproved); err != nil {
		return fmt.Errorf("writing self-approved sheet: %w", err)
	}

	return f.SaveAs(outputPath)
}

func writeSummarySheet(f *excelize.File, sheet string, summary []SummaryRow, opts ReportOpts) error {
	headers := []string{"Org", "Repo", "Total Commits", "Compliant", "Non-Compliant", "Bot Exempt", "Empty", "Self-Approved", "Compliance %"}

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
	greenStyle, _ := f.NewStyle(&excelize.Style{
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"C6EFCE"}},
		Font: &excelize.Font{Color: "006100"},
	})
	yellowStyle, _ := f.NewStyle(&excelize.Style{
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFEB9C"}},
		Font: &excelize.Font{Color: "9C5700"},
	})
	redStyle, _ := f.NewStyle(&excelize.Style{
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFC7CE"}},
		Font: &excelize.Font{Color: "9C0006"},
	})

	// Write data rows (starting at row 3)
	for i, s := range summary {
		row := i + 3
		f.SetCellValue(sheet, cellName(1, row), s.Org)
		f.SetCellValue(sheet, cellName(2, row), s.Repo)
		f.SetCellValue(sheet, cellName(3, row), s.TotalCommits)
		f.SetCellValue(sheet, cellName(4, row), s.CompliantCount)
		f.SetCellValue(sheet, cellName(5, row), s.NonCompliantCount)
		f.SetCellValue(sheet, cellName(6, row), s.BotCount)
		f.SetCellValue(sheet, cellName(7, row), s.EmptyCount)
		f.SetCellValue(sheet, cellName(8, row), s.SelfApprovedCount)

		pctCell := cellName(9, row)
		f.SetCellValue(sheet, pctCell, s.CompliancePct)

		// Apply conditional formatting on compliance %
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

		// SUM formulas for numeric columns (cols 3-8)
		for _, col := range []int{3, 4, 5, 6, 7, 8} {
			colLetter, _ := excelize.ColumnNumberToName(col)
			formula := fmt.Sprintf("SUM(%s%d:%s%d)", colLetter, 3, colLetter, totalsRow-1)
			f.SetCellFormula(sheet, cellName(col, totalsRow), formula)
		}

		// Compliance % as formula: Compliant / Total * 100
		colD, _ := excelize.ColumnNumberToName(4) // Compliant
		colC, _ := excelize.ColumnNumberToName(3) // Total
		pctFormula := fmt.Sprintf("IF(%s%d>0,%s%d/%s%d*100,0)", colC, totalsRow, colD, totalsRow, colC, totalsRow)
		f.SetCellFormula(sheet, cellName(9, totalsRow), pctFormula)

		for col := 1; col <= len(headers); col++ {
			c := cellName(col, totalsRow)
			f.SetCellStyle(sheet, c, c, totalStyle)
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
	widths := []float64{15, 30, 15, 12, 15, 12, 10, 15, 15}
	for i, w := range widths {
		colName, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, colName, colName, w)
	}

	return nil
}

// detailHeaders returns the standard column headers for detail sheets.
func detailHeaders() []string {
	return []string{
		"Org", "Repo", "SHA", "Author", "Committer", "Date", "Message",
		"Branch", "PR #", "Merged By", "Approved?", "Approver", "Self-Approved?",
		"Owner Approval", "Compliant?", "Reasons", "Commit Link", "PR Link",
	}
}

// writeAllCommitsSheet uses StreamWriter for potentially large datasets.
func writeAllCommitsSheet(f *excelize.File, sheet string, details []DetailRow) error {
	headers := detailHeaders()

	sw, err := f.NewStreamWriter(sheet)
	if err != nil {
		return fmt.Errorf("creating stream writer: %w", err)
	}

	// Header style
	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"4472C4"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return err
	}

	// Write header row
	headerRow := make([]any, len(headers))
	for i, h := range headers {
		headerRow[i] = excelize.Cell{StyleID: headerStyle, Value: h}
	}
	if err := sw.SetRow("A1", headerRow); err != nil {
		return err
	}

	// Write data rows
	for i, d := range details {
		row := i + 2
		cellRef, _ := excelize.CoordinatesToCellName(1, row)

		rowData := detailRowData(d)
		if err := sw.SetRow(cellRef, rowData); err != nil {
			return err
		}
	}

	if err := sw.Flush(); err != nil {
		return err
	}

	// Auto-filter on all columns (must be after flush)
	if len(details) > 0 {
		lastCell := cellName(len(headers), len(details)+1)
		f.AutoFilter(sheet, "A1:"+lastCell, nil)
	} else {
		// Empty data: still add auto-filter on header
		lastCell := cellName(len(headers), 1)
		f.AutoFilter(sheet, "A1:"+lastCell, nil)
	}

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
func writeNonCompliantSheet(f *excelize.File, sheet string, details []DetailRow) error {
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
		writeDetailRowWithHyperlinks(f, sheet, row, d)
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
func writeExemptionsSheet(f *excelize.File, sheet string, details []DetailRow) error {
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
		writeDetailRowWithHyperlinks(f, sheet, row, d)

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
func writeSelfApprovedSheet(f *excelize.File, sheet string, details []DetailRow) error {
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

		// SHA with hyperlink
		shaCell := cellName(3, row)
		shaDisplay := d.SHA
		if len(shaDisplay) > 8 {
			shaDisplay = shaDisplay[:8]
		}
		f.SetCellValue(sheet, shaCell, shaDisplay)
		if d.CommitHref != "" {
			f.SetCellHyperLink(sheet, shaCell, d.CommitHref, "External")
		}

		f.SetCellValue(sheet, cellName(1, row), d.Org)
		f.SetCellValue(sheet, cellName(2, row), d.Repo)
		f.SetCellValue(sheet, cellName(4, row), d.AuthorLogin)
		f.SetCellValue(sheet, cellName(5, row), d.ApproverLogins)
		f.SetCellValue(sheet, cellName(6, row), d.CommittedAt.Format("2006-01-02 15:04"))

		// PR # with hyperlink
		prCell := cellName(7, row)
		if d.PRNumber > 0 {
			f.SetCellValue(sheet, prCell, d.PRNumber)
			if d.PRHref != "" {
				f.SetCellHyperLink(sheet, prCell, d.PRHref, "External")
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

// detailRowData converts a DetailRow into a flat slice for StreamWriter.
func detailRowData(d DetailRow) []any {
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

	return []any{
		d.Org,
		d.Repo,
		shaDisplay,
		d.AuthorLogin,
		d.CommitterLogin,
		dateStr,
		msg,
		d.BranchName,
		d.PRNumber,
		d.MergedByLogin,
		approvedStr,
		d.ApproverLogins,
		selfApprovedStr,
		d.OwnerApprovalCheck,
		compliantStr,
		d.Reasons,
		d.CommitHref,
		d.PRHref,
	}
}

// writeDetailRowWithHyperlinks writes a single detail row using the normal (non-streaming)
// writer, enabling hyperlinks on SHA and PR columns.
func writeDetailRowWithHyperlinks(f *excelize.File, sheet string, row int, d DetailRow) {
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

	// Org, Repo
	f.SetCellValue(sheet, cellName(1, row), d.Org)
	f.SetCellValue(sheet, cellName(2, row), d.Repo)

	// SHA with hyperlink
	shaCell := cellName(3, row)
	f.SetCellValue(sheet, shaCell, shaDisplay)
	if d.CommitHref != "" {
		f.SetCellHyperLink(sheet, shaCell, d.CommitHref, "External")
	}

	f.SetCellValue(sheet, cellName(4, row), d.AuthorLogin)
	f.SetCellValue(sheet, cellName(5, row), d.CommitterLogin)
	f.SetCellValue(sheet, cellName(6, row), dateStr)
	f.SetCellValue(sheet, cellName(7, row), msg)
	f.SetCellValue(sheet, cellName(8, row), d.BranchName)

	// PR # with hyperlink
	prCell := cellName(9, row)
	if d.PRNumber > 0 {
		f.SetCellValue(sheet, prCell, d.PRNumber)
		if d.PRHref != "" {
			f.SetCellHyperLink(sheet, prCell, d.PRHref, "External")
		}
	}

	f.SetCellValue(sheet, cellName(10, row), d.MergedByLogin)
	f.SetCellValue(sheet, cellName(11, row), approvedStr)
	f.SetCellValue(sheet, cellName(12, row), d.ApproverLogins)
	f.SetCellValue(sheet, cellName(13, row), selfApprovedStr)
	f.SetCellValue(sheet, cellName(14, row), d.OwnerApprovalCheck)
	f.SetCellValue(sheet, cellName(15, row), compliantStr)
	f.SetCellValue(sheet, cellName(16, row), d.Reasons)
	f.SetCellValue(sheet, cellName(17, row), d.CommitHref)
	f.SetCellValue(sheet, cellName(18, row), d.PRHref)
}

func setDetailColumnWidths(f *excelize.File, sheet string, numCols int) {
	widths := []float64{12, 25, 12, 15, 15, 18, 40, 20, 8, 15, 10, 20, 14, 15, 10, 40, 40, 40}
	for i := 0; i < numCols && i < len(widths); i++ {
		colName, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, colName, colName, widths[i])
	}
}

func cellName(col, row int) string {
	name, _ := excelize.CoordinatesToCellName(col, row)
	return name
}
