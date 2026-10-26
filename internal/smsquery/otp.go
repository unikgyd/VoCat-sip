package smsquery

import "strings"

// ExtractCode is an independently implemented context heuristic, tested against
// contracts/otp-extraction-v1.json. It is not an identity-verification decision.
func ExtractCode(text string) *string {
	runes := []rune(text)
	for i, r := range runes {
		if r >= '０' && r <= '９' {
			runes[i] = '0' + r - '０'
		}
	}
	best, bestScore := "", 0
	for start := 0; start < len(runes); {
		if runes[start] < '0' || runes[start] > '9' {
			start++
			continue
		}
		end := start
		for end < len(runes) && runes[end] >= '0' && runes[end] <= '9' {
			end++
		}
		code := string(runes[start:end])
		if len(code) >= 2 && len(code) < 4 && end+2 < len(runes) && strings.ContainsRune("-－‐‑–", runes[end]) {
			second := end + 1
			stop := second
			for stop < len(runes) && runes[stop] >= '0' && runes[stop] <= '9' {
				stop++
			}
			if stop-second >= 2 {
				code += string(runes[second:stop])
				end = stop
			}
		}
		if len(code) >= 4 && len(code) <= 8 {
			near := strings.ToLower(string(runes[max(0, start-32):min(len(runes), end+32)]))
			before := strings.TrimRight(string(runes[max(0, start-10):start]), " :：\t-_是为的您你，,()（）")
			after := strings.TrimSpace(string(runes[end:min(len(runes), end+10)]))
			strong := hasAny(near, []string{"验证码", "校验码", "动态码", "认证码", "短信码", "登录码", "安全码", "验证代码"}) || word(near, "code") || word(near, "otp") || word(near, "pin") || word(near, "passcode")
			negative := false
			for _, v := range []string{"订单号", "运单号", "工单号", "流水号", "编号", "序号", "金额", "余额", "积分", "卡号", "尾号", "手机号", "账号", "账户", "日期", "版本", "端口"} {
				if strings.HasSuffix(before, v) {
					negative = true
				}
			}
			for _, v := range []string{"元", "年", "月", "日", "%", "gb", "mb", "kb", "tb", "bps"} {
				if strings.HasPrefix(strings.ToLower(after), v) {
					negative = true
				}
			}
			if strings.HasSuffix(before, ".") || strings.HasPrefix(after, ".") || strings.HasSuffix(before, "/") || strings.HasPrefix(after, "/") {
				negative = true
			}
			if strong && !negative {
				score := 10
				if len(code) == 6 {
					score = 20
				}
				if score > bestScore {
					best, bestScore = code, score
				}
			}
		}
		start = end
	}
	if best == "" {
		return nil
	}
	return &best
}
func hasAny(text string, words []string) bool {
	for _, w := range words {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}
func word(s, w string) bool {
	wordChar := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' }
	for offset := 0; offset < len(s); {
		i := strings.Index(s[offset:], w)
		if i < 0 {
			return false
		}
		i += offset
		e := i + len(w)
		if (i == 0 || !wordChar(s[i-1])) && (e == len(s) || !wordChar(s[e])) {
			return true
		}
		offset = e
	}
	return false
}
