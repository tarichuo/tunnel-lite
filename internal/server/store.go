package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Agent struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Status         string   `json:"status"`
	LastHeartbeat  string   `json:"last_heartbeat,omitempty"`
	LastConnected  string   `json:"last_connected,omitempty"`
	AllowedTargets []string `json:"allowed_targets"`
	CreatedAt      string   `json:"created_at"`
	TokenHash      string   `json:"-"`
}
type Tunnel struct {
	ID               string `json:"id"`
	AgentID          string `json:"agent_id"`
	Name             string `json:"name"`
	LocalAddress     string `json:"local_address"`
	LocalPort        int    `json:"local_port"`
	PublicAddress    string `json:"public_address"`
	PublicPort       int    `json:"public_port"`
	PortMode         string `json:"port_mode"`
	Protocol         string `json:"protocol"`
	Status           string `json:"status"`
	Enabled          bool   `json:"enabled"`
	Revision         int64  `json:"revision"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	Error            string `json:"error,omitempty"`
	LastForwardError string `json:"last_forward_error,omitempty"`
}
type Operation struct {
	ID         int64  `json:"id"`
	ResourceID string `json:"resource_id"`
	Action     string `json:"action"`
	Detail     string `json:"detail"`
	CreatedAt  string `json:"created_at"`
}
type store struct{ db *sql.DB }

func now() string              { return time.Now().UTC().Format(time.RFC3339) }
func hash(token string) string { v := sha256.Sum256([]byte(token)); return hex.EncodeToString(v[:]) }
func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// Precreate privately; database, WAL and shared-memory files inherit restricted permissions.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS agents(id TEXT PRIMARY KEY, token_hash TEXT NOT NULL, data TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS tunnels(id TEXT PRIMARY KEY, reserved_port INTEGER UNIQUE, data TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS operation_logs(id INTEGER PRIMARY KEY AUTOINCREMENT,resource_id TEXT NOT NULL,action TEXT NOT NULL,detail TEXT NOT NULL,created_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS logs_resource ON operation_logs(resource_id,id);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &store{db}, nil
}
func (st *store) saveAgent(a *Agent) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = st.db.Exec(`INSERT INTO agents(id,token_hash,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET token_hash=excluded.token_hash,data=excluded.data`, a.ID, a.TokenHash, string(b))
	return err
}
func (st *store) saveTunnel(t *Tunnel) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	var port any
	if t.Enabled {
		port = t.PublicPort
	}
	_, err = st.db.Exec(`INSERT INTO tunnels(id,reserved_port,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET reserved_port=excluded.reserved_port,data=excluded.data`, t.ID, port, string(b))
	return err
}
func (st *store) load() (map[string]*Agent, map[string]*Tunnel, error) {
	agents := map[string]*Agent{}
	tunnels := map[string]*Tunnel{}
	rows, err := st.db.Query("SELECT data,token_hash FROM agents")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var b, h string
		if err = rows.Scan(&b, &h); err != nil {
			break
		}
		var a Agent
		if err = json.Unmarshal([]byte(b), &a); err != nil {
			break
		}
		a.TokenHash = h
		if a.Status == "ONLINE" {
			a.Status = "OFFLINE"
		}
		agents[a.ID] = &a
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = st.db.Query("SELECT data FROM tunnels")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var b string
		if err = rows.Scan(&b); err != nil {
			break
		}
		var t Tunnel
		if err = json.Unmarshal([]byte(b), &t); err != nil {
			break
		}
		if t.Enabled {
			t.Status = "OFFLINE"
		}
		tunnels[t.ID] = &t
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	return agents, tunnels, err
}
func (st *store) log(id, action, detail string) error {
	_, err := st.db.Exec("INSERT INTO operation_logs(resource_id,action,detail,created_at) VALUES(?,?,?,?)", id, action, detail, now())
	return err
}
func (st *store) logs(id string) ([]Operation, error) {
	rows, err := st.db.Query("SELECT id,resource_id,action,detail,created_at FROM operation_logs WHERE resource_id=? ORDER BY id DESC LIMIT 20", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Operation{}
	for rows.Next() {
		var o Operation
		if err := rows.Scan(&o.ID, &o.ResourceID, &o.Action, &o.Detail, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (st *store) prune(days, entries int) error {
	_, err := st.db.Exec(`DELETE FROM operation_logs WHERE created_at < ? OR id NOT IN (SELECT id FROM operation_logs ORDER BY id DESC LIMIT ?)`, time.Now().AddDate(0, 0, -days).UTC().Format(time.RFC3339), entries)
	return err
}
