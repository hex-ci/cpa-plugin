package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestConfigureProxyPublishesSupportedAndBlockedStates(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	for _, raw := range []string{"", "   "} {
		if err := configureProxy(raw); err != nil {
			t.Fatalf("configureProxy(%q): %v", raw, err)
		}
		if got := currentProxyState().mode; got != proxyModeInherit {
			t.Fatalf("configureProxy(%q) mode = %v, want inherit", raw, got)
		}
	}

	for _, raw := range []string{
		"http://127.0.0.1:8080",
		"https://127.0.0.1:8443",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
	} {
		if err := configureProxy(raw); err != nil {
			t.Fatalf("configureProxy(%q): %v", raw, err)
		}
		state := currentProxyState()
		if state.mode != proxyModeExplicit || state.client == nil {
			t.Fatalf("configureProxy(%q) state = %#v, want explicit client", raw, state)
		}
	}

	for _, raw := range []string{
		"direct",
		"none",
		"127.0.0.1:8080",
		"http:///missing-host",
		"ftp://user:secret@proxy.example/path",
		"http://user:secret%@proxy.example:8080",
	} {
		if err := configureProxy(raw); err == nil {
			t.Fatalf("configureProxy(%q) succeeded, want rejection", raw)
		} else if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "user") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("configureProxy(%q) exposed proxy URL or credentials: %q", raw, err)
		}
		if got := currentProxyState().mode; got != proxyModeBlocked {
			t.Fatalf("configureProxy(%q) mode = %v, want blocked", raw, got)
		}
	}
}

func TestInvalidReconfigureBlocksPreviousProxyBeforeReturning(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	if err := configureProxy("http://127.0.0.1:8080"); err != nil {
		t.Fatal(err)
	}
	raw := mustJSON(map[string]any{
		"config_yaml": []byte("proxy-url: ftp://user:secret@proxy.example/path\nusage_report_url: http://127.0.0.1:1"),
	})
	_, err := handleMethod(pluginabi.MethodPluginReconfigure, raw)
	if err == nil {
		t.Fatal("invalid reconfigure succeeded")
	}
	if strings.Contains(err.Error(), "user") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "proxy.example") {
		t.Fatalf("reconfigure error exposed proxy URL or credentials: %q", err)
	}
	if got := currentProxyState().mode; got != proxyModeBlocked {
		t.Fatalf("state after invalid reconfigure = %v, want blocked", got)
	}
}

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

func testHTTPResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestHostHTTPHelpersRouteByProxyStateWithoutFallback(t *testing.T) {
	oldState := currentProxyState()
	oldShared := sharedHTTPClient()
	t.Cleanup(func() {
		proxyState.Store(oldState)
		sharedClient = oldShared
	})

	sharedCalls := 0
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sharedCalls++
		return testHTTPResponse(req, "inherit"), nil
	})}
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})

	buffered, err := hostHTTPDo(mustRequest(t, http.MethodGet, "http://upstream.invalid/inherit-buffered", nil))
	if err != nil || string(buffered.Body) != "inherit" {
		t.Fatalf("inherit buffered response = %#v, %v", buffered, err)
	}
	stream, _, _, err := hostHTTPDoStream(mustRequest(t, http.MethodGet, "http://upstream.invalid/inherit-stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	chunk, _, err := stream.Read()
	stream.Close()
	if err != nil || string(chunk) != "inherit" || sharedCalls != 2 {
		t.Fatalf("inherit stream chunk = %q, err = %v, shared calls = %d", chunk, err, sharedCalls)
	}

	proxyCalls := 0
	proxyClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		proxyCalls++
		return testHTTPResponse(req, "explicit"), nil
	})}
	proxyState.Store(&proxyRoutingState{mode: proxyModeExplicit, client: proxyClient})
	buffered, err = hostHTTPDo(mustRequest(t, http.MethodGet, "http://upstream.invalid/proxy-buffered", nil))
	if err != nil || string(buffered.Body) != "explicit" {
		t.Fatalf("explicit buffered response = %#v, %v", buffered, err)
	}
	stream, _, _, err = hostHTTPDoStream(mustRequest(t, http.MethodGet, "http://upstream.invalid/proxy-stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	chunk, _, err = stream.Read()
	stream.Close()
	if err != nil || string(chunk) != "explicit" || proxyCalls != 2 || sharedCalls != 2 {
		t.Fatalf("explicit stream chunk = %q, err = %v, proxy calls = %d, shared calls = %d", chunk, err, proxyCalls, sharedCalls)
	}

	proxyClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		proxyCalls++
		return nil, errors.New("proxy unavailable")
	})
	if _, err = hostHTTPDo(mustRequest(t, http.MethodPost, "http://upstream.invalid/no-replay", strings.NewReader("body"))); err == nil {
		t.Fatal("explicit proxy failure unexpectedly succeeded")
	}
	if sharedCalls != 2 {
		t.Fatalf("explicit proxy failure fell back to inherited client; shared calls = %d", sharedCalls)
	}

	proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
	body := &closeTrackingBody{Reader: strings.NewReader("blocked")}
	if _, err = hostHTTPDo(mustRequest(t, http.MethodPost, "http://upstream.invalid/blocked", body)); err == nil {
		t.Fatal("blocked buffered request unexpectedly succeeded")
	}
	if !body.closed || proxyCalls != 3 || sharedCalls != 2 {
		t.Fatalf("blocked buffered request: body closed = %v, proxy calls = %d, shared calls = %d", body.closed, proxyCalls, sharedCalls)
	}
	streamBody := &closeTrackingBody{Reader: strings.NewReader("blocked")}
	if _, _, _, err = hostHTTPDoStream(mustRequest(t, http.MethodPost, "http://upstream.invalid/blocked-stream", streamBody)); err == nil {
		t.Fatal("blocked stream request unexpectedly succeeded")
	}
	if !streamBody.closed || proxyCalls != 3 || sharedCalls != 2 {
		t.Fatalf("blocked stream request: body closed = %v, proxy calls = %d, shared calls = %d", streamBody.closed, proxyCalls, sharedCalls)
	}
}

func mustRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestHTTPProxyActuallyRoutesBufferedAndStreamingRequests(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	var mu sync.Mutex
	var requests []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.String())
		mu.Unlock()
		w.Header().Set("X-Via-Proxy", "yes")
		_, _ = io.WriteString(w, "proxied:"+r.URL.Path)
	}))
	t.Cleanup(proxy.Close)
	if err := configureProxy(proxy.URL); err != nil {
		t.Fatal(err)
	}

	buffered, err := hostHTTPDo(mustRequest(t, http.MethodPost, "http://origin.invalid/buffered", strings.NewReader("payload")))
	if err != nil {
		t.Fatal(err)
	}
	if string(buffered.Body) != "proxied:/buffered" || buffered.Headers.Get("X-Via-Proxy") != "yes" {
		t.Fatalf("buffered response = %#v", buffered)
	}
	stream, _, headers, err := hostHTTPDoStream(mustRequest(t, http.MethodGet, "http://origin.invalid/stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	streamBody, err := io.ReadAll(newHostStreamReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if string(streamBody) != "proxied:/stream" || headers.Get("X-Via-Proxy") != "yes" {
		t.Fatalf("stream response = %q, headers = %v", streamBody, headers)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"POST http://origin.invalid/buffered", "GET http://origin.invalid/stream"}
	if len(requests) != len(want) {
		t.Fatalf("proxy requests = %v, want %v", requests, want)
	}
	for i := range want {
		if requests[i] != want[i] {
			t.Fatalf("proxy request[%d] = %q, want %q", i, requests[i], want[i])
		}
	}
}

func TestConfiguredProxyStreamReturnsAfterFlushBeforeEOF(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "second")
	}))
	t.Cleanup(proxy.Close)
	if err := configureProxy(proxy.URL); err != nil {
		t.Fatal(err)
	}

	type openResult struct {
		stream *hostHTTPStream
		err    error
	}
	opened := make(chan openResult, 1)
	req := mustRequest(t, http.MethodGet, "http://origin.invalid/flush", nil)
	go func() {
		stream, _, _, err := hostHTTPDoStream(req)
		opened <- openResult{stream: stream, err: err}
	}()
	var stream *hostHTTPStream
	select {
	case result := <-opened:
		if result.err != nil {
			t.Fatal(result.err)
		}
		stream = result.stream
	case <-time.After(2 * time.Second):
		t.Fatal("hostHTTPDoStream waited for EOF instead of returning after headers")
	}
	defer stream.Close()

	firstRead := make(chan struct {
		chunk []byte
		err   error
	}, 1)
	go func() {
		chunk, _, err := stream.Read()
		firstRead <- struct {
			chunk []byte
			err   error
		}{chunk: chunk, err: err}
	}()
	select {
	case result := <-firstRead:
		if result.err != nil || string(result.chunk) != "first" {
			t.Fatalf("first flushed read = %q, %v", result.chunk, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first flushed bytes were not readable before EOF")
	}

	releaseOnce.Do(func() { close(release) })
	rest, err := io.ReadAll(newHostStreamReader(stream))
	if err != nil || string(rest) != "second" {
		t.Fatalf("remaining stream = %q, %v", rest, err)
	}
}

func TestOAuthClientsKeepProxySnapshotAndIsolatedCookies(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	var mu sync.Mutex
	var proxyARequests, proxyBRequests []string
	proxyA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxyARequests = append(proxyARequests, r.URL.Path+" cookie="+r.Header.Get("Cookie"))
		mu.Unlock()
		if r.URL.Path == "/a-first" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "a", Path: "/"})
		}
		_, _ = io.WriteString(w, "a")
	}))
	t.Cleanup(proxyA.Close)
	proxyB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxyBRequests = append(proxyBRequests, r.URL.Path+" cookie="+r.Header.Get("Cookie"))
		mu.Unlock()
		_, _ = io.WriteString(w, "b")
	}))
	t.Cleanup(proxyB.Close)

	if err := configureProxy(proxyA.URL); err != nil {
		t.Fatal(err)
	}
	clientA, err := newLoginClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := configureProxy(proxyB.URL); err != nil {
		t.Fatal(err)
	}
	clientB, err := newLoginClient()
	if err != nil {
		t.Fatal(err)
	}

	for _, call := range []struct {
		client *http.Client
		path   string
	}{
		{client: clientA, path: "/a-first"},
		{client: clientB, path: "/b-first"},
		{client: clientA, path: "/a-second"},
	} {
		resp, err := call.client.Get("http://oauth-origin.invalid" + call.path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if len(proxyARequests) != 2 || proxyARequests[0] != "/a-first cookie=" || proxyARequests[1] != "/a-second cookie=session=a" {
		t.Fatalf("proxy A requests = %v", proxyARequests)
	}
	if len(proxyBRequests) != 1 || proxyBRequests[0] != "/b-first cookie=" {
		t.Fatalf("proxy B requests = %v", proxyBRequests)
	}

	proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
	if _, err := newLoginClient(); err == nil {
		t.Fatal("blocked configuration created an OAuth client")
	}
}

func TestUsageProbeObeysInheritedExplicitAndBlockedRouting(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	var mu sync.Mutex
	originCalls := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		originCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(origin.Close)
	if err := configureProxy(""); err != nil {
		t.Fatal(err)
	}
	if !probeURL(origin.URL, time.Second) {
		t.Fatal("inherited usage probe did not reach direct origin")
	}

	proxyCalls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		proxyCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(proxy.Close)
	if err := configureProxy(proxy.URL); err != nil {
		t.Fatal(err)
	}
	if !probeURL("http://probe-origin.invalid/usage", time.Second) {
		t.Fatal("explicit proxy usage probe failed")
	}

	deadProxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadProxyURL := deadProxy.URL
	deadProxy.Close()
	if err := configureProxy(deadProxyURL); err != nil {
		t.Fatal(err)
	}
	if probeURL(origin.URL, 250*time.Millisecond) {
		t.Fatal("usage probe fell back to direct after explicit proxy failure")
	}
	proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
	if probeURL(origin.URL, time.Second) {
		t.Fatal("blocked usage probe reached the network")
	}

	mu.Lock()
	defer mu.Unlock()
	if originCalls != 1 || proxyCalls != 1 {
		t.Fatalf("origin calls = %d, proxy calls = %d", originCalls, proxyCalls)
	}
}

func TestConcurrentProxyReconfigureAndRequests(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, "proxied")
	}))
	t.Cleanup(proxy.Close)
	if err := configureProxy(proxy.URL); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errCh := make(chan error, 500)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 100; i++ {
			if i%2 == 0 {
				if err := configureProxy(proxy.URL); err != nil {
					errCh <- fmt.Errorf("valid reconfigure: %w", err)
				}
			} else if err := configureProxy("direct"); err == nil {
				errCh <- errors.New("invalid reconfigure succeeded")
			}
		}
	}()
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 100; i++ {
				req, err := http.NewRequest(http.MethodGet, "http://race-origin.invalid/request", nil)
				if err != nil {
					errCh <- err
					return
				}
				resp, err := hostHTTPDo(req)
				if err != nil {
					if !strings.Contains(err.Error(), "blocked") {
						errCh <- fmt.Errorf("unexpected request error: %w", err)
					}
					continue
				}
				if string(resp.Body) != "proxied" {
					errCh <- fmt.Errorf("response body = %q", resp.Body)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestSOCKS5AndSOCKS5HActuallyRouteBufferedAndStreamingRequests(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	var originMu sync.Mutex
	var originPaths []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originMu.Lock()
		originPaths = append(originPaths, r.URL.Path)
		originMu.Unlock()
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, "origin:"+r.URL.Path)
	}))
	t.Cleanup(origin.Close)
	_, port, err := net.SplitHostPort(origin.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proxy := newSOCKS5TestProxy(t, "socks-target.test", origin.Listener.Addr().String())
	targetBase := "http://socks-target.test:" + port

	for _, scheme := range []string{"socks5", "socks5h"} {
		before := proxy.connectCount()
		if err := configureProxy(scheme + "://" + proxy.listener.Addr().String()); err != nil {
			t.Fatalf("configure %s: %v", scheme, err)
		}
		buffered, err := hostHTTPDo(mustRequest(t, http.MethodGet, targetBase+"/"+scheme+"/buffered", nil))
		if err != nil {
			t.Fatalf("%s buffered: %v", scheme, err)
		}
		if got := string(buffered.Body); got != "origin:/"+scheme+"/buffered" {
			t.Fatalf("%s buffered body = %q", scheme, got)
		}
		stream, _, _, err := hostHTTPDoStream(mustRequest(t, http.MethodGet, targetBase+"/"+scheme+"/stream", nil))
		if err != nil {
			t.Fatalf("%s stream: %v", scheme, err)
		}
		streamBody, err := io.ReadAll(newHostStreamReader(stream))
		stream.Close()
		if err != nil {
			t.Fatalf("%s stream read: %v", scheme, err)
		}
		if got := string(streamBody); got != "origin:/"+scheme+"/stream" {
			t.Fatalf("%s stream body = %q", scheme, got)
		}
		if after := proxy.connectCount(); after <= before {
			t.Fatalf("%s made no SOCKS5 CONNECT", scheme)
		}
	}

	originMu.Lock()
	defer originMu.Unlock()
	wantPaths := []string{"/socks5/buffered", "/socks5/stream", "/socks5h/buffered", "/socks5h/stream"}
	if len(originPaths) != len(wantPaths) {
		t.Fatalf("origin paths = %v, want %v", originPaths, wantPaths)
	}
	for i := range wantPaths {
		if originPaths[i] != wantPaths[i] {
			t.Fatalf("origin path[%d] = %q, want %q", i, originPaths[i], wantPaths[i])
		}
	}
}

type socks5TestProxy struct {
	listener   net.Listener
	targetHost string
	targetAddr string

	mu       sync.Mutex
	closed   bool
	conns    map[net.Conn]struct{}
	connects []string
	wg       sync.WaitGroup
}

func newSOCKS5TestProxy(t *testing.T, targetHost, targetAddr string) *socks5TestProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &socks5TestProxy{
		listener:   listener,
		targetHost: targetHost,
		targetAddr: targetAddr,
		conns:      make(map[net.Conn]struct{}),
	}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(p.close)
	return p
}

func (p *socks5TestProxy) accept() {
	defer p.wg.Done()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = conn.Close()
			return
		}
		p.conns[conn] = struct{}{}
		p.wg.Add(1)
		p.mu.Unlock()
		go p.handle(conn)
	}
}

func (p *socks5TestProxy) handle(conn net.Conn) {
	defer p.wg.Done()
	defer func() {
		_ = conn.Close()
		p.mu.Lock()
		delete(p.conns, conn)
		p.mu.Unlock()
	}()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil || greeting[0] != 5 {
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 || header[1] != 1 {
		return
	}
	host, err := readSOCKS5Host(conn, header[3])
	if err != nil {
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return
	}
	requested := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes))))
	p.mu.Lock()
	p.connects = append(p.connects, requested)
	p.mu.Unlock()
	dialAddr := requested
	if host == p.targetHost {
		dialAddr = p.targetAddr
	}
	upstream, err := net.DialTimeout("tcp", dialAddr, 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	clientDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, conn)
		close(clientDone)
	}()
	_, _ = io.Copy(conn, upstream)
	<-clientDone
}

func readSOCKS5Host(r io.Reader, addressType byte) (string, error) {
	switch addressType {
	case 1:
		address := make([]byte, net.IPv4len)
		_, err := io.ReadFull(r, address)
		return net.IP(address).String(), err
	case 3:
		length := []byte{0}
		if _, err := io.ReadFull(r, length); err != nil {
			return "", err
		}
		address := make([]byte, int(length[0]))
		_, err := io.ReadFull(r, address)
		return string(address), err
	case 4:
		address := make([]byte, net.IPv6len)
		_, err := io.ReadFull(r, address)
		return net.IP(address).String(), err
	default:
		return "", fmt.Errorf("unsupported SOCKS5 address type %d", addressType)
	}
}

func (p *socks5TestProxy) connectCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.connects)
}

func (p *socks5TestProxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	_ = p.listener.Close()
	for conn := range p.conns {
		_ = conn.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func TestProxyRuntimeErrorUsesRedactedEndpoint(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := listener.Addr().String()
	_ = listener.Close()
	proxyURL := "http://alice:supersecret@" + proxyAddr
	if err := configureProxy(proxyURL); err != nil {
		t.Fatal(err)
	}

	_, err = hostHTTPDo(mustRequest(t, http.MethodGet, "http://origin.invalid/runtime-error", nil))
	if err == nil {
		t.Fatal("request through closed proxy unexpectedly succeeded")
	}
	message := err.Error()
	for _, secret := range []string{"alice", "supersecret", proxyURL} {
		if strings.Contains(message, secret) {
			t.Fatalf("runtime error exposed %q: %q", secret, message)
		}
	}
	if want := "http://redacted@" + proxyAddr; !strings.Contains(message, want) {
		t.Fatalf("runtime error = %q, want redacted proxy endpoint %q", message, want)
	}
}

func TestSafeProxyRoundTripperPreservesCauseWithoutExposingIt(t *testing.T) {
	cause := errors.New("dial failed with alice:supersecret")
	rt := safeProxyRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, cause
	})}
	req := mustRequest(t, http.MethodGet, "http://origin.invalid/cause", nil)
	_, err := rt.RoundTrip(req)
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(%v, cause) = false", err)
	}
	if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("safe error exposed cause text: %q", err)
	}
}

func TestSOCKSHandshakeClosesAfterRequestCancellation(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			greetingRead := make(chan struct{})
			peerClosed := make(chan struct{})
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				accepted <- conn
				greeting := make([]byte, 3)
				if _, err := io.ReadFull(conn, greeting); err != nil {
					close(greetingRead)
					close(peerClosed)
					return
				}
				close(greetingRead)
				_, _ = conn.Read(make([]byte, 1))
				_ = conn.Close()
				close(peerClosed)
			}()

			if err := configureProxy(scheme + "://" + listener.Addr().String()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://origin.invalid/hanging-socks", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := hostHTTPDo(req); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("request error = %v, want deadline exceeded", err)
			}

			var conn net.Conn
			select {
			case conn = <-accepted:
			case <-time.After(time.Second):
				t.Fatal("SOCKS proxy accepted no connection")
			}
			select {
			case <-greetingRead:
			case <-time.After(time.Second):
				_ = conn.Close()
				t.Fatal("SOCKS greeting was not sent")
			}
			select {
			case <-peerClosed:
			case <-time.After(time.Second):
				_ = conn.Close()
				t.Fatal("canceled SOCKS handshake left the proxy connection open")
			}
		})
	}
}

func TestBlockedReconfigureStopsExistingOAuthPoll(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	calls := 0
	stateID := "proxy-blocked-existing-login"
	loginStates.Store(stateID, &loginCtx{
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("unexpected network call")
		})},
		expires: time.Now().Add(time.Minute),
	})
	t.Cleanup(func() { loginStates.Delete(stateID) })
	if err := configureProxy("direct"); err == nil {
		t.Fatal("invalid proxy configuration succeeded")
	}

	_, err := handlePollLogin(mustJSON(map[string]any{"state": stateID}))
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("poll error = %v, want blocked proxy error", err)
	}
	if calls != 0 {
		t.Fatalf("blocked OAuth poll made %d network calls", calls)
	}
	if _, ok := loginStates.Load(stateID); ok {
		t.Fatal("blocked OAuth poll retained the login state")
	}
}

func TestPluginOwnedHTTPDoesNotFollowRedirects(t *testing.T) {
	sinkCalls := 0
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sinkCalls++
	}))
	defer sink.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", sink.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	req := mustRequest(t, http.MethodPost, origin.URL, nil)
	req.Header.Set("X-Refresh-Token", "long-lived-secret")
	resp, err := hostHTTPDoWithClient(&http.Client{}, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusTemporaryRedirect)
	}
	if sinkCalls != 0 {
		t.Fatalf("redirect target received %d requests", sinkCalls)
	}
}

func TestExistingOAuthPollUsesCurrentExplicitProxy(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	oldCalls, currentCalls := 0, 0
	stateID := "proxy-current-existing-login"
	loginStates.Store(stateID, &loginCtx{
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			oldCalls++
			return nil, errors.New("stale OAuth transport used")
		})},
		expires: time.Now().Add(time.Minute),
	})
	t.Cleanup(func() { loginStates.Delete(stateID) })
	activeClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		currentCalls++
		if strings.Contains(req.URL.Path, "/auth/token") {
			return testHTTPResponse(req, `{"code":0,"data":{"accessToken":"access","refreshToken":"refresh","expiresIn":3600}}`), nil
		}
		if strings.Contains(req.URL.Path, "/login/account") {
			return testHTTPResponse(req, `{"code":0,"data":{"uid":"uid"}}`), nil
		}
		return nil, fmt.Errorf("unexpected OAuth path %s", req.URL.Path)
	})}
	proxyState.Store(&proxyRoutingState{mode: proxyModeExplicit, client: activeClient})

	if _, err := handlePollLogin(mustJSON(map[string]any{"state": stateID})); err != nil {
		t.Fatal(err)
	}
	if oldCalls != 0 || currentCalls != 2 {
		t.Fatalf("old transport calls = %d, current proxy calls = %d", oldCalls, currentCalls)
	}
}

func TestNonStringProxyConfigFailsClosed(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	if err := configureProxy("http://127.0.0.1:8080"); err != nil {
		t.Fatal(err)
	}
	raw := mustJSON(map[string]any{
		"config_yaml": []byte("proxy-url:\n  address: socks5://127.0.0.1:1080\nusage_report_url: http://127.0.0.1:1\n"),
	})
	if _, err := handleMethod(pluginabi.MethodPluginReconfigure, raw); err == nil {
		t.Fatal("non-string proxy-url reconfigure succeeded")
	}
	if got := currentProxyState().mode; got != proxyModeBlocked {
		t.Fatalf("state = %v, want blocked", got)
	}
	if _, err := hostHTTPDo(mustRequest(t, http.MethodGet, "http://origin.invalid/must-not-send", nil)); err == nil {
		t.Fatal("blocked state allowed a request")
	}
}

func TestUsageProbeRejectsRedirects(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalls++
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()

	if probeURL(origin.URL, time.Second) {
		t.Fatal("redirecting usage endpoint was accepted")
	}
	if targetCalls != 0 {
		t.Fatalf("usage probe followed redirect %d times", targetCalls)
	}
}

func TestOAuthRedirectErrorDoesNotExposeLocation(t *testing.T) {
	old := currentProxyState()
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
	t.Cleanup(func() { proxyState.Store(old) })

	const stateID = "oauth-secret-redirect"
	const secretLocation = "https://example.invalid/callback?access_token=abcdefghijklmnop"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/auth/token") {
			return testHTTPResponse(req, `{"code":0,"data":{"accessToken":"access","refreshToken":"refresh","expiresIn":3600}}`), nil
		}
		resp := testHTTPResponse(req, "")
		resp.StatusCode = http.StatusFound
		resp.Header.Set("Location", secretLocation)
		return resp, nil
	})}
	loginStates.Store(stateID, &loginCtx{client: client, expires: time.Now().Add(time.Minute)})
	t.Cleanup(func() { loginStates.Delete(stateID) })

	_, err := handlePollLogin(mustJSON(map[string]any{"state": stateID}))
	if err == nil {
		t.Fatal("redirecting account lookup unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secretLocation) || strings.Contains(err.Error(), "abcdefghijklmnop") {
		t.Fatalf("OAuth redirect error exposed Location: %q", err)
	}
}

func TestOAuthPollRechecksBlockedStateBeforeAccountLookup(t *testing.T) {
	old := currentProxyState()
	t.Cleanup(func() { proxyState.Store(old) })

	calls := 0
	active := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if strings.Contains(req.URL.Path, "/auth/token") {
			if err := configureProxy("direct"); err == nil {
				return nil, errors.New("blocking reconfigure unexpectedly succeeded")
			}
			return testHTTPResponse(req, `{"code":0,"data":{"accessToken":"access","refreshToken":"refresh","expiresIn":3600}}`), nil
		}
		return testHTTPResponse(req, `{"code":0,"data":{"uid":"uid"}}`), nil
	})}
	proxyState.Store(&proxyRoutingState{mode: proxyModeExplicit, client: active})
	const stateID = "oauth-blocked-between-requests"
	loginStates.Store(stateID, &loginCtx{client: active, expires: time.Now().Add(time.Minute)})
	t.Cleanup(func() { loginStates.Delete(stateID) })

	_, err := handlePollLogin(mustJSON(map[string]any{"state": stateID}))
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("poll error = %v, want blocked proxy error", err)
	}
	if calls != 1 {
		t.Fatalf("OAuth poll made %d requests after blocking between token and account", calls)
	}
	if _, ok := loginStates.Load(stateID); ok {
		t.Fatal("blocked OAuth poll retained the login state")
	}
}
