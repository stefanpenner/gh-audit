package report

import (
	"context"
	"fmt"

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
	for _, d := range details {
		if d.IsBot || d.IsEmptyCommit {
			exemptions = append(exemptions, d)
		}
	}

	f := excelize.NewFile()
	defer f.Close()

	// --- Sheet 1: Summary ---
	summarySheet := "Summary"
	f.SetSheetName("Sheet1", summarySheet)

	if err := writeSummarySheet(f, summarySheet, summary); err != nil {
		return fmt.Errorf("writing summary sheet: %w", err)
	}

	// --- Sheet 2: All Commits ---
	allSheet := "All Commits"
	if _, err := f.NewSheet(allSheet); err != nil {
		return fmt.Errorf("creating all commits sheet: %w", err)
	}
	if err := writeDetailSheet(f, allSheet, details, false); err != nil {
		return fmt.Errorf("writing all commits sheet: %w", err)
	}

	// --- Sheet 3: Non-Compliant ---
	ncSheet := "Non-Compliant"
	if _, err := f.NewSheet(ncSheet); err != nil {
		return fmt.Errorf("creating non-compliant sheet: %w", err)
	}
	if err := writeDetailSheet(f, ncSheet, nonCompliant, true); err != nil {
		return fmt.Errorf("writing non-compliant sheet: %w", err)
	}

	// --- Sheet 4: Exemptions ---
	exSheet := "Exemptions"
	if _, err := f.NewSheet(exSheet); err != nil {
		return fmt.Errorf("creating exemptions sheet: %w", err)
	}
	if err := writeDetailSheet(f, exSheet, exemptions, false); err != nil {
		return fmt.Errorf("writing exemptions sheet: %w", err)
	}

	return f.SaveAs(outputPath)
}

func writeSummarySheet(f *excelize.File, sheet string, summary []SummaryRow) error {
	headers := []string{"Org", "Repo", "Total Commits", "Compliant", "Non-Compliant", "Bots", "Empty", "Compliance %"}

	// Header style
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
		cell, _ := excelize.CoordinatesToCellName(col+1, 1)
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

	// Write data rows
	for i, s := range summary {
		row := i + 2
		f.SetCellValue(sheet, cellName(1, row), s.Org)
		f.SetCellValue(sheet, cellName(2, row), s.Repo)
		f.SetCellValue(sheet, cellName(3, row), s.TotalCommits)
		f.SetCellValue(sheet, cellName(4, row), s.CompliantCount)
		f.SetCellValue(sheet, cellName(5, row), s.NonCompliantCount)
		f.SetCellValue(sheet, cellName(6, row), s.BotCount)
		f.SetCellValue(sheet, cellName(7, row), s.EmptyCount)

		pctCell := cellName(8, row)
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

	// Totals row
	if len(summary) > 0 {
		totalsRow := len(summary) + 2
		totalStyle, _ := f.NewStyle(&excelize.Style{
			Font: &excelize.Font{Bold: true},
		})

		f.SetCellValue(sheet, cellName(1, totalsRow), "TOTAL")
		var totalCommits, totalCompliant, totalNonCompliant, totalBots, totalEmpty int
		for _, s := range summary {
			totalCommits += s.TotalCommits
			totalCompliant += s.CompliantCount
			totalNonCompliant += s.NonCompliantCount
			totalBots += s.BotCount
			totalEmpty += s.EmptyCount
		}
		f.SetCellValue(sheet, cellName(3, totalsRow), totalCommits)
		f.SetCellValue(sheet, cellName(4, totalsRow), totalCompliant)
		f.SetCellValue(sheet, cellName(5, totalsRow), totalNonCompliant)
		f.SetCellValue(sheet, cellName(6, totalsRow), totalBots)
		f.SetCellValue(sheet, cellName(7, totalsRow), totalEmpty)

		pct := 0.0
		if totalCommits > 0 {
			pct = float64(totalCompliant) / float64(totalCommits) * 100.0
		}
		f.SetCellValue(sheet, cellName(8, totalsRow), pct)

		for col := 1; col <= 8; col++ {
			c := cellName(col, totalsRow)
			f.SetCellStyle(sheet, c, c, totalStyle)
		}
	}

	// Column widths
	widths := []float64{15, 30, 15, 12, 15, 10, 10, 15}
	for i, w := range widths {
		colName, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, colName, colName, w)
	}

	return nil
}

func writeDetailSheet(f *excelize.File, sheet string, details []DetailRow, redHeader bool) error {
	headers := []string{
		"Org", "Repo", "SHA", "Author", "Date", "Message",
		"PR #", "PR Link", "Approved?", "Approver", "Owner Approval",
		"Compliant?", "Reasons", "Commit Link",
	}

	// Use StreamWriter for large datasets
	sw, err := f.NewStreamWriter(sheet)
	if err != nil {
		return fmt.Errorf("creating stream writer: %w", err)
	}

	// Header style
	headerColor := "4472C4"
	fontColor := "FFFFFF"
	if redHeader {
		headerColor = "C00000"
	}
	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: fontColor},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{headerColor}},
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

		rowData := []any{
			d.Org,
			d.Repo,
			d.SHA,
			d.AuthorLogin,
			dateStr,
			msg,
			d.PRNumber,
			d.PRHref,
			approvedStr,
			d.ApproverLogins,
			d.OwnerApprovalCheck,
			compliantStr,
			d.Reasons,
			d.CommitHref,
		}
		if err := sw.SetRow(cellRef, rowData); err != nil {
			return err
		}
	}

	if err := sw.Flush(); err != nil {
		return err
	}

	// Auto-filter (must be done after flush since StreamWriter doesn't support it)
	if len(details) > 0 {
		lastCell := cellName(len(headers), len(details)+1)
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

	// Column widths
	widths := []float64{12, 25, 12, 15, 18, 40, 8, 35, 10, 20, 15, 10, 40, 40}
	for i, w := range widths {
		colName, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheet, colName, colName, w)
	}

	return nil
}

func cellName(col, row int) string {
	name, _ := excelize.CoordinatesToCellName(col, row)
	return name
}
