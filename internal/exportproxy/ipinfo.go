package exportproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

var ipInfoURLs = [...]string{"https://ipinfo.io/json", "https://v6.ipinfo.io/json"}

type PublicIPInfo struct {
	IP           string `json:"ip"`
	CountryCode  string `json:"country_code"`
	Region       string `json:"region"`
	City         string `json:"city"`
	Organization string `json:"organization,omitempty"`
}

// LookupPublicIP sends the lookup through the same marked, interface-bound
// dialer and isolated DNS resolver as Export Proxy. It therefore reports the
// modem's roaming exit rather than the host or browser's default connection.
func LookupPublicIP(ctx context.Context, networkInterface string) (PublicIPInfo, error) {
	networkInterface = strings.TrimSpace(networkInterface)
	if networkInterface == "" {
		return PublicIPInfo{}, errors.New("cellular network interface is required")
	}
	if err := platformSupported(); err != nil {
		return PublicIPInfo{}, err
	}
	dialer := boundDialer(networkInterface)
	resolver := boundResolver(networkInterface)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return dialTarget(ctx, address, &dialer, resolver)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 12 * time.Second,
	}
	defer transport.CloseIdleConnections()
	return lookupPublicIP(ctx, networkInterface, transport)
}

func lookupPublicIP(ctx context.Context, networkInterface string, transport http.RoundTripper) (PublicIPInfo, error) {
	// The legacy API has distinct IPv4 and IPv6 endpoints. The host's default
	// route says nothing about the marked modem session. Race both endpoints
	// within the same lease; cancel and join the loser before releasing it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		info PublicIPInfo
		err  error
	}
	results := make(chan result, len(ipInfoURLs))
	for _, endpoint := range ipInfoURLs {
		go func() {
			info, err := queryPublicIP(ctx, endpoint, networkInterface, transport)
			results <- result{info: info, err: err}
		}()
	}
	var selected PublicIPInfo
	var failures []error
	for range ipInfoURLs {
		r := <-results
		if r.err != nil {
			failures = append(failures, r.err)
		} else if selected.IP == "" {
			selected = r.info
			cancel()
		}
	}
	if selected.IP != "" {
		return selected, nil
	}
	return PublicIPInfo{}, errors.Join(failures...)
}

func queryPublicIP(ctx context.Context, endpoint, networkInterface string, transport http.RoundTripper) (PublicIPInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return PublicIPInfo{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "VoCat/1.0")
	response, err := transport.RoundTrip(request)
	if err != nil {
		return PublicIPInfo{}, fmt.Errorf("query %s through %s: %w", request.URL.Host, networkInterface, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return PublicIPInfo{}, fmt.Errorf("%s returned HTTP %d", request.URL.Host, response.StatusCode)
	}
	return decodePublicIPInfo(io.LimitReader(response.Body, 64<<10))
}

func decodePublicIPInfo(reader io.Reader) (PublicIPInfo, error) {
	var response struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
		Region  string `json:"region"`
		City    string `json:"city"`
		Org     string `json:"org"`
	}
	if err := json.NewDecoder(reader).Decode(&response); err != nil {
		return PublicIPInfo{}, fmt.Errorf("decode ipinfo.io response: %w", err)
	}
	response.IP = strings.TrimSpace(response.IP)
	response.Country = strings.ToUpper(strings.TrimSpace(response.Country))
	if net.ParseIP(response.IP) == nil {
		return PublicIPInfo{}, errors.New("ipinfo.io response contained no valid IP address")
	}
	if len(response.Country) != 2 {
		return PublicIPInfo{}, errors.New("ipinfo.io response contained no valid country code")
	}
	return PublicIPInfo{
		IP: response.IP, CountryCode: response.Country,
		Region: strings.TrimSpace(response.Region), City: strings.TrimSpace(response.City),
		Organization: strings.TrimSpace(response.Org),
	}, nil
}
