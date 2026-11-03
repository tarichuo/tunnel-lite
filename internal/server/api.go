package server

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"tunnel-lite/internal/protocol"
)

//go:embed web/*
var webFiles embed.FS

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Field     string `json:"field,omitempty"`
	RequestID string `json:"request_id"`
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, status int, code, message, field string) {
	jsonResponse(w, status, map[string]any{"error": apiError{Code: code, Message: message, Field: field, RequestID: protocol.Random()[:16]}})
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		failure(w, 400, "INVALID_REQUEST", "请求格式不正确", "")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		failure(w, 400, "INVALID_REQUEST", "请求只能包含一个 JSON 对象", "")
		return false
	}
	return true
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && u.Host == r.Host
}
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/session", s.sessionAPI)
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		if s.authorize(w, r) {
			s.api(w, r)
		}
	})
	sub, _ := fs.Sub(webFiles, "web")
	static := http.FileServer(http.FS(sub))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/app.js" && r.URL.Path != "/style.css" {
			http.NotFound(w, r)
			return
		}
		static.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") && subtle.ConstantTimeCompare([]byte(hash(strings.TrimPrefix(auth, "Bearer "))), []byte(hash(s.cfg.Server.AdminToken))) == 1 {
		return true
	}
	cookie, err := r.Cookie("tunnel_session")
	if err != nil {
		failure(w, 401, "UNAUTHORIZED", "请先登录", "")
		return false
	}
	s.mu.Lock()
	v, ok := s.webSessions[hash(cookie.Value)]
	s.mu.Unlock()
	if !ok || time.Now().After(v.expires) {
		failure(w, 401, "UNAUTHORIZED", "登录已过期", "")
		return false
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(v.csrf)) != 1 {
			failure(w, 403, "CSRF_REJECTED", "请求校验失败，请刷新页面", "")
			return false
		}
	}
	return true
}
func (s *Server) sessionAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		if !sameOrigin(r) {
			failure(w, 403, "CSRF_REJECTED", "请求来源无效", "")
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		if !decode(w, r, &body) {
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		s.mu.Lock()
		defer s.mu.Unlock()
		recent := []time.Time{}
		for _, t := range s.attempts[host] {
			if time.Since(t) < time.Minute {
				recent = append(recent, t)
			}
		}
		if len(recent) >= 10 {
			failure(w, 429, "RATE_LIMITED", "尝试过于频繁，请一分钟后重试", "")
			return
		}
		if len(s.attempts) > 4096 && len(recent) == 0 {
			failure(w, 429, "RATE_LIMITED", "请求过多，请稍后重试", "")
			return
		}
		if subtle.ConstantTimeCompare([]byte(hash(body.Token)), []byte(hash(s.cfg.Server.AdminToken))) != 1 {
			s.attempts[host] = append(recent, time.Now())
			failure(w, 401, "UNAUTHORIZED", "管理 Token 无效", "token")
			return
		}
		// Bound memory even if a client never logs out.
		if len(s.webSessions) >= 1024 {
			for k, v := range s.webSessions {
				if time.Now().After(v.expires) {
					delete(s.webSessions, k)
				}
			}
			if len(s.webSessions) >= 1024 {
				failure(w, 429, "RATE_LIMITED", "会话数量已达上限", "")
				return
			}
		}
		token := protocol.Random()
		csrf := protocol.Random()
		expires := time.Now().Add(12 * time.Hour)
		s.webSessions[hash(token)] = webSession{expires: expires, csrf: csrf}
		http.SetCookie(w, &http.Cookie{Name: "tunnel_session", Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
		jsonResponse(w, 200, map[string]any{"csrf_token": csrf, "expires_at": expires})
		return
	}
	if r.Method != "GET" && r.Method != "DELETE" {
		w.Header().Set("Allow", "GET, POST, DELETE")
		failure(w, 405, "METHOD_NOT_ALLOWED", "不支持的操作", "")
		return
	}
	if !s.authorize(w, r) {
		return
	}
	cookie, err := r.Cookie("tunnel_session")
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == "DELETE" {
		if err == nil {
			delete(s.webSessions, hash(cookie.Value))
		}
		http.SetCookie(w, &http.Cookie{Name: "tunnel_session", Value: "", Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		jsonResponse(w, 200, map[string]bool{"ok": true})
		return
	}
	csrf := ""
	if err == nil {
		csrf = s.webSessions[hash(cookie.Value)].csrf
	}
	jsonResponse(w, 200, map[string]string{"csrf_token": csrf})
}

type request struct {
	Name           string   `json:"name"`
	AgentID        string   `json:"agent_id"`
	LocalAddress   string   `json:"local_address"`
	LocalPort      int      `json:"local_port"`
	RemotePort     int      `json:"remote_port"`
	AllowedTargets []string `json:"allowed_targets"`
}

func validName(name string) bool {
	return len([]rune(name)) >= 1 && len([]rune(name)) <= 64 && !strings.ContainsAny(name, "\r\n\t")
}
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	var body request
	if r.Method == "POST" {
		if !decode(w, r, &body) {
			return
		}
		body.Name = strings.TrimSpace(body.Name)
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	parts := strings.Split(path, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		failure(w, 503, "UNAVAILABLE", "服务正在关闭", "")
		return
	}
	switch {
	case path == "meta" && r.Method == "GET":
		ca, err := os.ReadFile(s.cfg.Server.CACert)
		if err != nil {
			failure(w, 500, "INTERNAL_ERROR", "无法读取 CA 证书", "")
			return
		}
		jsonResponse(w, 200, map[string]any{"public_host": s.cfg.Server.PublicHost, "control_port": s.cfg.Server.ControlPort, "port_start": s.cfg.PortPool.Start, "port_end": s.cfg.PortPool.End, "agent_tunnel_limit": s.cfg.Limits.AgentTunnels, "tunnel_connection_limit": s.cfg.Limits.TunnelConnections, "ca_cert": string(ca)})
	case path == "overview" && r.Method == "GET":
		online, enabled, active, attention := 0, 0, 0, 0
		for _, a := range s.agents {
			if a.Status == "ONLINE" {
				online++
			}
		}
		for _, t := range s.tunnels {
			if t.Enabled {
				enabled++
				if t.Status == "ERROR" || t.Status == "OFFLINE" {
					attention++
				}
			}
			if t.Status == "ACTIVE" {
				active++
			}
		}
		jsonResponse(w, 200, map[string]int{"online_agents": online, "enabled_tunnels": enabled, "active_tunnels": active, "attention_tunnels": attention})
	case path == "agents" && r.Method == "POST":
		s.createAgent(w, body)
	case path == "agents" && r.Method == "GET":
		result := []any{}
		as := []*Agent{}
		for _, a := range s.agents {
			as = append(as, a)
		}
		sort.Slice(as, func(i, j int) bool { return as[i].CreatedAt > as[j].CreatedAt })
		for _, a := range as {
			count := 0
			for _, t := range s.tunnels {
				if t.AgentID == a.ID && t.Enabled {
					count++
				}
			}
			result = append(result, struct {
				*Agent
				TunnelCount int `json:"tunnel_count"`
			}{a, count})
		}
		jsonResponse(w, 200, map[string]any{"items": result, "total": len(result)})
	case len(parts) == 2 && parts[0] == "agents" && r.Method == "GET":
		a := s.agents[parts[1]]
		if a == nil {
			failure(w, 404, "NOT_FOUND", "Agent 不存在", "")
			return
		}
		logs, err := s.store.logs(a.ID)
		if err != nil {
			failure(w, 500, "INTERNAL_ERROR", "无法读取操作记录", "")
			return
		}
		jsonResponse(w, 200, map[string]any{"agent": a, "operations": logs})
	case path == "tunnels" && r.Method == "GET":
		s.listTunnels(w, r)
	case path == "tunnels" && r.Method == "POST":
		s.createTunnel(w, r, body)
	case len(parts) == 2 && parts[0] == "tunnels" && r.Method == "GET":
		t := s.tunnels[parts[1]]
		if t == nil {
			failure(w, 404, "NOT_FOUND", "映射不存在", "")
			return
		}
		logs, err := s.store.logs(t.ID)
		if err != nil {
			failure(w, 500, "INTERNAL_ERROR", "无法读取操作记录", "")
			return
		}
		jsonResponse(w, 200, map[string]any{"tunnel": t, "operations": logs})
	case len(parts) == 2 && parts[0] == "tunnels" && r.Method == "DELETE":
		s.stopTunnel(w, parts[1])
	case len(parts) == 3 && parts[0] == "tunnels" && parts[2] == "start" && r.Method == "POST":
		s.startTunnel(w, parts[1], body)
	default:
		failure(w, 404, "NOT_FOUND", "接口不存在或不支持此操作", "")
	}
}
func (s *Server) createAgent(w http.ResponseWriter, b request) {
	if !validName(b.Name) {
		failure(w, 400, "INVALID_NAME", "名称长度应为 1—64 个字符", "name")
		return
	}
	if len(b.AllowedTargets) == 0 {
		b.AllowedTargets = []string{"127.0.0.1"}
	}
	if len(b.AllowedTargets) > 32 {
		failure(w, 400, "INVALID_TARGET", "最多允许 32 个目标 IP", "allowed_targets")
		return
	}
	for i, ip := range b.AllowedTargets {
		parsed := net.ParseIP(strings.TrimSpace(ip))
		if parsed == nil || parsed.IsUnspecified() || parsed.IsMulticast() {
			failure(w, 400, "INVALID_TARGET", "请输入有效的目标 IP", "allowed_targets")
			return
		}
		b.AllowedTargets[i] = parsed.String()
	}
	token := protocol.Random()
	a := &Agent{ID: "agent_" + protocol.Random()[:24], Name: b.Name, Status: "PENDING", AllowedTargets: b.AllowedTargets, CreatedAt: now(), TokenHash: hash(token)}
	if err := s.store.saveAgent(a); err != nil {
		failure(w, 500, "INTERNAL_ERROR", "Agent 保存失败", "")
		return
	}
	s.agents[a.ID] = a
	s.record(a.ID, "CREATED", "已创建 Agent 身份")
	jsonResponse(w, 201, map[string]any{"agent": a, "token": token})
}
func (s *Server) listTunnels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	search := strings.ToLower(q.Get("q"))
	status := q.Get("status")
	items := []*Tunnel{}
	for _, t := range s.tunnels {
		if status == "" && t.Status == "STOPPED" {
			continue
		}
		if status != "" && status != "ALL" && t.Status != status {
			continue
		}
		if q.Get("agent_id") != "" && t.AgentID != q.Get("agent_id") {
			continue
		}
		a := s.agents[t.AgentID]
		text := strings.ToLower(fmt.Sprintf("%s %s %s %s %s:%d %s:%d", t.Name, t.ID, a.Name, a.ID, t.LocalAddress, t.LocalPort, t.PublicAddress, t.PublicPort))
		if !strings.Contains(text, search) {
			continue
		}
		items = append(items, t)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt == items[j].CreatedAt {
			return items[i].ID > items[j].ID
		}
		return items[i].CreatedAt > items[j].CreatedAt
	})
	total := len(items)
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(q.Get("page_size"))
	if size < 1 || size > 100 {
		size = 20
	}
	maxPage := (total + size - 1) / size
	if maxPage < 1 {
		maxPage = 1
	}
	if page > maxPage {
		page = maxPage
	}
	start := (page - 1) * size
	end := start + size
	if end > total {
		end = total
	}
	items = items[start:end]
	jsonResponse(w, 200, map[string]any{"items": items, "total": total, "page": page, "page_size": size})
}
func (s *Server) validateTunnel(w http.ResponseWriter, b request) bool {
	if !validName(b.Name) {
		failure(w, 400, "INVALID_NAME", "名称长度应为 1—64 个字符", "name")
		return false
	}
	a := s.agents[b.AgentID]
	if a == nil {
		failure(w, 400, "INVALID_AGENT", "请选择有效 Agent", "agent_id")
		return false
	}
	if s.sessions[a.ID] == nil {
		failure(w, 409, "AGENT_OFFLINE", "Agent 已离线，请等待上线或更换 Agent", "agent_id")
		return false
	}
	ip := net.ParseIP(b.LocalAddress)
	allowed := false
	for _, v := range a.AllowedTargets {
		if ip != nil && ip.Equal(net.ParseIP(v)) {
			allowed = true
		}
	}
	if !allowed {
		failure(w, 400, "TARGET_NOT_ALLOWED", "目标 IP 不在 Agent 授权范围内", "local_address")
		return false
	}
	if b.LocalPort < 1 || b.LocalPort > 65535 {
		failure(w, 400, "INVALID_PORT", "本地端口应为 1—65535 的整数", "local_port")
		return false
	}
	if b.RemotePort < 0 || b.RemotePort > 65535 {
		failure(w, 400, "INVALID_PORT", "公网端口无效", "remote_port")
		return false
	}
	count := 0
	for _, t := range s.tunnels {
		if t.AgentID == a.ID && t.Enabled {
			count++
		}
	}
	if count >= s.cfg.Limits.AgentTunnels {
		failure(w, 409, "AGENT_TUNNEL_LIMIT", "该 Agent 的有效映射数量已达上限", "")
		return false
	}
	return true
}
func portFailure(w http.ResponseWriter, err error) {
	code := err.Error()
	msg := "端口池已耗尽，请下线不再需要的映射"
	if code == "PORT_IN_USE" {
		msg = "该公网端口已被占用，请更换或使用自动分配"
	}
	if code == "PORT_OUT_OF_RANGE" {
		msg = "公网端口不在允许的端口池范围内"
	}
	failure(w, 409, code, msg, "remote_port")
}
func (s *Server) createTunnel(w http.ResponseWriter, r *http.Request, b request) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 128 {
		failure(w, 400, "INVALID_REQUEST", "幂等键过长", "")
		return
	}
	encoded, _ := json.Marshal(b)
	fingerprint := hash(string(encoded))
	if key != "" {
		if prev, ok := s.idempotency[key]; ok && time.Now().Before(prev.expires) {
			if prev.fingerprint != fingerprint {
				failure(w, 409, "IDEMPOTENCY_CONFLICT", "此幂等键已用于其他请求", "")
				return
			}
			jsonResponse(w, 200, s.tunnels[prev.id])
			return
		}
		if len(s.idempotency) >= 4096 {
			failure(w, 429, "RATE_LIMITED", "创建请求过多，请稍后再试", "")
			return
		}
	}
	if !s.validateTunnel(w, b) {
		return
	}
	for _, t := range s.tunnels {
		if t.AgentID == b.AgentID && t.Name == b.Name {
			failure(w, 409, "NAME_EXISTS", "该 Agent 中已存在同名映射", "name")
			return
		}
	}
	port, ln, err := s.choosePortLocked(b.RemotePort)
	if err != nil {
		portFailure(w, err)
		return
	}
	mode := "auto"
	if b.RemotePort != 0 {
		mode = "manual"
	}
	t := &Tunnel{ID: "tunnel_" + protocol.Random()[:24], AgentID: b.AgentID, Name: b.Name, LocalAddress: net.ParseIP(b.LocalAddress).String(), LocalPort: b.LocalPort, PublicAddress: s.cfg.Server.PublicHost, PublicPort: port, PortMode: mode, Protocol: "TCP", Status: "STARTING", Enabled: true, Revision: 1, CreatedAt: now(), UpdatedAt: now()}
	if err = s.store.saveTunnel(t); err != nil {
		ln.Close()
		failure(w, 500, "INTERNAL_ERROR", "映射保存失败", "")
		return
	}
	s.tunnels[t.ID] = t
	s.installListenerLocked(t, ln, true)
	s.record(t.ID, "CREATED", "已创建映射")
	s.sendConfigLocked(t)
	if key != "" {
		s.idempotency[key] = idempotent{fingerprint: fingerprint, id: t.ID, expires: time.Now().Add(24 * time.Hour)}
	}
	jsonResponse(w, 202, t)
}
func (s *Server) installListenerLocked(t *Tunnel, ln net.Listener, rollback bool) {
	rt := &runtimeTunnel{listener: ln, connections: map[string]*connection{}, rollback: rollback}
	s.runtime[t.ID] = rt
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.acceptTunnel(t.ID, rt) }()
}
func (s *Server) stopTunnel(w http.ResponseWriter, id string) {
	t := s.tunnels[id]
	if t == nil {
		failure(w, 404, "NOT_FOUND", "映射不存在", "")
		return
	}
	if t.Status == "STOPPED" {
		jsonResponse(w, 200, t)
		return
	}
	old := *t
	t.Enabled = false
	t.Status = "STOPPED"
	t.Revision++
	t.UpdatedAt = now()
	t.Error = ""
	t.LastForwardError = ""
	if err := s.store.saveTunnel(t); err != nil {
		*t = old
		failure(w, 500, "INTERNAL_ERROR", "下线配置保存失败", "")
		return
	}
	s.unbindLocked(id)
	if ss := s.sessions[t.AgentID]; ss != nil {
		if err := ss.peer.Send(protocol.Message{Type: "stop", TunnelID: id, Revision: t.Revision}); err != nil {
			ss.peer.Close()
		}
	}
	s.record(id, "STOPPED", "用户下线映射并释放端口")
	jsonResponse(w, 200, t)
}
func (s *Server) startTunnel(w http.ResponseWriter, id string, b request) {
	t := s.tunnels[id]
	if t == nil {
		failure(w, 404, "NOT_FOUND", "映射不存在", "")
		return
	}
	if t.Enabled {
		failure(w, 409, "ALREADY_ENABLED", "请先下线该映射，再尝试启用", "")
		return
	}
	check := request{Name: t.Name, AgentID: t.AgentID, LocalAddress: t.LocalAddress, LocalPort: t.LocalPort, RemotePort: b.RemotePort}
	if !s.validateTunnel(w, check) {
		return
	}
	old := *t
	// An explicit positive port selects manual mode; zero means auto. The UI submits the saved manual port by default.
	want := b.RemotePort
	var ln net.Listener
	var port int
	var err error
	if want == 0 && t.PortMode == "auto" {
		port, ln, err = s.choosePortLocked(t.PublicPort)
	}
	if ln == nil {
		port, ln, err = s.choosePortLocked(want)
	}
	if err != nil {
		portFailure(w, err)
		return
	}
	t.PublicPort = port
	t.PublicAddress = s.cfg.Server.PublicHost
	t.PortMode = "auto"
	if want != 0 {
		t.PortMode = "manual"
	}
	t.Enabled = true
	t.Status = "STARTING"
	t.Revision++
	t.UpdatedAt = now()
	t.Error = ""
	if err = s.store.saveTunnel(t); err != nil {
		*t = old
		ln.Close()
		failure(w, 500, "INTERNAL_ERROR", "启用配置保存失败", "")
		return
	}
	s.installListenerLocked(t, ln, true)
	s.sendConfigLocked(t)
	s.record(id, "STARTED", "用户再次启用映射")
	jsonResponse(w, 202, t)
}
