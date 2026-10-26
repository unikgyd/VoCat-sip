package ike

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"vocat/internal/vowifi"
)

const googleDNSOverHTTPS = "https://dns.google/resolve"

var standardEPDGName = regexp.MustCompile(`^epdg\.epc\.mnc[0-9]{2,3}\.mcc[0-9]{3}\.pub\.3gppnetwork\.org$`)

type dnsOverHTTPSResponse struct {
	Status int `json:"Status"`
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"Answer"`
}

func resolveEPDG(ctx context.Context, resolver *net.Resolver, host string) ([]net.IPAddr, error) {
	return resolveEPDGOptions(ctx, resolver, host, false)
}
func resolveEPDGOptions(ctx context.Context, resolver *net.Resolver, host string, geographicFallback bool) ([]net.IPAddr, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	normalized, err := vowifi.NormalizeEPDGHost(host)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(normalized); ip != nil {
		return []net.IPAddr{{IP: ip}}, nil
	}
	hostsToTry := []string{normalized}
	if alt := alternate3GPPHostname(normalized); alt != "" && alt != normalized {
		hostsToTry = append(hostsToTry, alt)
	}

	var systemErr error
	for _, targetHost := range hostsToTry {
		addresses, err := resolveSystemFamilies(ctx, resolver, targetHost)
		if err == nil && len(addresses) > 0 {
			return addresses, nil
		}
		systemErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if !geographicFallback {
		return nil, fmt.Errorf("system ePDG DNS returned no usable addresses: %w", systemErr)
	}

	subnet := vowifi.EPDGDNSClientSubnet(normalized)
	client := &http.Client{Timeout: 8 * time.Second}
	var fallbackErr error

	for _, targetHost := range hostsToTry {
		var addresses []net.IPAddr
		for _, kind := range []string{"A", "AAAA"} {
			fallback, err := resolveEPDGWithECSType(ctx, client, googleDNSOverHTTPS, targetHost, subnet, kind)
			if err == nil {
				addresses = append(addresses, fallback...)
			} else {
				fallbackErr = err
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		if addresses = filterValidPublicEPDGAddresses(addresses); len(addresses) > 0 {
			return addresses, nil
		}
	}
	if systemErr == nil {
		systemErr = errors.New("system DNS returned no usable public IP addresses")
	}
	return nil, fmt.Errorf("system DNS failed (%v); geographic DNS fallback failed: %w", systemErr, fallbackErr)
}

type epdgIPLookup interface {
	LookupIP(context.Context, string, string) ([]net.IP, error)
}

func resolveSystemFamilies(ctx context.Context, lookup epdgIPLookup, host string) ([]net.IPAddr, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	type result struct {
		index int
		ips   []net.IP
		err   error
	}
	results := make(chan result, 2)
	for index, family := range []string{"ip4", "ip6"} {
		go func(index int, family string) {
			ips, err := lookup.LookupIP(queryCtx, family, host)
			results <- result{index, ips, err}
		}(index, family)
	}
	families := [2][]net.IP{}
	var lastErr error
	for remaining := 2; remaining > 0; remaining-- {
		select {
		case got := <-results:
			families[got.index] = got.ips
			lastErr = got.err
		case <-queryCtx.Done():
			lastErr = queryCtx.Err()
			remaining = 0
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	addresses := []net.IPAddr{}
	for _, ips := range families {
		for _, ip := range ips {
			addresses = append(addresses, net.IPAddr{IP: ip})
		}
	}
	addresses = filterValidPublicEPDGAddresses(addresses)
	if len(addresses) > 0 {
		return addresses, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no usable A or AAAA record")
	}
	return nil, lastErr
}

func filterValidPublicEPDGAddresses(addresses []net.IPAddr) []net.IPAddr {
	result := make([]net.IPAddr, 0, len(addresses))
	for _, addr := range addresses {
		if !vowifi.UsableEPDGAddress(addr.IP) {
			continue
		}
		duplicate := false
		for _, old := range result {
			if old.IP.Equal(addr.IP) {
				duplicate = true
				break
			}
		}
		if !duplicate && len(result) < 16 {
			result = append(result, net.IPAddr{IP: append(net.IP(nil), addr.IP...)})
		}
	}
	return result
}

func alternate3GPPHostname(host string) string {
	if !standardEPDGName.MatchString(host) {
		return ""
	}
	const prefix = "epdg.epc.mnc"
	if !strings.HasPrefix(host, prefix) {
		return ""
	}
	rest := host[len(prefix):]
	dot := strings.Index(rest, ".")
	if dot <= 0 {
		return ""
	}
	mnc := rest[:dot]
	suffix := rest[dot:]
	if len(mnc) == 3 && strings.HasPrefix(mnc, "0") {
		return prefix + mnc[1:] + suffix
	}
	if len(mnc) == 2 {
		return prefix + "0" + mnc + suffix
	}
	return ""
}

func resolveEPDGWithECS(
	ctx context.Context,
	client *http.Client,
	endpoint, host, subnet string,
) ([]net.IPAddr, error) {
	return resolveEPDGWithECSType(ctx, client, endpoint, host, subnet, "A")
}
func resolveEPDGWithECSType(ctx context.Context, client *http.Client, endpoint, host, subnet, kind string) ([]net.IPAddr, error) {
	if kind != "A" && kind != "AAAA" {
		return nil, errors.New("unsupported DNS address family")
	}
	if client == nil {
		return nil, errors.New("nil DNS-over-HTTPS client")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse DNS-over-HTTPS endpoint: %w", err)
	}
	query := parsed.Query()
	query.Set("name", strings.TrimSpace(host))
	query.Set("type", kind)
	if strings.TrimSpace(subnet) != "" {
		query.Set("edns_client_subnet", strings.TrimSpace(subnet))
	}
	parsed.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build DNS-over-HTTPS request: %w", err)
	}
	request.Header.Set("Accept", "application/dns-json")
	request.Header.Set("Cache-Control", "no-cache")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query DNS-over-HTTPS: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DNS-over-HTTPS returned HTTP %d", response.StatusCode)
	}
	var payload dnsOverHTTPSResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode DNS-over-HTTPS response: %w", err)
	}
	if payload.Status != 0 {
		return nil, fmt.Errorf("DNS-over-HTTPS returned DNS status %d", payload.Status)
	}
	result := make([]net.IPAddr, 0, len(payload.Answer))
	for _, answer := range payload.Answer {
		if kind == "A" && answer.Type != 1 || kind == "AAAA" && answer.Type != 28 {
			continue
		}
		ip := net.ParseIP(strings.TrimSuffix(strings.TrimSpace(answer.Data), "."))
		if !vowifi.UsableEPDGAddress(ip) || kind == "A" && ip.To4() == nil || kind == "AAAA" && ip.To4() != nil {
			continue
		}
		duplicate := false
		for _, existing := range result {
			if existing.IP.Equal(ip) {
				duplicate = true
				break
			}
		}
		if !duplicate && len(result) < 16 {
			result = append(result, net.IPAddr{IP: append(net.IP(nil), ip...)})
		}
	}
	if len(result) == 0 {
		return nil, errors.New("DNS-over-HTTPS response contained no ePDG IP addresses")
	}
	return result, nil
}
