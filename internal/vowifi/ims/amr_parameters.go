package ims

import (
	"errors"
	"strconv"
	"strings"
)

type amrParameters struct {
	octet    bool
	modes    [8]bool
	period   int
	neighbor bool
}

func parseAMRParameters(body []byte, payload byte) (amrParameters, error) {
	p := amrParameters{period: 1}
	for i := range p.modes {
		p.modes[i] = true
	}
	block, err := selectAudioSDP(body)
	if err != nil {
		return p, err
	}
	prefix := "a=fmtp:" + strconv.Itoa(int(payload))
	value := ""
	found := false
	for _, raw := range strings.Split(string(block), "\n") {
		line := strings.TrimSpace(raw)
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.EqualFold(fields[0], prefix) {
			if found {
				return p, errors.New("ims: repeated AMR fmtp")
			}
			found = true
			value = strings.TrimSpace(line[len(fields[0]):])
		}
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return p, errors.New("ims: malformed AMR fmtp")
		}
		key, val := strings.ToLower(strings.TrimSpace(kv[0])), strings.TrimSpace(kv[1])
		if seen[key] {
			return p, errors.New("ims: duplicate AMR parameter")
		}
		seen[key] = true
		switch key {
		case "octet-align":
			if val != "0" && val != "1" {
				return p, errors.New("ims: invalid AMR alignment")
			}
			p.octet = val == "1"
		case "mode-set":
			p.modes = [8]bool{}
			for _, m := range strings.Split(val, ",") {
				m = strings.TrimSpace(m)
				if len(m) != 1 || m[0] < '0' || m[0] > '7' || p.modes[m[0]-'0'] {
					return p, errors.New("ims: invalid AMR mode-set")
				}
				p.modes[m[0]-'0'] = true
			}
		case "mode-change-period":
			if val != "1" && val != "2" {
				return p, errors.New("ims: invalid AMR mode change period")
			}
			p.period = int(val[0] - '0')
		case "mode-change-neighbor":
			if val != "0" && val != "1" {
				return p, errors.New("ims: invalid AMR neighboring-mode constraint")
			}
			p.neighbor = val == "1"
		case "mode-change-capability":
			if val != "1" && val != "2" {
				return p, errors.New("ims: invalid AMR mode change capability")
			}
		case "crc", "robust-sorting":
			if val != "0" {
				return p, errors.New("ims: AMR CRC/robust sorting is not supported")
			}
		case "interleaving":
			return p, errors.New("ims: AMR interleaving is not supported")
		}
	}
	return p, nil
}

func (p amrParameters) highest() int {
	for i := 7; i >= 0; i-- {
		if p.modes[i] {
			return i
		}
	}
	return -1
}
func (p amrParameters) next(current, request int, sent uint64) int {
	if request < 0 || request > 7 || !p.modes[request] || p.period == 0 || sent%uint64(p.period) != 0 {
		return current
	}
	if !p.neighbor {
		return request
	}
	if request < current {
		for i := current - 1; i >= request; i-- {
			if p.modes[i] {
				return i
			}
		}
	}
	if request > current {
		for i := current + 1; i <= request; i++ {
			if p.modes[i] {
				return i
			}
		}
	}
	return current
}

func (p amrParameters) fmtp(payload byte) string {
	modes := []string{}
	for i, yes := range p.modes {
		if yes {
			modes = append(modes, strconv.Itoa(i))
		}
	}
	octet := "0"
	if p.octet {
		octet = "1"
	}
	neighbor := "0"
	if p.neighbor {
		neighbor = "1"
	}
	return "a=fmtp:" + strconv.Itoa(int(payload)) + " octet-align=" + octet + ";mode-set=" + strings.Join(modes, ",") + ";mode-change-capability=2;mode-change-period=" + strconv.Itoa(p.period) + ";mode-change-neighbor=" + neighbor
}

// Select one usable audio block; video and later audio cannot change its
// connection, payload assignments or AMR format parameters.
func selectAudioSDP(body []byte) ([]byte, error) {
	if len(body) > 65536 {
		return nil, errors.New("ims: excessive SDP")
	}
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	firstMedia := len(lines)
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "m=") {
			firstMedia = i
			break
		}
	}
	for start := firstMedia; start < len(lines); start++ {
		f := strings.Fields(strings.TrimSpace(lines[start]))
		if len(f) < 4 || f[0] != "m=audio" {
			continue
		}
		port, err := strconv.Atoi(f[1])
		if err != nil || port < 1 || port > 65535 || !strings.EqualFold(f[2], "RTP/AVP") {
			continue
		}
		end := start + 1
		for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "m=") {
			end++
		}
		selected := append([]string(nil), lines[:firstMedia]...)
		selected = append(selected, lines[start:end]...)
		return []byte(strings.Join(selected, "\n")), nil
	}
	return nil, errors.New("ims: no supported active RTP/AVP audio block")
}
