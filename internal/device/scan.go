package device

import (
	"context"
	"strings"

	"vocat/internal/modem"
)

// ScannedOperator is one network reported by an operator scan (AT+COPS=?).
type ScannedOperator struct {
	// Status is "current", "available", "forbidden", or "unknown".
	Status  string `json:"status"`
	Name    string `json:"name"`
	Short   string `json:"shortName,omitempty"`
	Numeric string `json:"numeric"`
	Country string `json:"countryCode,omitempty"`
	Act     string `json:"act,omitempty"`
}

// OperatorScanResult is the outcome of a full operator scan.
type OperatorScanResult struct {
	Status    string            `json:"status"` // "complete" or "failed"
	Operators []ScannedOperator `json:"operators"`
}

// ScanOperators runs AT+COPS=? to list the networks the modem can currently
// see. The command is slow (tens of seconds, up to the modem's documented
// ceiling), so it uses the manager's scan timeout rather than the normal
// command timeout. It is abortable through the caller's context.
func (manager *Manager) ScanOperators(
	ctx context.Context,
	id string,
) (OperatorScanResult, error) {
	state, err := manager.lookup(id)
	if err != nil {
		return OperatorScanResult{}, err
	}
	state.opMu.Lock()
	defer state.opMu.Unlock()
	if err := manager.validateActive(id, state); err != nil {
		return OperatorScanResult{}, err
	}
	if err := manager.requireNoModuleVoice(state); err != nil {
		return OperatorScanResult{}, err
	}
	client, err := manager.clientLocked(ctx, state, manager.candidateFor(state))
	if err != nil {
		manager.setResult(id, state, nil, err)
		return OperatorScanResult{}, err
	}
	scanContext, cancel := manager.withTimeout(ctx, manager.scanTimeout)
	defer cancel()
	response, err := client.Execute(scanContext, "AT+COPS=?")
	if err != nil {
		manager.setResult(id, state, nil, err)
		return OperatorScanResult{Status: "failed", Operators: []ScannedOperator{}}, err
	}
	result := OperatorScanResult{
		Status:    "complete",
		Operators: parseOperatorScan(response),
	}
	manager.setResult(id, state, nil, nil)
	return result, nil
}

// parseOperatorScan parses the +COPS: list returned by AT+COPS=?. Each entry is
// a parenthesised tuple (stat,"long","short","numeric"[,act]).
func parseOperatorScan(response modem.Response) []ScannedOperator {
	operators := make([]ScannedOperator, 0)
	for _, line := range response.Lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(trimmed), "+COPS:") {
			continue
		}
		payload := strings.TrimSpace(trimmed[len("+COPS:"):])
		for _, tuple := range extractScanTuples(payload) {
			fields := csvValues(tuple)
			if len(fields) < 4 {
				continue
			}
			name, country, _ := CarrierForPLMN(fields[3])
			if name == "" {
				name = strings.TrimSpace(fields[1])
			}
			if name == "" {
				name = strings.TrimSpace(fields[3])
			}
			operator := ScannedOperator{
				Status:  operatorScanStatus(fields[0]),
				Name:    name,
				Short:   fields[2],
				Numeric: fields[3],
				Country: country,
			}
			if len(fields) >= 5 {
				operator.Act = accessTechnology(fields[4])
			}
			operators = append(operators, operator)
		}
	}
	return operators
}

// carrierNameForPLMN resolves the numeric serving PLMN through the bundled
// global carrier database. Some EC20 firmware returns an empty, localized, or
// stale long name even though the MCC/MNC is correct. The numeric identity is
// the authoritative value used for network selection.
func carrierNameForPLMN(plmn, fallback string) string {
	if name, _, ok := CarrierForPLMN(plmn); ok {
		return name
	}
	if fallback = strings.TrimSpace(fallback); fallback != "" {
		return fallback
	}
	return strings.TrimSpace(plmn)
}

// extractScanTuples returns the contents of each top-level parenthesised group,
// ignoring parentheses inside quoted strings.
func extractScanTuples(payload string) []string {
	tuples := make([]string, 0)
	depth := 0
	start := -1
	inQuote := false
	for index, r := range payload {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == '(' && !inQuote:
			if depth == 0 {
				start = index + 1
			}
			depth++
		case r == ')' && !inQuote:
			depth--
			if depth == 0 && start >= 0 {
				tuples = append(tuples, payload[start:index])
				start = -1
			}
		}
	}
	return tuples
}

func operatorScanStatus(code string) string {
	switch strings.TrimSpace(code) {
	case "1":
		return "available"
	case "2":
		return "current"
	case "3":
		return "forbidden"
	default:
		return "unknown"
	}
}
