package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
	"tunnel-lite/internal/config"
	"tunnel-lite/internal/protocol"
)

// Init creates a private CA and server certificate; it never overwrites files.
func Init(dir, host string) error {
	if net.ParseIP(host) == nil {
		return fmt.Errorf("public host must be an IP address")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, name := range []string{"server.yaml", "ca.crt", "server.crt", "server.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			return fmt.Errorf("refusing to overwrite %s", name)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial := func() *big.Int { n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)); return n }
	ca := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "Tunnel Lite local CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		return err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0), IPAddresses: []net.IP{net.ParseIP(host), net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &serverKey.PublicKey, key)
	if err != nil {
		return err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return err
	}
	files := map[string][]byte{"ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), "server.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "server.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})}
	// The CA signing key is intentionally not persisted. Re-run init in a fresh directory to rotate the trust root.
	var c config.Server
	c.Logs.Path = "logs/tunnel.log"
	c.Logs.MaxSizeMB = 5
	c.Logs.Backups = 3
	c.Server.APIHost = "127.0.0.1"
	c.Server.APIPort = 7000
	c.Server.ControlPort = 7001
	c.Server.PublicHost = host
	c.Server.AdminToken = protocol.Random()
	c.Server.TLSCert = "server.crt"
	c.Server.TLSKey = "server.key"
	c.Server.CACert = "ca.crt"
	c.PortPool.Start = 10000
	c.PortPool.End = 20000
	c.Heartbeat.Interval = 15
	c.Heartbeat.Timeout = 45
	c.Database.Path = "data/tunnel.db"
	c.Limits.AgentTunnels = 20
	c.Limits.TunnelConnections = 64
	c.Limits.TotalConnections = 256
	c.Limits.PairTimeout = 10
	c.Limits.LogDays = 30
	c.Limits.LogEntries = 10000
	files["server.yaml"], err = yaml.Marshal(c)
	if err != nil {
		return err
	}
	for name, data := range files {
		f, e := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(data)
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	}
	return nil
}
