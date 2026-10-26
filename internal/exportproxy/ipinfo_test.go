package exportproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type publicIPTransportFunc func(*http.Request) (*http.Response, error)

func (f publicIPTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type publicIPTestBody struct {
	io.Reader
	closed     *atomic.Bool
	afterClose func()
}

func (b publicIPTestBody) Close() error {
	b.closed.Store(true)
	if b.afterClose != nil {
		b.afterClose()
	}
	return nil
}

func TestPublicIPLookupRacesFamiliesAndJoinsLoser(t *testing.T) {
	for _, winner := range []string{"ipinfo.io", "v6.ipinfo.io"} {
		t.Run(winner, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := make(chan struct{}, 2)
			bothStarted := make(chan struct{})
			canceled := make(chan struct{})
			allowExit := make(chan struct{})
			defer func() {
				select {
				case <-allowExit:
				default:
					close(allowExit)
				}
			}()
			var bodyClosed atomic.Bool
			transport := publicIPTransportFunc(func(r *http.Request) (*http.Response, error) {
				started <- struct{}{}
				if r.URL.Host == winner {
					select {
					case <-bothStarted:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					return &http.Response{StatusCode: 200, Body: publicIPTestBody{
						Reader: strings.NewReader(`{"ip":"2001:db8::8","country":"GB"}`), closed: &bodyClosed,
					}}, nil
				}
				<-r.Context().Done()
				close(canceled)
				<-allowExit
				return nil, r.Context().Err()
			})
			done := make(chan error, 1)
			go func() {
				info, err := lookupPublicIP(ctx, "owned-test-interface", transport)
				if err == nil && (info.IP != "2001:db8::8" || info.CountryCode != "GB") {
					err = errors.New("wrong winning response")
				}
				done <- err
			}()
			for range 2 {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("both endpoints did not start")
				}
			}
			close(bothStarted)
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("losing request was not canceled")
			}
			select {
			case <-done:
				t.Fatal("lookup returned before the losing request exited")
			case <-time.After(25 * time.Millisecond):
			}
			close(allowExit)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("lookup did not join the canceled request")
			}
			if !bodyClosed.Load() {
				t.Fatal("winning response body was not closed")
			}
		})
	}
}

func TestPublicIPLookupKeepsOtherFamilyAfterInvalidResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invalidClosed := make(chan struct{})
	var bodyClosed atomic.Bool
	transport := publicIPTransportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "ipinfo.io" {
			return &http.Response{StatusCode: 200, Body: publicIPTestBody{
				Reader: strings.NewReader(`{"ip":"invalid","country":"GB"}`), closed: &bodyClosed,
				afterClose: func() { close(invalidClosed) },
			}}, nil
		}
		select {
		case <-invalidClosed:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ip":"2001:db8::9","country":"GB"}`))}, nil
	})
	info, err := lookupPublicIP(ctx, "owned-test-interface", transport)
	if err != nil || info.IP != "2001:db8::9" {
		t.Fatalf("lookup = %+v, %v", info, err)
	}
}

func TestPublicIPLookupParentCancellationJoinsBothFamilies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 2)
	var finished atomic.Int32
	transport := publicIPTransportFunc(func(r *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-r.Context().Done()
		finished.Add(1)
		return nil, r.Context().Err()
	})
	done := make(chan error, 1)
	go func() {
		_, err := lookupPublicIP(ctx, "owned-test-interface", transport)
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("both endpoints did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || finished.Load() != 2 {
			t.Fatalf("cancellation = %v, exited requests = %d", err, finished.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parent cancellation did not finish both requests")
	}
}

func TestDecodePublicIPInfo(t *testing.T) {
	info, err := decodePublicIPInfo(strings.NewReader(`{"ip":"203.0.113.8","city":"London","region":"England","country":"gb","org":"AS64500 Test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.IP != "203.0.113.8" || info.CountryCode != "GB" || info.Region != "England" || info.City != "London" {
		t.Fatalf("info = %+v", info)
	}
}

func TestDecodePublicIPInfoRejectsInvalidResponse(t *testing.T) {
	if _, err := decodePublicIPInfo(strings.NewReader(`{"ip":"not-an-ip","country":"GB"}`)); err == nil {
		t.Fatal("invalid IP was accepted")
	}
}
