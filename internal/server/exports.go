package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/signintech/gopdf"
	"github.com/tatoalo/degitide/internal/db"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
)

type exportedTransaction struct {
	AccountID    string  `json:"account_id"`
	AccountName  string  `json:"account_name"`
	ID           string  `json:"transaction_id"`
	Date         string  `json:"date"`
	Type         string  `json:"type"`
	ProductID    string  `json:"product_id"`
	Instrument   string  `json:"instrument"`
	Quantity     float64 `json:"quantity"`
	UnitPrice    float64 `json:"unit_price"`
	Currency     string  `json:"currency"`
	Fee          float64 `json:"fee"`
	Counterparty string  `json:"counterparty"`
}

func (s *Server) handleTransactionExport(w http.ResponseWriter, r *http.Request) {
	account, ok := s.requestAccount(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "pdf" && format != "json" {
		writeError(w, http.StatusBadRequest, errors.New("choose csv, pdf or json"))
		return
	}
	// Exports include the entire saved account history, not the UI's 100-row page.
	var transactions []db.Transaction
	if err := s.db.WithContext(r.Context()).Where("account_id = ?", account.ID).
		Order("date DESC, id ASC").Find(&transactions).Error; err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("could not load transactions"))
		return
	}
	rows := make([]exportedTransaction, 0, len(transactions))
	for _, tx := range transactions {
		rows = append(rows, exportedTransaction{account.ID, account.Name, tx.ID,
			tx.Date.Format("2006-01-02"), tx.Type, tx.ProductID, tx.Symbol,
			tx.Quantity, tx.Price, tx.Currency, tx.Fee, tx.Counterparty})
	}
	var output []byte
	var err error
	contentType := "text/csv; charset=utf-8"
	now := time.Now().UTC()
	switch format {
	case "csv":
		output, err = transactionsCSV(rows)
	case "json":
		contentType = "application/json; charset=utf-8"
		output, err = json.MarshalIndent(rows, "", "  ")
	case "pdf":
		contentType = "application/pdf"
		output, err = transactionsPDF(account, rows, now)
	}
	if err != nil {
		log.Printf("transaction export failed: %v", err)
		writeError(w, http.StatusInternalServerError, errors.New("could not create transaction export"))
		return
	}
	filename := fmt.Sprintf("degitide-%s-transactions-%s.%s", account.ID, now.Format("2006-01-02"), format)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(output)))
	_, _ = w.Write(output)
}

func exportNumber(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }

// Quote spreadsheet formula-like text without changing legitimate numeric cells.
func spreadsheetText(value string) string {
	trimmed := strings.TrimLeftFunc(value, unicode.IsSpace)
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") ||
		(trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0]))) {
		return "'" + value
	}
	return value
}

func transactionsCSV(rows []exportedTransaction) ([]byte, error) {
	var output bytes.Buffer
	writer := csv.NewWriter(&output)
	writer.UseCRLF = true
	if err := writer.Write([]string{"account_id", "account_name", "transaction_id", "date", "type", "product_id", "instrument", "quantity", "unit_price", "currency", "fee", "counterparty"}); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := writer.Write([]string{spreadsheetText(row.AccountID), spreadsheetText(row.AccountName),
			spreadsheetText(row.ID), row.Date, spreadsheetText(row.Type), spreadsheetText(row.ProductID),
			spreadsheetText(row.Instrument), exportNumber(row.Quantity), exportNumber(row.UnitPrice),
			spreadsheetText(row.Currency), exportNumber(row.Fee), spreadsheetText(row.Counterparty)}); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	return output.Bytes(), writer.Error()
}

func transactionsPDF(account db.Account, rows []exportedTransaction, now time.Time) ([]byte, error) {
	const width, height, margin = 842.0, 595.0, 36.0
	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: gopdf.Rect{W: width, H: height}})
	if err := pdf.AddTTFFontData("Go", goregular.TTF); err != nil {
		return nil, err
	}
	if err := pdf.AddTTFFontData("GoBold", gobold.TTF); err != nil {
		return nil, err
	}
	pdf.SetInfo(gopdf.PdfInfo{Title: "Degitide transaction history", Author: "Degitide"})
	var renderErr error
	text := func(x, y, w, size float64, bold bool, value string) {
		if renderErr != nil {
			return
		}
		font := "Go"
		if bold {
			font = "GoBold"
		}
		if renderErr = pdf.SetFont(font, "", size); renderErr != nil {
			return
		}
		pdf.SetXY(x, y)
		renderErr = pdf.Cell(&gopdf.Rect{W: w, H: size + 4}, value)
	}
	columns := []float64{76, 224, 56, 96, 112, 50, 156}
	headers := []string{"DATE", "INSTRUMENT", "TYPE", "UNITS", "UNIT PRICE", "CCY", "FEE*"}
	newPage := func() {
		pdf.AddPage()
		pdf.SetTextColor(45, 51, 42)
		text(margin, 27, 500, 24, true, "degitide / Transactions")
		text(margin, 64, 770, 11, false, account.Name+"  ·  "+account.ID)
		pdf.SetTextColor(103, 112, 94)
		text(margin, 85, 770, 9, false, fmt.Sprintf("%d saved transactions  |  Exported %s UTC", len(rows), now.Format("02 Jan 2006, 15:04")))
		pdf.SetFillColor(232, 236, 223)
		pdf.RectFromUpperLeftWithStyle(margin, 111, width-2*margin, 25, "F")
		x := margin
		for i, label := range headers {
			text(x+6, 118, columns[i]-12, 8, true, label)
			x += columns[i]
		}
	}
	split := func(value string, w float64, limit int) []string {
		if renderErr != nil {
			return nil
		}
		if renderErr = pdf.SetFont("Go", "", 9); renderErr != nil {
			return nil
		}
		var lines []string
		lines, renderErr = pdf.SplitTextWithWordWrap(strings.Join(strings.Fields(value), " "), w)
		if len(lines) > limit {
			lines = lines[:limit]
			lines[limit-1] = strings.TrimSpace(lines[limit-1]) + "…"
		}
		return lines
	}
	newPage()
	y := 144.0
	if len(rows) == 0 {
		text(margin, y+15, 770, 11, false, "No saved transactions for this account.")
	}
	for _, row := range rows {
		values := []string{row.Date, row.Instrument, row.Type, exportNumber(row.Quantity), exportNumber(row.UnitPrice), row.Currency, exportNumber(row.Fee)}
		lines := make([][]string, len(values))
		maxLines := 1
		for i, value := range values {
			lines[i] = split(value, columns[i]-18, 4)
			if len(lines[i]) > maxLines {
				maxLines = len(lines[i])
			}
		}
		detail := "ID: " + row.ID + "   |   Product: " + row.ProductID
		if row.Counterparty != "" {
			detail += "   |   Counterparty: " + row.Counterparty
		}
		details := split(detail, width-2*margin-18, 2)
		rowHeight := float64(maxLines*13 + len(details)*12 + 17)
		if y+rowHeight > height-79 {
			newPage()
			y = 144
		}
		x := margin
		pdf.SetTextColor(45, 51, 42)
		for i, cellLines := range lines {
			for j, line := range cellLines {
				text(x+6, y+float64(j)*13, columns[i]-12, 9, false, line)
			}
			x += columns[i]
		}
		pdf.SetTextColor(103, 112, 94)
		for j, line := range details {
			text(margin+6, y+float64(maxLines*13+j*12)+3, 750, 8, false, line)
		}
		y += rowHeight
		pdf.SetStrokeColor(220, 222, 210)
		pdf.SetLineWidth(.5)
		pdf.Line(margin, y-7, width-margin, y-7)
	}
	totalPages := pdf.GetNumberOfPages()
	for page := 1; page <= totalPages; page++ {
		if err := pdf.SetPage(page); err != nil {
			return nil, err
		}
		pdf.SetTextColor(103, 112, 94)
		text(margin, height-57, 770, 8, false, "* Fees are recorded as received; their currency is not inferred. Values in different currencies are not added together.")
		text(margin, height-43, 650, 8, false, "All saved history for this account. Long labels may be shortened; CSV and JSON retain their full text.")
		text(width-113, height-43, 77, 8, false, fmt.Sprintf("Page %d of %d", page, totalPages))
	}
	if renderErr != nil {
		return nil, renderErr
	}
	return pdf.GetBytesPdfReturnErr()
}
