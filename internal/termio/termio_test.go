package termio

import (
	"os"
	"testing"
)

func TestIsTerminalFRRHZ130(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-a-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(int(f.Fd())) {
		t.Fatal("regular file reported as a terminal")
	}
	if _, err := ReadPassword(int(f.Fd())); err == nil {
		t.Fatal("ReadPassword accepted a regular file")
	}
}
