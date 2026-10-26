// Package modulecall owns USB modem calls independently of IMS registration.
package modulecall

import (
	"context"
	"encoding/csv"
	"errors"
	"strconv"
	"strings"

	"vocat/internal/modem"
	"vocat/internal/vowifi"
)

var (
	ErrUnavailable  = errors.New("module call: the selected module has no ready USB voice path")
	ErrBusy         = errors.New("module call: the selected module is already owned")
	ErrChanged      = errors.New("module call: physical device, SIM, or call lifecycle changed")
	ErrState        = errors.New("module call: the exact call is not in the required state")
	ErrNotFound     = errors.New("module call: exact call not found")
	ErrUncertain    = errors.New("module call: call outcome or cleanup is not confirmed")
	ErrNotAttempted = errors.New("module call: answer was not submitted")
	ErrClosed       = errors.New("module call: manager is closed")
)

type Record struct {
	Index, Direction, State, Mode, Multiparty int
	Number                                    string
}
type Action struct {
	Kind, Number string
	Expected     *Record
	Digit        byte
}

type Audio interface {
	vowifi.CallMedia
	Start() error
	Close() error
	Done() <-chan struct{}
}

// A lease is an opaque physical/SIM/control-session reservation, never a JSON
// device path. Execute must revalidate the card and actual CLCC under the same
// per-modem command lock and fence terminal URCs at the final serial write.
type Lease interface {
	Current() bool
	Read(context.Context) ([]Record, error)
	Execute(context.Context, Action) (attempted bool, err error)
	OpenAudio(context.Context, func() bool) (Audio, error)
	Close()
}
type Backend interface {
	Available(context.Context, string) error // metadata only; no AT or audio open
	Acquire(context.Context, string) (Lease, error)
}

// SignalingBackend is optional. Maintenance calls can retain the same exact
// physical/call ownership on an AT modem that has no USB audio endpoint.
type SignalingBackend interface {
	AvailableSignaling(context.Context, string) error
	AcquireSignaling(context.Context, string) (Lease, error)
}

func ParseRecords(response modem.Response) ([]Record, error) {
	if !response.OK() {
		return nil, ErrUncertain
	}
	var records []Record
	seen := map[int]bool{}
	for _, line := range response.Lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+CLCC:") {
			continue
		}
		reader := csv.NewReader(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, "+CLCC:"))))
		reader.TrimLeadingSpace = true
		fields, err := reader.Read()
		if err != nil || len(fields) < 5 || len(fields) > 8 {
			return nil, ErrUncertain
		}
		values := make([]int, 5)
		for i := range values {
			n, e := strconv.Atoi(strings.TrimSpace(fields[i]))
			if e != nil {
				return nil, ErrUncertain
			}
			values[i] = n
		}
		r := Record{Index: values[0], Direction: values[1], State: values[2], Mode: values[3], Multiparty: values[4]}
		if r.Index < 1 || r.Index > 7 || seen[r.Index] || r.Direction < 0 || r.Direction > 1 || r.State < 0 || r.State > 5 || r.Mode < 0 || r.Mode > 2 || r.Multiparty < 0 || r.Multiparty > 1 {
			return nil, ErrUncertain
		}
		seen[r.Index] = true
		if len(fields) > 5 {
			r.Number = strings.TrimSpace(fields[5])
			if len(r.Number) > 64 || strings.ContainsAny(r.Number, "\r\n\x00") {
				return nil, ErrUncertain
			}
			if len(fields) > 6 {
				kind, e := strconv.Atoi(strings.TrimSpace(fields[6]))
				if e != nil || kind < 0 || kind > 255 {
					return nil, ErrUncertain
				}
				if (kind>>4)&7 == 1 && r.Number != "" && !strings.HasPrefix(r.Number, "+") && strings.Trim(r.Number, "0123456789") == "" {
					r.Number = "+" + r.Number
				}
			}
		}
		records = append(records, r)
	}
	return records, nil
}
func VoiceRecords(records []Record) []Record {
	var result []Record
	for _, r := range records {
		if r.Mode == 0 {
			result = append(result, r)
		}
	}
	return result
}
func ValidNumber(number string) bool {
	if len(number) < 2 || len(number) > 32 {
		return false
	}
	for i, c := range number {
		if c >= '0' && c <= '9' || i == 0 && c == '+' || c == '*' || c == '#' {
			continue
		}
		return false
	}
	return true
}
func ID(id string) bool {
	return len(id) == 36 && strings.HasPrefix(id, "usb-") && strings.Trim(id[4:], "0123456789abcdef") == ""
}
