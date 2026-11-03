// Package protocol defines bounded length-prefixed JSON frames over TLS.
package protocol

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const MaxFrame = 64 * 1024

type Message struct {
	Type         string `json:"type"`
	AgentID      string `json:"agent_id,omitempty"`
	Token        string `json:"token,omitempty"`
	TunnelID     string `json:"tunnel_id,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
	Ticket       string `json:"ticket,omitempty"`
	Target       string `json:"target,omitempty"`
	Revision     int64  `json:"revision,omitempty"`
	Error        string `json:"error,omitempty"`
	Heartbeat    int    `json:"heartbeat,omitempty"`
}

type Peer struct {
	net.Conn
	mu sync.Mutex
}

func (p *Peer) Send(m Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	defer p.SetWriteDeadline(time.Time{})
	return Write(p.Conn, m)
}
func Write(w io.Writer, m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return fmt.Errorf("frame too large")
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(b)))
	if err = writeAll(w, header); err != nil {
		return err
	}
	return writeAll(w, b)
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func Read(r io.Reader) (Message, error) {
	var m Message
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return m, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > MaxFrame {
		return m, fmt.Errorf("invalid frame length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return m, err
	}
	err := json.Unmarshal(b, &m)
	return m, err
}
func Random() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Relay preserves TCP half-close semantics; each direction has one bounded buffer.
func Relay(a, b net.Conn) {
	var wg sync.WaitGroup
	copyOne := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		_, err := io.CopyBuffer(dst, src, buf)
		if err != nil {
			a.Close()
			b.Close()
			return
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go copyOne(a, b)
	go copyOne(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}
