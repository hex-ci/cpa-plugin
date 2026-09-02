package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

type proxyMode uint8

const (
	proxyModeInherit proxyMode = iota
	proxyModeExplicit
	proxyModeBlocked
)

type proxyRoutingState struct {
	mode   proxyMode
	client *http.Client
}

var proxyState atomic.Pointer[proxyRoutingState]

func init() {
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
}

func currentProxyState() *proxyRoutingState {
	return proxyState.Load()
}

func configureProxy(raw string) error {
	setting, err := proxyutil.Parse(raw)
	if err != nil {
		proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
		return errors.New("invalid proxy-url")
	}
	if setting.Mode == proxyutil.ModeInherit {
		proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
		return nil
	}
	if setting.Mode != proxyutil.ModeProxy {
		proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
		return errors.New("proxy-url must use http, https, socks5, or socks5h")
	}
	transport, err := buildPluginProxyTransport(setting)
	if err != nil {
		proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
		return errors.New("invalid proxy-url transport")
	}
	proxyState.Store(&proxyRoutingState{
		mode: proxyModeExplicit,
		client: &http.Client{
			Timeout: 120 * time.Second,
			Transport: safeProxyRoundTripper{
				base:     transport,
				endpoint: proxyutil.Redact(setting.Raw),
			},
		},
	})
	return nil
}

type contextProxyDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

func buildPluginProxyTransport(setting proxyutil.Setting) (*http.Transport, error) {
	transport, mode, err := proxyutil.BuildHTTPTransport(setting.Raw)
	if err != nil || mode != proxyutil.ModeProxy || transport == nil {
		return nil, errors.New("build proxy transport failed")
	}
	if setting.URL.Scheme != "socks5" && setting.URL.Scheme != "socks5h" {
		return transport, nil
	}
	dialer, dialerMode, err := proxyutil.BuildDialer(setting.Raw)
	if err != nil || dialerMode != proxyutil.ModeProxy || dialer == nil {
		return nil, errors.New("build SOCKS proxy dialer failed")
	}
	contextDialer, ok := dialer.(contextProxyDialer)
	if !ok {
		return nil, errors.New("SOCKS proxy dialer does not support context cancellation")
	}
	transport.DialContext = contextDialer.DialContext
	return transport, nil
}

type safeProxyRoundTripper struct {
	base     http.RoundTripper
	endpoint string
}

func (t safeProxyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if contextErr := req.Context().Err(); contextErr != nil {
		if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		return nil, contextErr
	}
	return nil, &proxyRequestError{endpoint: t.endpoint, cause: err}
}

type proxyRequestError struct {
	endpoint string
	cause    error
}

func (e *proxyRequestError) Error() string {
	if e.endpoint == "" {
		return "proxy request failed"
	}
	return "proxy request via " + e.endpoint + " failed"
}

func (e *proxyRequestError) Unwrap() error {
	return e.cause
}

func blockedProxyError() error {
	return errors.New("HTTP requests blocked by invalid proxy-url configuration")
}
