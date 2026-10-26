package sipgateway

import (
	"strings"

	"github.com/emiago/sipgo/sip"
)

func registrarParser() *sip.Parser {
	// DefaultHeadersParser returns a shared map. Copy it instead of changing
	// process-global behavior for another device/runtime.
	parsers := map[string]sip.HeaderParser{}
	for name, parser := range sip.DefaultHeadersParser() {
		parsers[name] = parser
	}
	contactParser := parsers["contact"]
	strictContact := func(name []byte, value string) (sip.Header, error) {
		// sipgo normalizes parameters into a map and loses duplicate expires.
		// Keep malformed contacts generic so our handler can return 400 rather
		// than silently choosing a different registration lifetime.
		if duplicateExpires(value) {
			return sip.NewHeader("Contact", value), nil
		}
		return contactParser(name, value)
	}
	parsers["contact"] = strictContact
	parsers["m"] = strictContact
	fromParser := parsers["from"]
	strictFrom := func(name []byte, value string) (sip.Header, error) {
		if duplicateHeaderParameter(value, "tag") {
			return sip.NewHeader("From", value), nil
		}
		return fromParser(name, value)
	}
	parsers["from"] = strictFrom
	parsers["f"] = strictFrom
	toParser := parsers["to"]
	strictTo := func(name []byte, value string) (sip.Header, error) {
		if duplicateHeaderParameter(value, "tag") {
			return sip.NewHeader("To", value), nil
		}
		return toParser(name, value)
	}
	parsers["to"] = strictTo
	parsers["t"] = strictTo
	parser := sip.NewParser(sip.WithHeadersParsers(parsers))
	parser.MaxMessageLength = 16 * 1024
	return parser
}
func duplicateExpires(value string) bool {
	return duplicateHeaderParameter(value, "expires")
}
func duplicateHeaderParameter(value, key string) bool {
	quoted, escaped, inURI := false, false, false
	seen := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '<':
			inURI = true
		case '>':
			inURI = false
		case ',':
			if !inURI {
				seen = false
			}
		case ';':
			if inURI {
				continue
			}
			end := i + 1
			for end < len(value) && value[end] != '=' && value[end] != ';' && value[end] != ',' {
				end++
			}
			if strings.EqualFold(strings.TrimSpace(value[i+1:end]), key) {
				if seen {
					return true
				}
				seen = true
			}
		}
	}
	return false
}
