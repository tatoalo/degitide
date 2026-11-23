package db

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"
)

// ErrTransactionIdentityReconciliation means no identity change was published.
// Its message deliberately excludes broker payloads and account details.
var ErrTransactionIdentityReconciliation = errors.New("transaction identities could not be reconciled safely")

// RepairTransactionIdentities migrates legacy fallback IDs before reimporting
// an account. The old importer used orderId (shared by separate fills), or a
// hash sensitive to JSON key order. Explicit broker execution IDs do not change.
// Conflicts are removed only for the same raw execution with equal economic
// fields; an ambiguous candidate fails the whole account transaction.
func (d *DB) RepairTransactionIdentities(accountID string) error {
	return d.DB.Transaction(func(tx *gorm.DB) error {
		var rows []Transaction
		if err := tx.Where("account_id = ?", accountID).Order("id").Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			identity, canonical, err := repairedTransactionIdentity(row)
			if err != nil {
				return err
			}
			if identity == row.ID {
				continue
			}
			var existing Transaction
			err = tx.Where("account_id = ? AND id = ?", accountID, identity).First(&existing).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				if err := tx.Model(&Transaction{}).Where("account_id = ? AND id = ?", accountID, row.ID).Update("id", identity).Error; err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				other, err := canonicalTransactionJSON(existing.RawJSON)
				if err != nil || !bytes.Equal(canonical, other) || !sameExecutionEconomics(row, existing) {
					return fmt.Errorf("%w: conflicting execution records", ErrTransactionIdentityReconciliation)
				}
				if err := tx.Where("account_id = ? AND id = ?", accountID, row.ID).Delete(&Transaction{}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func sameExecutionEconomics(a, b Transaction) bool {
	return a.ProductID == b.ProductID && strings.EqualFold(a.Type, b.Type) &&
		a.Date.Format("2006-01-02") == b.Date.Format("2006-01-02") &&
		a.Quantity == b.Quantity && a.Price == b.Price && a.Fee == b.Fee &&
		(a.Currency == "" || b.Currency == "" || a.Currency == b.Currency)
}

func repairedTransactionIdentity(row Transaction) (string, []byte, error) {
	canonical, err := canonicalTransactionJSON(row.RawJSON)
	if err != nil {
		if strings.HasPrefix(row.ID, "derived-") {
			return "", nil, fmt.Errorf("%w: missing fallback execution evidence", ErrTransactionIdentityReconciliation)
		}
		// Hand-entered/imported records without fallback evidence are outside
		// this migration. Never infer an identity from financial amounts alone.
		return row.ID, nil, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &raw); err != nil {
		return "", nil, ErrTransactionIdentityReconciliation
	}
	for _, key := range []string{"id", "transactionId"} {
		value := raw[key]
		if len(value) == 0 || string(value) == "null" {
			continue
		}
		identity, err := rawTransactionIdentifier(value)
		if err != nil || identity != row.ID {
			return "", nil, fmt.Errorf("%w: broker execution ID mismatch", ErrTransactionIdentityReconciliation)
		}
		return row.ID, canonical, nil
	}
	// A raw object without identifying execution fields can be an external
	// import, not a legacy fallback row. Leave those records untouched.
	if len(raw["productId"]) == 0 && !strings.HasPrefix(row.ID, "derived-") && len(raw["orderId"]) == 0 {
		return row.ID, canonical, nil
	}
	identity := derivedTransactionID(canonical)
	if row.ID == identity || row.ID == derivedTransactionID([]byte(row.RawJSON)) {
		return identity, canonical, nil
	}
	if order, err := rawTransactionIdentifier(raw["orderId"]); err == nil && row.ID == order {
		return identity, canonical, nil
	}
	return "", nil, fmt.Errorf("%w: unproven fallback execution ID", ErrTransactionIdentityReconciliation)
}

func derivedTransactionID(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "derived-" + hex.EncodeToString(digest[:12])
}

func rawTransactionIdentifier(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text), nil
	}
	value := string(raw)
	if value != "" {
		if isIntegerIdentifier(value) {
			return value, nil
		}
	}
	return "", ErrTransactionIdentityReconciliation
}

// isIntegerIdentifier accepts Python's arbitrary-sized JSON integer IDs.
func isIntegerIdentifier(value string) bool {
	start := 0
	if len(value) > 0 && value[0] == '-' {
		start++
	}
	if start == len(value) {
		return false
	}
	for _, r := range value[start:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// canonicalTransactionJSON matches Python's json.dumps(sort_keys=True,
// ensure_ascii=False, separators=(",", ":")) for stored worker raw_json.
// UseNumber preserves the Python-formatted numeric lexemes already recorded by
// the worker, including 1.0 and exponent spelling, which affect its hash.
func canonicalTransactionJSON(raw string) ([]byte, error) {
	if !utf8.ValidString(raw) {
		return nil, ErrTransactionIdentityReconciliation
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, ErrTransactionIdentityReconciliation
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, ErrTransactionIdentityReconciliation
	}
	var out bytes.Buffer
	writeCanonicalTransactionJSON(&out, value)
	return out.Bytes(), nil
}

func writeCanonicalTransactionJSON(out *bytes.Buffer, value any) {
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(v))
	case json.Number:
		out.WriteString(v.String())
	case string:
		writePythonJSONString(out, v)
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			writeCanonicalTransactionJSON(out, item)
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			writePythonJSONString(out, key)
			out.WriteByte(':')
			writeCanonicalTransactionJSON(out, v[key])
		}
		out.WriteByte('}')
	}
}

func writePythonJSONString(out *bytes.Buffer, value string) {
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}
