//go:build linux

package peercred_test

import (
	"os"
	"testing"

	"rhizome/internal/peercred"
)

func TestVerifyPIDFRRHZ130(t *testing.T) {
	server := connectedUnixConn(t)
	got, err := peercred.Verify(server)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID() <= 0 {
		t.Fatalf("PID = %d, want positive", got.PID())
	}
	if got.PID() != int32(os.Getpid()) {
		t.Fatalf("PID = %d, want %d", got.PID(), os.Getpid())
	}
}
