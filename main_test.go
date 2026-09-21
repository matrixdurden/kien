//go:build linux

package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
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

func TestAppendHistoryKeepsMostRecentOutput(t *testing.T) {
	server := sessionServer{history: []byte("existing")}
	server.appendHistory(bytes.Repeat([]byte("x"), historyLimit))

	if got, want := len(server.history), historyLimit; got != want {
		t.Fatalf("history length = %d, want %d", got, want)
	}
	if got, want := string(server.history[:7]), "xxxxxxx"; got != want {
		t.Errorf("history prefix = %q, want %q", got, want)
	}
}

func TestAppendHistoryReplacesWithLargeOutputTail(t *testing.T) {
	server := sessionServer{history: []byte("existing")}
	output := append(bytes.Repeat([]byte("a"), historyLimit), 'z')
	server.appendHistory(output)

	if got, want := len(server.history), historyLimit; got != want {
		t.Fatalf("history length = %d, want %d", got, want)
	}
	if got, want := server.history[0], byte('a'); got != want {
		t.Errorf("history first byte = %q, want %q", got, want)
	}
	if got, want := server.history[len(server.history)-1], byte('z'); got != want {
		t.Errorf("history final byte = %q, want %q", got, want)
	}
}

func TestAppendHistoryTracksAlternateScreenAcrossOutputChunks(t *testing.T) {
	server := sessionServer{}
	server.appendHistory([]byte("\033[?104"))
	server.appendHistory([]byte("9h"))
	if !server.alternateScreen {
		t.Error("alternate screen should be active")
	}

	server.appendHistory([]byte("\033[?1049l"))
	if server.alternateScreen {
		t.Error("alternate screen should be inactive")
	}
}

func TestSessionState(t *testing.T) {
	if got := sessionState(42, 42); got != "idle" {
		t.Errorf("same foreground and shell group = %q, want idle", got)
	}
	if got := sessionState(99, 42); got != "active" {
		t.Errorf("different foreground and shell group = %q, want active", got)
	}
}

func TestSessionReadyRequiresServerResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kien.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := readLine(conn); err == nil {
			_, _ = conn.Write([]byte("ok\n"))
		}
	}()

	if !sessionReady(path) {
		t.Error("sessionReady returned false for a responding server")
	}
}

func TestSessionReadyRejectsUnresponsiveSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kien.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	if sessionReady(path) {
		t.Error("sessionReady returned true without a server response")
	}
}

func TestBroadcastRecordsOutputWithoutAttachedClient(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	server := sessionServer{terminal: reader}
	done := make(chan struct{})
	go func() {
		server.broadcast()
		close(done)
	}()
	if _, err := writer.Write([]byte("output before attach")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	<-done

	if got, want := string(server.history), "output before attach"; got != want {
		t.Errorf("history = %q, want %q", got, want)
	}
}
