package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"tunnel-lite/internal/config"
	"tunnel-lite/internal/protocol"
)

type Agent struct {
	cfg config.Agent
	tls *tls.Config
}
type target struct {
	address  string
	revision int64
}
type run struct {
	agent       *Agent
	peer        *protocol.Peer
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	targets     map[string]target
	connections map[net.Conn]string
	slots       chan struct{}
	wg          sync.WaitGroup
}

func New(c config.Agent) (*Agent, error) {
	ca, err := os.ReadFile(c.CA)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid CA certificate")
	}
	host, _, err := net.SplitHostPort(c.Server)
	if err != nil {
		return nil, err
	}
	return &Agent{cfg: c, tls: &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}}, nil
}
func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := a.once(ctx)
		if ctx.Err() != nil {
			break
		}
		slog.Warn("control connection ended; reconnecting", "error", err, "retry_in", backoff)
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		timer := time.NewTimer(backoff + time.Duration(rand.IntN(500))*time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
	return nil
}
func (a *Agent) dial(ctx context.Context) (net.Conn, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}, Config: a.tls}
	return d.DialContext(ctx, "tcp", a.cfg.Server)
}
func (a *Agent) once(ctx context.Context) error {
	conn, err := a.dial(ctx)
	if err != nil {
		return err
	}
	child, cancel := context.WithCancel(ctx)
	r := &run{agent: a, peer: &protocol.Peer{Conn: conn}, ctx: child, cancel: cancel, targets: map[string]target{}, connections: map[net.Conn]string{}, slots: make(chan struct{}, 256)}
	stop := context.AfterFunc(child, func() { conn.Close() })
	defer func() {
		cancel()
		stop()
		conn.Close()
		r.mu.Lock()
		for c := range r.connections {
			c.Close()
		}
		r.mu.Unlock()
		r.wg.Wait()
	}()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = r.peer.Send(protocol.Message{Type: "hello", AgentID: a.cfg.ID, Token: a.cfg.Token}); err != nil {
		return err
	}
	welcome, err := protocol.Read(conn)
	if err != nil {
		return err
	}
	if welcome.Type != "welcome" || welcome.Heartbeat < 1 || welcome.Heartbeat > 3600 {
		return fmt.Errorf("server rejected connection or heartbeat configuration")
	}
	interval := config.Seconds(welcome.Heartbeat)
	conn.SetReadDeadline(time.Time{})
	slog.Info("Agent connected", "agent", a.cfg.ID)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-child.Done():
				return
			case <-ticker.C:
				if err := r.peer.Send(protocol.Message{Type: "heartbeat"}); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	for {
		conn.SetReadDeadline(time.Now().Add(3*interval + 10*time.Second))
		m, err := protocol.Read(conn)
		if err != nil {
			return err
		}
		switch m.Type {
		case "heartbeat_ack":
			continue
		case "start":
			reason := ""
			if !a.allowed(m.Target) {
				reason = "target not allowed"
			}
			r.mu.Lock()
			if current, ok := r.targets[m.TunnelID]; ok && current.revision > m.Revision {
				r.mu.Unlock()
				continue
			}
			if reason == "" {
				r.targets[m.TunnelID] = target{m.Target, m.Revision}
			}
			r.mu.Unlock()
			if err = r.peer.Send(protocol.Message{Type: "started", TunnelID: m.TunnelID, Revision: m.Revision, Error: reason}); err != nil {
				return err
			}
		case "stop":
			r.mu.Lock()
			if current, ok := r.targets[m.TunnelID]; ok && m.Revision >= current.revision {
				delete(r.targets, m.TunnelID)
				for c, id := range r.connections {
					if id == m.TunnelID {
						c.Close()
					}
				}
			}
			r.mu.Unlock()
		case "connect":
			r.mu.Lock()
			t, ok := r.targets[m.TunnelID]
			r.mu.Unlock()
			if !ok || t.revision != m.Revision {
				r.peer.Send(protocol.Message{Type: "connect_error", ConnectionID: m.ConnectionID, Error: "CONFIG_STALE"})
				continue
			}
			select {
			case r.slots <- struct{}{}:
				r.wg.Add(1)
				go func() { defer r.wg.Done(); defer func() { <-r.slots }(); r.forward(m, t) }()
			default:
				r.peer.Send(protocol.Message{Type: "connect_error", ConnectionID: m.ConnectionID, Error: "AGENT_CONNECTION_LIMIT"})
			}
		default:
			return fmt.Errorf("unexpected control message")
		}
	}
}
func (a *Agent) allowed(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, v := range a.cfg.AllowedTargets {
		if ip.Equal(net.ParseIP(strings.TrimSpace(v))) {
			return true
		}
	}
	return false
}
func (r *run) track(c net.Conn, id string, revision int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.targets[id]
	if r.ctx.Err() != nil || !ok || t.revision != revision {
		c.Close()
		return false
	}
	r.connections[c] = id
	return true
}
func (r *run) untrack(c net.Conn) { c.Close(); r.mu.Lock(); delete(r.connections, c); r.mu.Unlock() }
func (r *run) forward(m protocol.Message, t target) {
	fail := func(reason string) {
		r.peer.Send(protocol.Message{Type: "connect_error", ConnectionID: m.ConnectionID, Error: reason})
	}
	ctx, cancel := context.WithTimeout(r.ctx, 8*time.Second)
	defer cancel()
	local, err := (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", t.address)
	if err != nil {
		fail("LOCAL_CONNECT_FAILED")
		return
	}
	if !r.track(local, m.TunnelID, t.revision) {
		return
	}
	defer r.untrack(local)
	data, err := r.agent.dial(ctx)
	if err != nil {
		fail("DATA_CHANNEL_FAILED")
		return
	}
	if !r.track(data, m.TunnelID, t.revision) {
		return
	}
	defer r.untrack(data)
	data.SetDeadline(time.Now().Add(5 * time.Second))
	if err = protocol.Write(data, protocol.Message{Type: "data", AgentID: r.agent.cfg.ID, TunnelID: m.TunnelID, ConnectionID: m.ConnectionID, Ticket: m.Ticket}); err != nil {
		fail("DATA_CHANNEL_FAILED")
		return
	}
	paired, err := protocol.Read(data)
	if err != nil || paired.Type != "paired" {
		fail("DATA_CHANNEL_FAILED")
		return
	}
	data.SetDeadline(time.Time{})
	protocol.Relay(local, data)
}
