package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestFraming(t *testing.T) {
	var stream bytes.Buffer
	for _, m := range []Message{{Type: "hello", Token: "test"}, {Type: "heartbeat"}} {
		if err := Write(&stream, m); err != nil {
			t.Fatal(err)
		}
	}
	first, err := Read(&stream)
	if err != nil || first.Token != "test" {
		t.Fatalf("first frame: %+v %v", first, err)
	}
	second, err := Read(&stream)
	if err != nil || second.Type != "heartbeat" {
		t.Fatalf("second frame: %+v %v", second, err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxFrame+1)
	if _, err := Read(bytes.NewReader(header[:])); err == nil {
		t.Fatal("unbounded frame accepted")
	}
	if _, err := Read(bytes.NewReader([]byte{0, 0, 0, 5, '{'})); err == nil {
		t.Fatal("truncated frame accepted")
	}
}
