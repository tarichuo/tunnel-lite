package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"tunnel-lite/internal/agent"
	"tunnel-lite/internal/cert"
	"tunnel-lite/internal/config"
	"tunnel-lite/internal/protocol"
	"tunnel-lite/internal/server"
)

type harness struct {
	t      *testing.T
	cfg    config.Server
	base   string
	client *http.Client
	stop   context.CancelFunc
	done   chan error
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}
func newHarness(t *testing.T, configure ...func(*config.Server)) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := cert.Init(dir, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	c, err := config.LoadServer(filepath.Join(dir, "server.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c.Server.APIPort = freePort(t)
	c.Server.ControlPort = freePort(t)
	c.Server.ControlHost = "127.0.0.1"
	c.PortPool.Start = freePort(t)
	c.PortPool.End = c.PortPool.Start + 5
	c.Heartbeat.Interval = 1
	c.Heartbeat.Timeout = 4
	c.Limits.PairTimeout = 2
	for _, fn := range configure {
		fn(&c)
	}
	h := &harness{t: t, cfg: c, base: fmt.Sprintf("http://127.0.0.1:%d/api/v1/", c.Server.APIPort), client: &http.Client{Timeout: 3 * time.Second}}
	h.start()
	t.Cleanup(h.shutdown)
	return h
}
func (h *harness) start() {
	h.t.Helper()
	s, err := server.New(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- s.Run(ctx) }()
	h.wait(func() bool {
		req, _ := http.NewRequest("GET", h.base+"overview", nil)
		req.Header.Set("Authorization", "Bearer "+h.cfg.Server.AdminToken)
		resp, err := h.client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200
	})
}
func (h *harness) shutdown() {
	if h.stop == nil {
		return
	}
	h.stop()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("server shutdown: %v", err)
		}
	case <-time.After(12 * time.Second):
		h.t.Error("server shutdown timed out")
	}
	h.stop = nil
}
func (h *harness) wait(predicate func() bool) {
	h.t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		if predicate() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatal("timed out waiting for expected state")
}
func (h *harness) call(method, path string, body any, key string) (int, []byte) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			h.t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(method, h.base+path, &buf)
	req.Header.Set("Authorization", "Bearer "+h.cfg.Server.AdminToken)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, data
}
func (h *harness) must(method, path string, body any, status int, out any) {
	h.t.Helper()
	code, data := h.call(method, path, body, "")
	if code != status {
		h.t.Fatalf("%s %s = %d: %s", method, path, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			h.t.Fatal(err)
		}
	}
}
func (h *harness) agent() (config.Agent, context.CancelFunc) {
	h.t.Helper()
	var result struct {
		Agent server.Agent `json:"agent"`
		Token string       `json:"token"`
	}
	h.must("POST", "agents", map[string]any{"name": "integration-agent", "allowed_targets": []string{"127.0.0.1"}}, 201, &result)
	cfg := config.Agent{Server: net.JoinHostPort("127.0.0.1", strconv.Itoa(h.cfg.Server.ControlPort)), ID: result.Agent.ID, Token: result.Token, CA: h.cfg.Server.CACert, AllowedTargets: []string{"127.0.0.1"}}
	return cfg, h.startAgent(cfg)
}
func (h *harness) startAgent(c config.Agent) context.CancelFunc {
	a, err := agent.New(c)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				h.t.Error("Agent shutdown timeout")
			}
		})
	}
	h.t.Cleanup(stop)
	h.wait(func() bool {
		code, b := h.call("GET", "agents/"+c.ID, nil, "")
		return code == 200 && bytes.Contains(b, []byte(`"ONLINE"`))
	})
	return stop
}
func (h *harness) waitTunnel(id, status string) {
	h.t.Helper()
	h.wait(func() bool {
		var detail struct {
			Tunnel server.Tunnel `json:"tunnel"`
		}
		code, b := h.call("GET", "tunnels/"+id, nil, "")
		json.Unmarshal(b, &detail)
		return code == 200 && detail.Tunnel.Status == status
	})
}
func localService(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				body, err := io.ReadAll(c)
				if err == nil {
					c.Write(append([]byte("response:"), body...))
				}
			}()
		}
	}()
	return ln
}
func roundTrip(port int, payload []byte) error {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = c.Write(payload); err != nil {
		return err
	}
	if err = c.(*net.TCPConn).CloseWrite(); err != nil {
		return err
	}
	got, err := io.ReadAll(c)
	if err != nil {
		return err
	}
	want := append([]byte("response:"), payload...)
	if !bytes.Equal(got, want) {
		return fmt.Errorf("payload differs: got %d bytes, want %d", len(got), len(want))
	}
	return nil
}
func TestTunnelLifecycle(t *testing.T) {
	h := newHarness(t)
	cfg, stopAgent := h.agent()
	local := localService(t)
	localPort := local.Addr().(*net.TCPAddr).Port
	body := map[string]any{"agent_id": cfg.ID, "name": "web", "local_address": "127.0.0.1", "local_port": localPort, "remote_port": 0}
	code, data := h.call("POST", "tunnels", body, "create-once")
	if code != 202 {
		t.Fatalf("create %d: %s", code, data)
	}
	var tun server.Tunnel
	if err := json.Unmarshal(data, &tun); err != nil {
		t.Fatal(err)
	}
	code, data = h.call("POST", "tunnels", body, "create-once")
	var duplicate server.Tunnel
	json.Unmarshal(data, &duplicate)
	if code != 200 || duplicate.ID != tun.ID {
		t.Fatalf("idempotency: %d %s", code, data)
	}
	h.waitTunnel(tun.ID, "ACTIVE")
	// The target waits for EOF before responding; this verifies half-close across TLS.
	payload := bytes.Repeat([]byte{0, 1, 255, 123, 10}, 20000)
	if err := roundTrip(tun.PublicPort, payload); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- roundTrip(tun.PublicPort, []byte("parallel TCP")) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	conflict := map[string]any{"agent_id": cfg.ID, "name": "conflict", "local_address": "127.0.0.1", "local_port": localPort, "remote_port": tun.PublicPort}
	h.must("POST", "tunnels", conflict, 409, nil)
	forbidden := map[string]any{"agent_id": cfg.ID, "name": "forbidden", "local_address": "192.168.1.1", "local_port": 80}
	h.must("POST", "tunnels", forbidden, 400, nil)
	// Agent offline retains the logical and OS-level reservation.
	stopAgent()
	h.waitTunnel(tun.ID, "OFFLINE")
	if ln, err := net.Listen("tcp", fmt.Sprintf(":%d", tun.PublicPort)); err == nil {
		ln.Close()
		t.Fatal("offline mapping released port")
	}
	stopAgent = h.startAgent(cfg)
	h.waitTunnel(tun.ID, "ACTIVE")
	if err := roundTrip(tun.PublicPort, []byte("reconnected")); err != nil {
		t.Fatal(err)
	}
	// An active external connection is closed when the mapping is stopped.
	active, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tun.PublicPort))
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	h.must("DELETE", "tunnels/"+tun.ID, nil, 200, nil)
	active.SetReadDeadline(time.Now().Add(3 * time.Second))
	one := make([]byte, 1)
	if _, err = active.Read(one); err == nil {
		t.Fatal("stopped mapping connection remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("stop did not close connection")
	}
	h.must("DELETE", "tunnels/"+tun.ID, nil, 200, nil)
	occupied, err := net.Listen("tcp", fmt.Sprintf(":%d", tun.PublicPort))
	if err != nil {
		t.Fatalf("stop did not release port: %v", err)
	}
	h.must("POST", "tunnels/"+tun.ID+"/start", map[string]int{"remote_port": tun.PublicPort}, 409, nil)
	var restarted server.Tunnel
	h.must("POST", "tunnels/"+tun.ID+"/start", map[string]int{"remote_port": 0}, 202, &restarted)
	occupied.Close()
	if restarted.PublicPort == tun.PublicPort {
		t.Fatal("auto restart silently used occupied port")
	}
	h.waitTunnel(tun.ID, "ACTIVE")
	// Restart Server while Agent remains running. Same identity and port recover from SQLite.
	h.shutdown()
	h.start()
	h.waitTunnel(tun.ID, "ACTIVE")
	if err := roundTrip(restarted.PublicPort, []byte("server restarted")); err != nil {
		t.Fatal(err)
	}
	h.must("DELETE", "tunnels/"+tun.ID, nil, 200, nil)
	stopAgent()
	h.startAgent(cfg)
	h.waitTunnel(tun.ID, "STOPPED")
	h.shutdown()
	h.start()
	h.waitTunnel(tun.ID, "STOPPED")
	// Plaintext Agent tokens never appear in the SQLite database, WAL or SHM.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, _ := os.ReadFile(h.cfg.Database.Path + suffix)
		if bytes.Contains(b, []byte(cfg.Token)) {
			t.Fatal("plaintext Token persisted")
		}
	}
}
func TestWebAuthentication(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("GET", h.base+"agents", nil)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("API allowed unauthenticated access")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	login := fmt.Sprintf(`{"token":%q}`, h.cfg.Server.AdminToken)
	resp, err = client.Post(h.base+"session", "application/json", bytes.NewBufferString(login))
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	json.NewDecoder(resp.Body).Decode(&session)
	resp.Body.Close()
	if resp.StatusCode != 200 || session.CSRF == "" {
		t.Fatal("login failed")
	}
	send := func(csrf, origin string) int {
		req, _ := http.NewRequest("POST", h.base+"agents", bytes.NewBufferString(`{"name":"web-agent"}`))
		req.Header.Set("X-CSRF-Token", csrf)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := send("", ""); code != 403 {
		t.Fatalf("missing CSRF accepted: %d", code)
	}
	if code := send(session.CSRF, "https://evil.invalid"); code != 403 {
		t.Fatalf("foreign Origin accepted: %d", code)
	}
	if code := send(session.CSRF, ""); code != 201 {
		t.Fatalf("authenticated create failed: %d", code)
	}
	req, _ = http.NewRequest("DELETE", h.base+"session", nil)
	req.Header.Set("X-CSRF-Token", session.CSRF)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = client.Get(h.base + "agents")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("logout did not revoke session")
	}
}
func TestTLSAndDataAuthentication(t *testing.T) {
	h := newHarness(t)
	cfg, _ := h.agent()
	ca, _ := os.ReadFile(cfg.CA)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	tlsCfg := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(h.cfg.Server.ControlPort))
	for _, m := range []protocol.Message{{Type: "hello", AgentID: cfg.ID, Token: "wrong"}, {Type: "data", AgentID: cfg.ID, TunnelID: "unknown", ConnectionID: "unknown", Ticket: "wrong"}} {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, tlsCfg)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		if err = protocol.Write(c, m); err != nil {
			t.Fatal(err)
		}
		got, err := protocol.Read(c)
		c.Close()
		if err == nil && got.Type != "error" {
			t.Fatalf("invalid credentials accepted: %+v", got)
		}
	}
	// An untrusted CA must fail before application authentication.
	badRoots := x509.NewCertPool()
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, &tls.Config{RootCAs: badRoots, MinVersion: tls.VersionTLS12})
	if err == nil {
		c.Close()
		t.Fatal("untrusted TLS certificate accepted")
	}
}
func TestAgentLocalPolicy(t *testing.T) {
	h := newHarness(t)
	var result struct {
		Agent server.Agent `json:"agent"`
		Token string       `json:"token"`
	}
	h.must("POST", "agents", map[string]any{"name": "restricted", "allowed_targets": []string{"127.0.0.1", "127.0.0.2"}}, 201, &result)
	cfg := config.Agent{Server: net.JoinHostPort("127.0.0.1", strconv.Itoa(h.cfg.Server.ControlPort)), ID: result.Agent.ID, Token: result.Token, CA: h.cfg.Server.CACert, AllowedTargets: []string{"127.0.0.1"}}
	h.startAgent(cfg)
	var tun server.Tunnel
	h.must("POST", "tunnels", map[string]any{"agent_id": cfg.ID, "name": "blocked locally", "local_address": "127.0.0.2", "local_port": 80}, 202, &tun)
	h.waitTunnel(tun.ID, "ERROR")
	var detail struct {
		Tunnel server.Tunnel `json:"tunnel"`
	}
	h.must("GET", "tunnels/"+tun.ID, nil, 200, &detail)
	if detail.Tunnel.Enabled {
		t.Fatal("failed fresh mapping retained reservation")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", tun.PublicPort))
	if err != nil {
		t.Fatal("failed mapping leaked listener")
	}
	ln.Close()
}

func TestSingleUseDataTicket(t *testing.T) {
	h := newHarness(t)
	var identity struct {
		Agent server.Agent `json:"agent"`
		Token string       `json:"token"`
	}
	h.must("POST", "agents", map[string]any{"name": "manual-agent"}, 201, &identity)
	ca, _ := os.ReadFile(h.cfg.Server.CACert)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	dial := func() net.Conn {
		t.Helper()
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(h.cfg.Server.ControlPort)), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		t.Cleanup(func() { c.Close() })
		return c
	}
	control := dial()
	if err := protocol.Write(control, protocol.Message{Type: "hello", AgentID: identity.Agent.ID, Token: identity.Token}); err != nil {
		t.Fatal(err)
	}
	welcome, err := protocol.Read(control)
	if err != nil || welcome.Type != "welcome" {
		t.Fatal("manual Agent authentication failed", err)
	}
	var tun server.Tunnel
	h.must("POST", "tunnels", map[string]any{"agent_id": identity.Agent.ID, "name": "ticket-test", "local_address": "127.0.0.1", "local_port": 3000}, 202, &tun)
	start, err := protocol.Read(control)
	if err != nil || start.Type != "start" {
		t.Fatal("missing start configuration", err)
	}
	protocol.Write(control, protocol.Message{Type: "started", TunnelID: start.TunnelID, Revision: start.Revision})
	h.waitTunnel(tun.ID, "ACTIVE")
	external, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tun.PublicPort)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	external.SetDeadline(time.Now().Add(3 * time.Second))
	request, err := protocol.Read(control)
	if err != nil || request.Type != "connect" {
		t.Fatal("missing connection request", err)
	}
	frame := protocol.Message{Type: "data", AgentID: identity.Agent.ID, TunnelID: tun.ID, ConnectionID: request.ConnectionID, Ticket: request.Ticket}
	wrong := dial()
	bad := frame
	bad.Ticket = "wrong"
	protocol.Write(wrong, bad)
	if _, err := protocol.Read(wrong); err == nil {
		t.Fatal("invalid ticket accepted")
	}
	wrong.Close()
	data := dial()
	protocol.Write(data, frame)
	paired, err := protocol.Read(data)
	if err != nil || paired.Type != "paired" {
		t.Fatal("correct ticket rejected", err)
	}
	replay := dial()
	protocol.Write(replay, frame)
	if _, err := protocol.Read(replay); err == nil {
		t.Fatal("ticket replay accepted")
	}
	replay.Close()
	if _, err = external.Write([]byte("ticket-bound-payload")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("ticket-bound-payload"))
	if _, err = io.ReadFull(data, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ticket-bound-payload" {
		t.Fatal("incorrect associated stream")
	}
	data.Close()
}

func TestRecoveryPortConflict(t *testing.T) {
	h := newHarness(t)
	cfg, _ := h.agent()
	local := localService(t)
	var tun server.Tunnel
	h.must("POST", "tunnels", map[string]any{"agent_id": cfg.ID, "name": "recovery", "local_address": "127.0.0.1", "local_port": local.Addr().(*net.TCPAddr).Port}, 202, &tun)
	h.waitTunnel(tun.ID, "ACTIVE")
	h.shutdown()
	occupied, err := net.Listen("tcp", fmt.Sprintf(":%d", tun.PublicPort))
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	h.start()
	h.waitTunnel(tun.ID, "ERROR")
	var detail struct {
		Tunnel server.Tunnel `json:"tunnel"`
	}
	h.must("GET", "tunnels/"+tun.ID, nil, 200, &detail)
	if !detail.Tunnel.Enabled || detail.Tunnel.PublicPort != tun.PublicPort {
		t.Fatal("recovery lost the reserved port or desired state")
	}
}

func TestMappingAndConnectionLimits(t *testing.T) {
	h := newHarness(t, func(c *config.Server) {
		c.Limits.AgentTunnels = 1
		c.Limits.TunnelConnections = 1
		c.Limits.TotalConnections = 1
	})
	cfg, _ := h.agent()
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := local.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); accepted <- struct{}{}; io.Copy(c, c) }()
		}
	}()
	body := map[string]any{"agent_id": cfg.ID, "name": "limited", "local_address": "127.0.0.1", "local_port": local.Addr().(*net.TCPAddr).Port}
	var tun server.Tunnel
	h.must("POST", "tunnels", body, 202, &tun)
	h.waitTunnel(tun.ID, "ACTIVE")
	body["name"] = "over-quota"
	code, data := h.call("POST", "tunnels", body, "")
	if code != 409 || !bytes.Contains(data, []byte("AGENT_TUNNEL_LIMIT")) {
		t.Fatalf("mapping limit: %d %s", code, data)
	}
	dial := func() net.Conn {
		t.Helper()
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tun.PublicPort)), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		return c
	}
	first := dial()
	defer first.Close()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("first connection never reached target")
	}
	second := dial()
	defer second.Close()
	one := make([]byte, 1)
	if _, err := second.Read(one); err == nil {
		t.Fatal("connection limit did not reject second connection")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("excess connection was queued instead of rejected")
	}
	if _, err = first.Write([]byte("existing")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("existing"))
	if _, err = io.ReadFull(first, got); err != nil || string(got) != "existing" {
		t.Fatal("connection limit disrupted established stream", err)
	}
}
