package server

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"tunnel-lite/internal/config"
	"tunnel-lite/internal/protocol"
)

type session struct {
	peer    *protocol.Peer
	agentID string
}
type runtimeTunnel struct {
	listener     net.Listener
	connections  map[string]*connection
	rollback     bool
	lastErrorLog time.Time
}
type connection struct {
	id, tunnelID, agentID, ticket string
	external                      net.Conn
	data                          net.Conn
	paired                        chan net.Conn
	owner                         *session
	done                          chan struct{}
	once                          sync.Once
}
type webSession struct {
	expires time.Time
	csrf    string
}
type idempotent struct {
	fingerprint, id string
	expires         time.Time
}
type Server struct {
	cfg         config.Server
	store       *store
	mu          sync.Mutex
	agents      map[string]*Agent
	tunnels     map[string]*Tunnel
	sessions    map[string]*session
	runtime     map[string]*runtimeTunnel
	pending     map[string]*connection
	webSessions map[string]webSession
	attempts    map[string][]time.Time
	idempotency map[string]idempotent
	control     net.Listener
	http        *http.Server
	closed      bool
	wg          sync.WaitGroup
	// Limit unauthenticated handshakes before goroutine creation.
	incoming chan struct{}
}

func New(c config.Server) (*Server, error) {
	st, err := openStore(c.Database.Path)
	if err != nil {
		return nil, err
	}
	a, t, err := st.load()
	if err != nil {
		st.db.Close()
		return nil, err
	}
	s := &Server{cfg: c, store: st, agents: a, tunnels: t, sessions: map[string]*session{}, runtime: map[string]*runtimeTunnel{}, pending: map[string]*connection{}, webSessions: map[string]webSession{}, attempts: map[string][]time.Time{}, idempotency: map[string]idempotent{}, incoming: make(chan struct{}, 128)}
	return s, nil
}
func (s *Server) Run(ctx context.Context) error {
	cert, err := tls.LoadX509KeyPair(s.cfg.Server.TLSCert, s.cfg.Server.TLSKey)
	if err != nil {
		return err
	}
	listener, err := tls.Listen("tcp", net.JoinHostPort(s.cfg.Server.ControlHost, strconv.Itoa(s.cfg.Server.ControlPort)), &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	s.control = listener
	api, err := net.Listen("tcp", net.JoinHostPort(s.cfg.Server.APIHost, strconv.Itoa(s.cfg.Server.APIPort)))
	if err != nil {
		listener.Close()
		return err
	}
	s.http = &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	s.mu.Lock()
	for _, t := range s.tunnels {
		if t.Enabled {
			t.PublicAddress = s.cfg.Server.PublicHost
			if err := s.bindLocked(t); err != nil {
				t.Status = "ERROR"
				t.Error = "公网端口监听失败"
			}
			if err := s.store.saveTunnel(t); err != nil {
				s.mu.Unlock()
				api.Close()
				s.Close()
				return err
			}
		}
	}
	s.mu.Unlock()
	errCh := make(chan error, 2)
	go func() { errCh <- s.acceptControl() }()
	go func() {
		if s.cfg.Server.APIHTTPS {
			errCh <- s.http.ServeTLS(api, s.cfg.Server.TLSCert, s.cfg.Server.TLSKey)
		} else {
			errCh <- s.http.Serve(api)
		}
	}()
	go s.maintenance(ctx)
	slog.Info("server ready", "api", api.Addr(), "https", s.cfg.Server.APIHTTPS, "control", listener.Addr())
	select {
	case <-ctx.Done():
		err = nil
	case err = <-errCh:
		if err == http.ErrServerClosed || isClosed(err) {
			err = nil
		}
	}
	s.Close()
	return err
}
func isClosed(err error) bool { return errors.Is(err, net.ErrClosed) }
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.control != nil {
		s.control.Close()
	}
	for _, ss := range s.sessions {
		ss.peer.Close()
	}
	for id, rt := range s.runtime {
		rt.listener.Close()
		for _, c := range rt.connections {
			s.closeConnectionLocked(c)
		}
		delete(s.runtime, id)
	}
	s.mu.Unlock()
	if s.http != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.http.Shutdown(ctx)
		cancel()
	}
	s.wg.Wait()
	s.store.db.Close()
}
func (s *Server) maintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			n := time.Now()
			for k, v := range s.webSessions {
				if n.After(v.expires) {
					delete(s.webSessions, k)
				}
			}
			for k, v := range s.idempotency {
				if n.After(v.expires) {
					delete(s.idempotency, k)
				}
			}
			for k, v := range s.attempts {
				if len(v) == 0 || n.Sub(v[len(v)-1]) > time.Minute {
					delete(s.attempts, k)
				}
			}
			if err := s.store.prune(s.cfg.Limits.LogDays, s.cfg.Limits.LogEntries); err != nil {
				slog.Error("log cleanup failed", "error", err)
			}
			s.mu.Unlock()
		}
	}
}
func (s *Server) record(id, action, detail string) {
	if err := s.store.log(id, action, detail); err != nil {
		slog.Error("operation log failed", "error", err)
	}
}
func (s *Server) persist(t *Tunnel) {
	if err := s.store.saveTunnel(t); err != nil {
		slog.Error("persist tunnel failed", "id", t.ID, "error", err)
	}
}
func (s *Server) bindLocked(t *Tunnel) error {
	if s.runtime[t.ID] != nil {
		return nil
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(t.PublicPort)))
	if err != nil {
		return err
	}
	rt := &runtimeTunnel{listener: listener, connections: map[string]*connection{}}
	s.runtime[t.ID] = rt
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.acceptTunnel(t.ID, rt) }()
	return nil
}
func (s *Server) closeConnectionLocked(c *connection) {
	c.once.Do(func() { close(c.done) })
	c.external.Close()
	if c.data != nil {
		c.data.Close()
	}
	delete(s.pending, c.id)
}
func (s *Server) unbindLocked(id string) {
	if rt := s.runtime[id]; rt != nil {
		rt.listener.Close()
		for _, c := range rt.connections {
			s.closeConnectionLocked(c)
		}
		delete(s.runtime, id)
	}
}
func (s *Server) sendConfigLocked(t *Tunnel) {
	ss := s.sessions[t.AgentID]
	if ss == nil {
		return
	}
	t.Status = "STARTING"
	t.UpdatedAt = now()
	t.Error = ""
	s.persist(t)
	if err := ss.peer.Send(protocol.Message{Type: "start", TunnelID: t.ID, Revision: t.Revision, Target: net.JoinHostPort(t.LocalAddress, strconv.Itoa(t.LocalPort))}); err != nil {
		ss.peer.Close()
		return
	}
	rev := t.Revision
	// The timer only applies while waiting for this exact configuration acknowledgement.
	time.AfterFunc(10*time.Second, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return
		}
		current := s.tunnels[t.ID]
		if current == nil || current.Revision != rev || current.Status != "STARTING" || s.sessions[t.AgentID] != ss {
			return
		}
		s.failLocked(current, "Agent 确认配置超时")
	})
}
func (s *Server) failLocked(t *Tunnel, reason string) {
	t.Status = "ERROR"
	t.Error = reason
	t.UpdatedAt = now()
	// Keep the reservation for a previously active mapping; a fresh attempt rolls back.
	if rt := s.runtime[t.ID]; rt != nil && rt.rollback {
		t.Enabled = false
		t.Revision++
		s.unbindLocked(t.ID)
		if ss := s.sessions[t.AgentID]; ss != nil {
			if err := ss.peer.Send(protocol.Message{Type: "stop", TunnelID: t.ID, Revision: t.Revision}); err != nil {
				ss.peer.Close()
			}
		}
	}
	s.persist(t)
	s.record(t.ID, "ERROR", reason)
}
func (s *Server) acceptControl() error {
	for {
		c, err := s.control.Accept()
		if err != nil {
			return err
		}
		select {
		case s.incoming <- struct{}{}:
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				c.Close()
				<-s.incoming
				return net.ErrClosed
			}
			s.wg.Add(1)
			s.mu.Unlock()
			go func() { defer s.wg.Done(); s.handleControl(c) }()
		default:
			c.Close()
		}
	}
}
func (s *Server) handleControl(c net.Conn) {
	defer c.Close()
	release := sync.OnceFunc(func() { <-s.incoming })
	defer release()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	m, err := protocol.Read(c)
	if err != nil {
		return
	}
	if m.Type == "data" {
		s.handleData(c, m, release)
		return
	}
	if m.Type != "hello" {
		return
	}
	s.mu.Lock()
	a := s.agents[m.AgentID]
	if s.closed || a == nil || subtle.ConstantTimeCompare([]byte(a.TokenHash), []byte(hash(m.Token))) != 1 {
		s.mu.Unlock()
		protocol.Write(c, protocol.Message{Type: "error", Error: "authentication failed"})
		return
	}
	ss := &session{peer: &protocol.Peer{Conn: c}, agentID: a.ID}
	if old := s.sessions[a.ID]; old != nil {
		old.peer.Close()
		for _, rt := range s.runtime {
			for _, cn := range rt.connections {
				if cn.owner == old {
					s.closeConnectionLocked(cn)
				}
			}
		}
	}
	s.sessions[a.ID] = ss
	a.Status = "ONLINE"
	a.LastConnected = now()
	a.LastHeartbeat = now()
	if err = s.store.saveAgent(a); err != nil {
		slog.Error("save agent failed", "error", err)
	}
	s.record(a.ID, "CONNECTED", "Agent 已连接")
	c.SetDeadline(time.Time{})
	if err = ss.peer.Send(protocol.Message{Type: "welcome", Heartbeat: s.cfg.Heartbeat.Interval}); err != nil {
		s.mu.Unlock()
		s.disconnect(ss)
		return
	}
	for _, t := range s.tunnels {
		if t.AgentID == a.ID && t.Enabled {
			if s.runtime[t.ID] == nil {
				if err = s.bindLocked(t); err != nil {
					t.Status = "ERROR"
					t.Error = "公网端口监听失败"
					s.persist(t)
					continue
				}
			}
			s.sendConfigLocked(t)
		}
	}
	s.mu.Unlock()
	release()
	defer s.disconnect(ss)
	for {
		c.SetReadDeadline(time.Now().Add(config.Seconds(s.cfg.Heartbeat.Timeout)))
		msg, err := protocol.Read(c)
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed || s.sessions[a.ID] != ss {
			s.mu.Unlock()
			return
		}
		switch msg.Type {
		case "heartbeat":
			if err := ss.peer.Send(protocol.Message{Type: "heartbeat_ack"}); err != nil {
				s.mu.Unlock()
				return
			}
			a.LastHeartbeat = now()
			if err := s.store.saveAgent(a); err != nil {
				slog.Error("save heartbeat failed", "error", err)
			}
		case "started":
			t := s.tunnels[msg.TunnelID]
			if t != nil && t.AgentID == a.ID && t.Enabled && t.Revision == msg.Revision && t.Status == "STARTING" {
				if msg.Error != "" {
					s.failLocked(t, "Agent 拒绝目标配置")
				} else {
					t.Status = "ACTIVE"
					if rt := s.runtime[t.ID]; rt != nil {
						rt.rollback = false
					}
					t.Error = ""
					t.LastForwardError = ""
					t.UpdatedAt = now()
					s.persist(t)
					s.record(t.ID, "ACTIVE", "映射已运行")
				}
			}
		case "connect_error":
			cn := s.pending[msg.ConnectionID]
			if cn != nil && cn.owner == ss {
				t := s.tunnels[cn.tunnelID]
				if t != nil {
					reason := "数据通道连接失败，请检查 Agent 网络与服务端连接"
					switch msg.Error {
					case "LOCAL_CONNECT_FAILED":
						reason = "本地服务连接失败，请检查目标地址、端口和服务进程"
					case "AGENT_CONNECTION_LIMIT":
						reason = "Agent 的并发连接数已达上限"
					case "CONFIG_STALE":
						reason = "Agent 配置已变化，请等待状态同步"
					}
					if t.LastForwardError != reason {
						t.LastForwardError = reason
						s.persist(t)
					}
					if rt := s.runtime[t.ID]; rt != nil && time.Since(rt.lastErrorLog) > time.Minute {
						s.record(t.ID, "FORWARD_ERROR", reason)
						rt.lastErrorLog = time.Now()
					}
				}
				s.closeConnectionLocked(cn)
			}
		default:
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}
}
func (s *Server) disconnect(ss *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.sessions[ss.agentID] != ss {
		return
	}
	delete(s.sessions, ss.agentID)
	a := s.agents[ss.agentID]
	a.Status = "OFFLINE"
	s.store.saveAgent(a)
	s.record(a.ID, "DISCONNECTED", "Agent 已断开")
	for _, t := range s.tunnels {
		if t.AgentID == ss.agentID && t.Enabled {
			s.record(t.ID, "OFFLINE", "Agent 已断开，公网端口保持预留")
			t.Status = "OFFLINE"
			t.UpdatedAt = now()
			s.persist(t)
		}
	}
	for _, rt := range s.runtime {
		for _, cn := range rt.connections {
			if cn.owner == ss {
				s.closeConnectionLocked(cn)
			}
		}
	}
}
func (s *Server) acceptTunnel(id string, rt *runtimeTunnel) {
	for {
		c, err := rt.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		t := s.tunnels[id]
		ss := s.sessions[t.AgentID]
		count := 0
		for _, r := range s.runtime {
			count += len(r.connections)
		}
		if s.closed || s.runtime[id] != rt || t.Status != "ACTIVE" || ss == nil || len(rt.connections) >= s.cfg.Limits.TunnelConnections || count >= s.cfg.Limits.TotalConnections {
			s.mu.Unlock()
			c.Close()
			continue
		}
		cn := &connection{id: protocol.Random(), tunnelID: id, agentID: t.AgentID, ticket: protocol.Random(), external: c, paired: make(chan net.Conn, 1), owner: ss, done: make(chan struct{})}
		rt.connections[cn.id] = cn
		s.pending[cn.id] = cn
		revision := t.Revision
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() { s.mu.Lock(); s.closeConnectionLocked(cn); delete(rt.connections, cn.id); s.mu.Unlock() }()
			if err := ss.peer.Send(protocol.Message{Type: "connect", TunnelID: id, ConnectionID: cn.id, Ticket: cn.ticket, Revision: revision}); err != nil {
				ss.peer.Close()
				return
			}
			timer := time.NewTimer(config.Seconds(s.cfg.Limits.PairTimeout))
			defer timer.Stop()
			select {
			case data := <-cn.paired:
				protocol.Relay(c, data)
			case <-timer.C:
				return
			case <-cn.done:
				return
			}
		}()
	}
}
func (s *Server) handleData(c net.Conn, m protocol.Message, release func()) {
	s.mu.Lock()
	cn := s.pending[m.ConnectionID]
	if s.closed || cn == nil || cn.agentID != m.AgentID || cn.tunnelID != m.TunnelID || cn.owner != s.sessions[m.AgentID] || subtle.ConstantTimeCompare([]byte(cn.ticket), []byte(m.Ticket)) != 1 {
		s.mu.Unlock()
		return
	}
	delete(s.pending, cn.id)
	cn.data = c
	c.SetDeadline(time.Now().Add(5 * time.Second))
	// Acknowledge before handing the stream to relay, so payload cannot overtake the frame.
	if err := protocol.Write(c, protocol.Message{Type: "paired"}); err != nil {
		s.closeConnectionLocked(cn)
		s.mu.Unlock()
		return
	}
	c.SetDeadline(time.Time{})
	if t := s.tunnels[cn.tunnelID]; t != nil && t.LastForwardError != "" {
		t.LastForwardError = ""
		s.persist(t)
	}
	cn.paired <- c
	s.mu.Unlock()
	release()
	// The relay owns closure; never read payload bytes from this goroutine.
	<-cn.done
}
func (s *Server) choosePortLocked(want int) (int, net.Listener, error) {
	used := map[int]bool{}
	for _, t := range s.tunnels {
		if t.Enabled {
			used[t.PublicPort] = true
		}
	}
	start, end := s.cfg.PortPool.Start, s.cfg.PortPool.End
	if want != 0 {
		if want < start || want > end {
			return 0, nil, fmt.Errorf("PORT_OUT_OF_RANGE")
		}
		start = want
		end = want
	}
	for p := start; p <= end; p++ {
		if used[p] {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(p)))
		if err == nil {
			return p, ln, nil
		}
	}
	if want != 0 {
		return 0, nil, fmt.Errorf("PORT_IN_USE")
	}
	return 0, nil, fmt.Errorf("PORT_POOL_EXHAUSTED")
}
