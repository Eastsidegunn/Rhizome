//go:build linux || darwin

package peercred_test

import (
	"os"
	"reflect"
	"testing"

	"rhizome/internal/peercred"
)

func TestVerifyFRRHZ130(t *testing.T) {
	server := connectedUnixConn(t)
	got, err := peercred.Verify(server)
	if err != nil {
		t.Fatal(err)
	}
	if got.UID() != uint32(os.Getuid()) {
		t.Fatalf("UID = %d, want %d", got.UID(), os.Getuid())
	}
	if got.GID() != uint32(os.Getgid()) {
		t.Fatalf("GID = %d, want %d", got.GID(), os.Getgid())
	}
	if !got.SameUser() {
		t.Fatal("SameUser returned false for current user")
	}
}

func TestVerifyRejectsInvalidConnectionWithoutPathFRRHZ130(t *testing.T) {
	if _, err := peercred.Verify(nil); err == nil {
		t.Fatal("Verify(nil) succeeded")
	}

	conn := connectedUnixConn(t)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := peercred.Verify(conn)
	if err == nil {
		t.Fatal("Verify on a closed Unix connection succeeded")
	}
	if err.Error() != "peercred: invalid Unix connection" {
		t.Fatalf("error may expose connection details: %q", err)
	}
}

func TestUnverifiedValueCannotBeForgedFRRHZ130(t *testing.T) {
	zero := peercred.Verified{}
	copyOfZero := zero
	for i, v := range []peercred.Verified{zero, copyOfZero} {
		if v.SameUser() {
			t.Fatalf("unverified value %d was accepted", i)
		}
	}

	typ := reflect.TypeOf(zero)
	for _, name := range []string{"UID", "GID", "PID"} {
		if _, ok := typ.FieldByName(name); ok {
			t.Fatalf("Verified.%s is an exported, writable field", name)
		}
	}
	value := reflect.ValueOf(&zero).Elem()
	for _, name := range []string{"uid", "gid", "pid", "verified"} {
		field := value.FieldByName(name)
		if !field.IsValid() {
			t.Fatalf("Verified.%s does not exist", name)
		}
		if field.CanSet() {
			t.Fatalf("outside package can change Verified.%s via reflection", name)
		}
	}
}
