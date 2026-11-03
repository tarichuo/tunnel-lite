package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Logs struct {
		Path      string `yaml:"path"`
		MaxSizeMB int    `yaml:"max_size_mb"`
		Backups   int    `yaml:"backups"`
	} `yaml:"logs"`
	Server struct {
		APIHost     string `yaml:"api_host"`
		APIPort     int    `yaml:"api_port"`
		ControlHost string `yaml:"control_host"`
		ControlPort int    `yaml:"control_port"`
		PublicHost  string `yaml:"public_host"`
		AdminToken  string `yaml:"admin_token"`
		TLSCert     string `yaml:"tls_cert"`
		TLSKey      string `yaml:"tls_key"`
		CACert      string `yaml:"ca_cert"`
		APIHTTPS    bool   `yaml:"api_https"`
	} `yaml:"server"`
	PortPool struct {
		Start int `yaml:"start"`
		End   int `yaml:"end"`
	} `yaml:"port_pool"`
	Heartbeat struct {
		Interval int `yaml:"interval"`
		Timeout  int `yaml:"timeout"`
	} `yaml:"heartbeat"`
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`
	Limits struct {
		AgentTunnels      int `yaml:"agent_tunnels"`
		TunnelConnections int `yaml:"tunnel_connections"`
		TotalConnections  int `yaml:"total_connections"`
		PairTimeout       int `yaml:"pair_timeout"`
		LogDays           int `yaml:"log_days"`
		LogEntries        int `yaml:"log_entries"`
	} `yaml:"limits"`
}

type Agent struct {
	Server         string   `yaml:"server"`
	ID             string   `yaml:"agent_id"`
	Token          string   `yaml:"token"`
	Name           string   `yaml:"name"`
	CA             string   `yaml:"ca_cert"`
	AllowedTargets []string `yaml:"allowed_targets"`
}

func Read(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err = d.Decode(out); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	return nil
}

func LoadServer(path string) (Server, error) {
	var c Server
	c.Logs.Path = "./logs/tunnel.log"
	c.Logs.MaxSizeMB = 5
	c.Logs.Backups = 3
	c.Server.APIHost = "127.0.0.1"
	c.Server.APIPort = 7000
	c.Server.ControlPort = 7001
	c.PortPool.Start = 10000
	c.PortPool.End = 20000
	c.Heartbeat.Interval = 15
	c.Heartbeat.Timeout = 45
	c.Database.Path = "./data/tunnel.db"
	c.Limits.AgentTunnels = 20
	c.Limits.TunnelConnections = 64
	c.Limits.TotalConnections = 256
	c.Limits.PairTimeout = 10
	c.Limits.LogDays = 30
	c.Limits.LogEntries = 10000
	if err := Read(path, &c); err != nil {
		return c, err
	}
	if c.Server.AdminToken == "" || len(c.Server.AdminToken) < 24 {
		return c, fmt.Errorf("admin_token must contain at least 24 characters")
	}
	if c.Server.PublicHost == "" || net.ParseIP(c.Server.PublicHost) == nil {
		return c, fmt.Errorf("public_host must be a public-facing IP address")
	}
	if c.Server.TLSCert == "" || c.Server.TLSKey == "" || c.Server.CACert == "" {
		return c, fmt.Errorf("tls_cert, tls_key and ca_cert are required")
	}
	if c.Server.APIPort < 1 || c.Server.APIPort > 65535 || c.Server.ControlPort < 1 || c.Server.ControlPort > 65535 || c.Server.APIPort == c.Server.ControlPort {
		return c, fmt.Errorf("invalid API/control ports")
	}
	if c.PortPool.Start < 1 || c.PortPool.End > 65535 || c.PortPool.Start > c.PortPool.End {
		return c, fmt.Errorf("invalid port pool")
	}
	for _, p := range []int{c.Server.APIPort, c.Server.ControlPort} {
		if p >= c.PortPool.Start && p <= c.PortPool.End {
			return c, fmt.Errorf("management ports cannot overlap the port pool")
		}
	}
	if c.Heartbeat.Interval < 1 || c.Heartbeat.Interval > 3600 || c.Heartbeat.Timeout <= c.Heartbeat.Interval {
		return c, fmt.Errorf("invalid heartbeat timing")
	}
	if c.Logs.MaxSizeMB < 1 || c.Logs.MaxSizeMB > 100 || c.Logs.Backups < 1 || c.Logs.Backups > 10 {
		return c, fmt.Errorf("invalid log rotation limits")
	}
	if c.Limits.AgentTunnels < 1 || c.Limits.TunnelConnections < 1 || c.Limits.TotalConnections < 1 || c.Limits.PairTimeout < 1 || c.Limits.LogDays < 1 || c.Limits.LogEntries < 1 {
		return c, fmt.Errorf("limits must be positive")
	}
	if !c.Server.APIHTTPS {
		ip := net.ParseIP(c.Server.APIHost)
		if ip == nil || !ip.IsLoopback() {
			return c, fmt.Errorf("HTTP API must bind to a loopback IP; enable api_https for remote access")
		}
	}
	// Relative paths resolve against the config directory, not the working directory.
	base, _ := filepath.Abs(filepath.Dir(path))
	for _, v := range []*string{&c.Logs.Path, &c.Database.Path, &c.Server.TLSCert, &c.Server.TLSKey, &c.Server.CACert} {
		if !filepath.IsAbs(*v) {
			*v = filepath.Join(base, *v)
		}
	}
	return c, nil
}

func LoadAgent(path string) (Agent, error) {
	var c Agent
	if err := Read(path, &c); err != nil {
		return c, err
	}
	if c.Server == "" || c.ID == "" || c.Token == "" || c.CA == "" {
		return c, fmt.Errorf("server, agent_id, token and ca_cert are required")
	}
	if _, _, err := net.SplitHostPort(c.Server); err != nil {
		return c, fmt.Errorf("invalid server address: %w", err)
	}
	if len(c.AllowedTargets) == 0 {
		c.AllowedTargets = []string{"127.0.0.1"}
	}
	for _, target := range c.AllowedTargets {
		if net.ParseIP(target) == nil {
			return c, fmt.Errorf("allowed_targets must contain IP addresses")
		}
	}
	if !filepath.IsAbs(c.CA) {
		base, _ := filepath.Abs(filepath.Dir(path))
		c.CA = filepath.Join(base, c.CA)
	}
	return c, nil
}

func Seconds(n int) time.Duration { return time.Duration(n) * time.Second }
