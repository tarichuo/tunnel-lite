package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBoundedRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	w, err := Open(path, 12, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"first-line\n", "second-line\n", "third-line\n", "fourth-line\n"} {
		if _, err = w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{"": "fourth-line\n", ".1": "third-line\n", ".2": "second-line\n"}
	for suffix, want := range expected {
		b, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Fatalf("%s: got %q want %q", suffix, b, want)
		}
	}
	if _, err = os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("rotation exceeded backup count")
	}
}
