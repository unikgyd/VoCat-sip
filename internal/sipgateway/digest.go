package sipgateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Parse quoted Digest fields strictly; duplicates and incomplete tokens must
// not turn into an alternate interpretation in another SIP implementation.
func parseDigest(value string) (map[string]string, error) {
	return parseDigestFields(value, false)
}
func parseDigestFields(value string, allowEmptyRealm bool) (map[string]string, error) {
	if len(value) > 4096 || len(value) < 7 || !strings.EqualFold(value[:7], "Digest ") {
		return nil, errors.New("invalid digest")
	}
	value = value[7:]
	fields := map[string]string{}
	for len(strings.TrimSpace(value)) > 0 {
		value = strings.TrimSpace(value)
		eq := strings.IndexByte(value, '=')
		if eq < 1 {
			return nil, errors.New("invalid digest field")
		}
		key := strings.ToLower(strings.TrimSpace(value[:eq]))
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate digest field")
		}
		if !digestToken(key) {
			return nil, errors.New("invalid digest key")
		}
		value = strings.TrimSpace(value[eq+1:])
		var field string
		quoted := false
		if strings.HasPrefix(value, "\"") {
			quoted = true
			value = value[1:]
			var b strings.Builder
			ended := false
			for i := 0; i < len(value); i++ {
				c := value[i]
				if c == '"' {
					field = b.String()
					value = value[i+1:]
					ended = true
					break
				}
				if c == '\\' {
					i++
					if i >= len(value) {
						break
					}
					c = value[i]
				}
				if c < 32 || c == 127 {
					return nil, errors.New("invalid quoted digest")
				}
				b.WriteByte(c)
			}
			if !ended {
				return nil, errors.New("unterminated digest")
			}
		} else {
			comma := strings.IndexByte(value, ',')
			if comma < 0 {
				field = strings.TrimSpace(value)
				value = ""
			} else {
				field = strings.TrimSpace(value[:comma])
				value = value[comma:]
			}
			if !digestToken(field) {
				return nil, errors.New("invalid unquoted digest field")
			}
		}
		if field == "" && !(allowEmptyRealm && key == "realm" && quoted) || strings.ContainsFunc(field, unicode.IsControl) {
			return nil, errors.New("empty or unsafe digest field")
		}
		fields[key] = field
		value = strings.TrimSpace(value)
		if value != "" {
			if value[0] != ',' {
				return nil, errors.New("invalid digest separator")
			}
			value = value[1:]
			if strings.TrimSpace(value) == "" {
				return nil, errors.New("trailing digest separator")
			}
		}
	}
	return fields, nil
}

func digestToken(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-.!%*_+`'~", c) {
			continue
		}
		return false
	}
	return true
}

func (r *runtime) challenge(source string, stale bool) string {
	data := make([]byte, 24)
	binary.BigEndian.PutUint64(data, uint64(time.Now().Unix()))
	_, _ = rand.Read(data[8:])
	mac := hmac.New(sha256.New, r.key[:])
	mac.Write(data)
	mac.Write([]byte(source))
	data = append(data, mac.Sum(nil)...)
	nonce := base64.RawURLEncoding.EncodeToString(data)
	return fmt.Sprintf("Digest realm=\"%s\",nonce=\"%s\",algorithm=%s,qop=\"auth\",opaque=\"%s\",stale=%t", r.config.Config.Realm, nonce, r.config.Config.Algorithm, r.opaque, stale)
}

func (r *runtime) authenticate(method, uri, source, header string) (string, bool) {
	return r.authenticateCredentials(method, uri, source, header, "local", func(username string) (secret, bool) {
		credentials, exists := r.config.Secrets[username]
		return credentials, exists
	})
}
func (r *runtime) authenticateCredentials(method, uri, source, header, scope string, lookup func(string) (secret, bool)) (string, bool) {
	f, err := parseDigest(header)
	if err != nil {
		return "", false
	}
	algorithm := strings.ToUpper(f["algorithm"])
	if algorithm == "" {
		algorithm = "MD5"
	}
	if algorithm != r.config.Config.Algorithm || f["realm"] != r.config.Config.Realm || f["uri"] != uri || f["qop"] != "auth" || f["opaque"] != r.opaque || len(f["cnonce"]) > 128 || f["cnonce"] == "" {
		return "", false
	}
	credentials, exists := lookup(f["username"])
	if !exists {
		return "", false
	}
	data, err := base64.RawURLEncoding.DecodeString(f["nonce"])
	if err != nil || len(data) != 56 {
		return "", false
	}
	now := time.Now()
	issued := int64(binary.BigEndian.Uint64(data[:8]))
	if issued > now.Unix()+5 || now.Unix()-issued > 300 {
		return "", false
	}
	mac := hmac.New(sha256.New, r.key[:])
	mac.Write(data[:24])
	mac.Write([]byte(source))
	if !hmac.Equal(mac.Sum(nil), data[24:]) {
		return "", false
	}
	if len(f["nc"]) != 8 {
		return "", false
	}
	nc, err := strconv.ParseUint(f["nc"], 16, 32)
	if err != nil || nc == 0 {
		return "", false
	}
	ha1 := credentials.SHA256
	if algorithm == "MD5" {
		ha1 = credentials.MD5
	}
	expected := digestHash(algorithm, ha1+":"+f["nonce"]+":"+f["nc"]+":"+f["cnonce"]+":auth:"+digestHash(algorithm, method+":"+uri))
	provided, err := hex.DecodeString(f["response"])
	want, _ := hex.DecodeString(expected)
	if err != nil || len(provided) != len(want) || subtle.ConstantTimeCompare(provided, want) != 1 {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	id := scope + "\x00" + f["username"] + "\x00" + f["nonce"] + "\x00" + f["cnonce"]
	if previous, ok := r.replays[id]; ok && nc <= previous.count {
		return "", false
	}
	if _, ok := r.replays[id]; !ok && len(r.replays) >= 2048 {
		return "", false
	}
	r.replays[id] = replay{count: nc, until: time.Unix(issued+301, 0)}
	return f["username"], true
}
