// Package smsapi owns an optional, independent read-only HTTP listener. Its
// Bearer credential never authenticates an administrator or a carrier action.
package smsapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vocat/internal/smsquery"
	"vocat/internal/store"
)

const SettingKey = "sms.api.private.v1"

type Settings struct {
	Version     int    `json:"version"`
	Enabled     bool   `json:"enabled"`
	BindAddress string `json:"bind_address"`
	Port        int    `json:"port"`
	AllowLAN    bool   `json:"allow_lan_access"`
	TokenHash   string `json:"token_hash,omitempty"`
}
type Update struct {
	Enabled     bool   `json:"enabled"`
	BindAddress string `json:"bind_address"`
	Port        int    `json:"port"`
	AllowLAN    bool   `json:"allow_lan_access"`
	Token       string `json:"token,omitempty"`
}
type Status struct {
	Enabled         bool   `json:"enabled"`
	BindAddress     string `json:"bind_address"`
	Port            int    `json:"port"`
	AllowLAN        bool   `json:"allow_lan_access"`
	TokenConfigured bool   `json:"token_configured"`
	Running         bool   `json:"running"`
	Error           string `json:"error,omitempty"`
	BaseURL         string `json:"base_url"`
}
type Repository interface {
	AppSetting(context.Context, string) (store.AppSetting, error)
	UpsertAppSetting(context.Context, store.AppSetting) error
	QueryReadOnlySMS(context.Context, smsquery.Query) (smsquery.Result, error)
}
type Service struct {
	mu       sync.Mutex
	database Repository
	settings Settings
	snapshot *snapshot
	server   *http.Server
	listener net.Listener
	cancel   context.CancelFunc
	error    string
	closed   bool
}

func defaults() Settings { return Settings{Version: 1, BindAddress: "127.0.0.1", Port: 8787} }
func private(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}
func (s Settings) endpoint() (netip.AddrPort, error) {
	ip, err := netip.ParseAddr(s.BindAddress)
	if err != nil || !private(ip) || ip.Is4In6() || s.Port < 1024 || s.Port > 65535 || s.Version != 1 {
		return netip.AddrPort{}, errors.New("use a specific loopback/private IP and port 1024–65535")
	}
	if !ip.IsLoopback() && !s.AllowLAN {
		return netip.AddrPort{}, errors.New("confirm LAN/VPN access first")
	}
	digest, err := hex.DecodeString(s.TokenHash)
	if s.TokenHash != "" && (err != nil || len(digest) != sha256.Size) {
		return netip.AddrPort{}, errors.New("invalid token digest")
	}
	if s.Enabled && s.TokenHash == "" {
		return netip.AddrPort{}, errors.New("configure an independent Token before enabling")
	}
	return netip.AddrPortFrom(ip, uint16(s.Port)), nil
}
func New(ctx context.Context, database Repository) *Service {
	s := &Service{database: database, settings: defaults()}
	value, err := database.AppSetting(ctx, SettingKey)
	if errors.Is(err, store.ErrNotFound) {
		return s
	}
	if err == nil {
		decoder := json.NewDecoder(strings.NewReader(string(value.Value)))
		decoder.DisallowUnknownFields()
		var saved Settings
		if err = decoder.Decode(&saved); err == nil {
			var trailing any
			if decoder.Decode(&trailing) != io.EOF {
				err = errors.New("trailing settings")
			}
		}
		if err == nil {
			_, err = saved.endpoint()
		}
		if err == nil {
			s.settings = saved
			s.bindLocked()
			return s
		}
	}
	s.error = "API settings could not be loaded; API remains off. Save settings to recover."
	return s
}
func (s *Service) Status() Status { s.mu.Lock(); defer s.mu.Unlock(); return s.statusLocked() }
func (s *Service) statusLocked() Status {
	endpoint, _ := s.settings.endpoint()
	base := ""
	if endpoint.IsValid() {
		base = "http://" + endpoint.String() + "/"
	}
	running := s.snapshot != nil && s.snapshot.active.Load()
	err := s.error
	if s.settings.Enabled && !running && err == "" && !s.closed {
		err = "API listener stopped; save settings to retry."
	}
	return Status{s.settings.Enabled, s.settings.BindAddress, s.settings.Port, s.settings.AllowLAN, s.settings.TokenHash != "", running, err, base}
}
func (s *Service) Apply(ctx context.Context, u Update) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.statusLocked(), errors.New("API service is closed")
	}
	next := Settings{Version: 1, Enabled: u.Enabled, BindAddress: u.BindAddress, Port: u.Port, AllowLAN: u.AllowLAN, TokenHash: s.settings.TokenHash}
	if u.Token != "" {
		if len(u.Token) < 32 || len(u.Token) > 128 || strings.ContainsFunc(u.Token, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
		}) {
			return s.statusLocked(), errors.New("Token must contain 32–128 ASCII letters, digits, underscores or hyphens")
		}
		digest := sha256.Sum256([]byte(u.Token))
		next.TokenHash = hex.EncodeToString(digest[:])
	}
	if _, err := next.endpoint(); err != nil {
		return s.statusLocked(), err
	}
	raw, _ := json.Marshal(next)
	// A failed durable save leaves both listener and credential unchanged.
	if err := s.database.UpsertAppSetting(ctx, store.AppSetting{Key: SettingKey, Value: raw, Sensitive: true}); err != nil {
		return s.statusLocked(), errors.New("API settings could not be saved; previous settings remain active")
	}
	s.stopLocked()
	s.settings = next
	s.error = ""
	s.bindLocked()
	return s.statusLocked(), nil
}
func (s *Service) stopLocked() {
	if s.snapshot != nil {
		s.snapshot.active.Store(false)
	}
	if s.cancel != nil {
		s.cancel()
	}
	// Close our original listener synchronously, even if the Serve goroutine
	// has not registered it with net/http yet. Server.Close alone has a startup
	// race that can briefly retain the port after Apply(false) or Close returns.
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}
	if s.server != nil {
		_ = s.server.Close()
	}
	s.snapshot, s.server, s.cancel = nil, nil, nil
}
func (s *Service) Close() { s.mu.Lock(); defer s.mu.Unlock(); s.closed = true; s.stopLocked() }
func (s *Service) bindLocked() {
	if !s.settings.Enabled {
		return
	}
	endpoint, err := s.settings.endpoint()
	if err != nil {
		s.error = "Invalid saved API settings."
		return
	}
	network := "tcp4"
	if endpoint.Addr().Is6() {
		network = "tcp6"
	}
	listener, err := net.Listen(network, endpoint.String())
	if err != nil {
		s.error = "API listen failed. Check the local IP and port; no firewall changes were made."
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	digest, _ := hex.DecodeString(s.settings.TokenHash)
	p := &snapshot{database: s.database, endpoint: endpoint, capacity: make(chan struct{}, 8), tokenHash: digest}
	p.active.Store(true)
	server := &http.Server{Handler: p, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 7 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }, ErrorLog: log.New(io.Discard, "", 0)}
	s.server, s.snapshot, s.cancel, s.listener = server, p, cancel, listener
	go func() { _ = server.Serve(listener); p.active.Store(false) }()
}

type snapshot struct {
	database  Repository
	endpoint  netip.AddrPort
	tokenHash []byte
	active    atomic.Bool
	mu        sync.Mutex
	window    time.Time
	requests  int
	capacity  chan struct{}
}

func (p *snapshot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fail := func(status int, code string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	if !p.active.Load() {
		fail(503, "disabled")
		return
	}
	now := time.Now().UTC()
	p.mu.Lock()
	if p.window.IsZero() || now.Sub(p.window) >= time.Minute {
		p.window = now
		p.requests = 0
	}
	p.requests++
	limited := p.requests > 120
	retry := max(1, 60-int(now.Sub(p.window).Seconds()))
	p.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		fail(429, "rate_limited")
		return
	}
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	_, origin := r.Header["Origin"]
	if err != nil || !private(remote.Addr()) || !ok || local.String() != p.endpoint.String() || origin || r.Host != p.endpoint.String() {
		fail(403, "access_denied")
		return
	}
	authorizations := r.Header.Values("Authorization")
	if len(authorizations) != 1 || len(authorizations[0]) < 7 || len(authorizations[0]) > 256 || !strings.EqualFold(authorizations[0][:7], "Bearer ") {
		fail(401, "unauthorized")
		return
	}
	digest := sha256.Sum256([]byte(authorizations[0][7:]))
	if subtle.ConstantTimeCompare(digest[:], p.tokenHash) != 1 {
		fail(401, "unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		fail(405, "read_only_get_required")
		return
	}
	if len(r.RequestURI) > 2048 || !strings.HasPrefix(r.RequestURI, "/") || strings.Contains(r.RequestURI, "#") {
		fail(400, "invalid_query")
		return
	}
	path, raw, _ := strings.Cut(r.RequestURI, "?")
	if path != "/api/v1/sms" && path != "/api/v1/otp" {
		fail(404, "not_found")
		return
	}
	q, err := smsquery.Parse(raw, path == "/api/v1/otp", now)
	if err != nil {
		fail(400, "invalid_query")
		return
	}
	select {
	case p.capacity <- struct{}{}:
		defer func() { <-p.capacity }()
	default:
		fail(503, "temporarily_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := p.database.QueryReadOnlySMS(ctx, q)
	if err != nil || ctx.Err() != nil || !p.active.Load() {
		fail(503, "temporarily_unavailable")
		return
	}
	if path == "/api/v1/otp" {
		var message *smsquery.Message
		var code *string
		if len(result.Messages) > 0 {
			message = &result.Messages[0]
			code = message.Code
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"found": message != nil, "code": code, "message": message, "scanLimited": result.ScanLimited, "generatedAtUtc": now, "codeExtraction": "heuristic"})
	} else {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": result.Messages, "count": len(result.Messages), "scanLimited": result.ScanLimited, "generatedAtUtc": now})
	}
}
