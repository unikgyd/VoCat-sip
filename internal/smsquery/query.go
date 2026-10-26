// Package smsquery defines the bounded, read-only cross-client SMS API contract.
package smsquery

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Query struct {
	Since, Until            time.Time
	Limit                   int
	Keyword, Sender, SlotID string
	OTPOnly                 bool
}
type Message struct {
	ID            string    `json:"id"`
	SlotID        *string   `json:"slotId"`
	Sender        string    `json:"sender"`
	Text          string    `json:"text"`
	Timestamp     time.Time `json:"timestampUtc"`
	Code          *string   `json:"code"`
	TextTruncated bool      `json:"textTruncated"`
}
type Result struct {
	Messages    []Message
	ScanLimited bool
}

var ErrQuery = errors.New("invalid query")

func Parse(raw string, otp bool, now time.Time) (Query, error) {
	q := Query{Until: now.UTC(), Limit: 20, OTPOnly: otp}
	age := 86400
	if otp {
		q.Limit = 1
		age = 300
	}
	values := map[string]string{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return q, ErrQuery
		}
		key, err := url.QueryUnescape(key)
		if err != nil {
			return q, ErrQuery
		}
		value, err = url.QueryUnescape(value)
		if err != nil || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return q, ErrQuery
		}
		switch key {
		case "keyword", "sender", "slotId", "limit", "otpOnly", "maxAgeSeconds", "since":
		default:
			return q, ErrQuery
		}
		if _, duplicate := values[key]; duplicate {
			return q, ErrQuery
		}
		values[key] = value
	}
	for _, n := range []struct {
		key    string
		target *int
		max    int
	}{{"limit", &q.Limit, 100}, {"maxAgeSeconds", &age, 604800}} {
		if v, ok := values[n.key]; ok {
			if v == "" || strings.ContainsFunc(v, func(r rune) bool { return r < '0' || r > '9' }) {
				return q, ErrQuery
			}
			parsed, err := strconv.Atoi(v)
			if err != nil || parsed < 1 || parsed > n.max {
				return q, ErrQuery
			}
			*n.target = parsed
		}
	}
	if v, ok := values["otpOnly"]; ok {
		switch {
		case strings.EqualFold(v, "true"):
			q.OTPOnly = true
		case strings.EqualFold(v, "false"):
			q.OTPOnly = false
		default:
			return q, ErrQuery
		}
	}
	if otp && (q.Limit != 1 || !q.OTPOnly) {
		return q, ErrQuery
	}
	q.Since = now.Add(-time.Duration(age) * time.Second).UTC()
	if v, ok := values["since"]; ok {
		// RFC 3339 requires an explicit timezone. Lowercase z is accepted by Windows.
		if strings.HasSuffix(v, "z") {
			v = strings.TrimSuffix(v, "z") + "Z"
		}
		parsed, err := time.Parse(time.RFC3339Nano, v)
		if err != nil || parsed.After(now) {
			return q, ErrQuery
		}
		if parsed.After(q.Since) {
			q.Since = parsed.UTC()
		}
	}
	q.Keyword, q.Sender, q.SlotID = values["keyword"], values["sender"], values["slotId"]
	if Units(q.Keyword) > 200 || Units(q.Sender) > 100 || Units(q.SlotID) > 128 {
		return q, ErrQuery
	}
	return q, nil
}

// Units and Truncate use UTF-16 units for compatibility with Windows without
// splitting Unicode scalars. Strings are never byte-sliced through an emoji.
func Units(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}
func Truncate(s string) (string, bool) {
	n := 0
	for i, r := range s {
		step := 1
		if r > 0xffff {
			step = 2
		}
		if n+step > 4096 {
			return s[:i], true
		}
		n += step
	}
	return s, false
}
func Contains(text, keyword string) bool {
	// OrdinalIgnoreCase is not locale folding or Unicode compatibility folding.
	fold := func(r rune) rune {
		if r == 'ı' || r == 'ſ' {
			return r
		}
		return unicode.ToUpper(r)
	}
	return strings.Contains(strings.Map(fold, text), strings.Map(fold, keyword))
}
