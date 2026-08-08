//go:build linux

package main

import (
	"bytes"
	"errors"
	"testing"
)

func TestCopyInputDetachesOnCtrlSpace(t *testing.T) {
	var destination bytes.Buffer
	err := copyInput(&destination, bytes.NewBufferString("echo hello\x00ignored"))
	if !errors.Is(err, errDetach) {
		t.Fatalf("copyInput error = %v, want detach", err)
	}
	if got, want := destination.String(), "echo hello"; got != want {
		t.Errorf("forwarded input = %q, want %q", got, want)
	}
}
