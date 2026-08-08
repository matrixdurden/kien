//go:build linux

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const usage = `usage:
  kien new <name>
  kien attach <name>
  kien list
  kien kill <name>`

var errDetach = errors.New("detach")

const detachKey = 0x00

func main() {
	if len(os.Args) < 2 {
		fail(usage)
	}

	var err error
	switch os.Args[1] {
	case "new":
		err = newSession(os.Args[2:])
	case "attach":
		err = attachSession(os.Args[2:])
	case "list":
		err = listSessions(os.Args[2:])
	case "kill":
		err = killSession(os.Args[2:])
	case "serve":
		err = serveSession(os.Args[2:])
	default:
		err = errors.New(usage)
	}
	if err != nil {
		fail(err.Error())
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "kien:", message)
	os.Exit(1)
}

func newSession(args []string) error {
	if len(args) != 1 || !validName(args[0]) {
		return errors.New("new requires a name containing only letters, numbers, _ or -")
	}
	name := args[0]
	path, err := socketPath(name)
	if err != nil {
		return err
	}
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		return fmt.Errorf("session %q already exists", name)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	rows, cols := 24, 80
	if term.IsTerminal(int(os.Stdout.Fd())) {
		if cols, rows, err = term.GetSize(int(os.Stdout.Fd())); err != nil {
			return err
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, "serve", name, strconv.Itoa(rows), strconv.Itoa(cols))
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	command.Stdin = devNull
	command.Stdout = devNull
	command.Stderr = devNull
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	if err := command.Process.Release(); err != nil {
		return err
	}

	for attempt := 0; attempt < 20; attempt++ {
		if conn, err := net.Dial("unix", path); err == nil {
			conn.Close()
			fmt.Println(name)
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("session failed to start")
}

func attachSession(args []string) error {
	if len(args) != 1 || !validName(args[0]) {
		return errors.New("attach requires a valid session name")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("attach requires a terminal")
	}
	conn, err := dial(args[0], "attach")
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprint(os.Stdout, "\033[2J\033[H")
	fmt.Fprintf(os.Stdout, "\033]0;kien | %s | Ctrl-Space detach\a", args[0])

	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), state)

	resize := func() { _ = resizeSession(args[0]) }
	resize()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	defer signal.Stop(signals)
	go func() {
		for range signals {
			resize()
		}
	}()

	detached := make(chan struct{})
	go func() {
		if err := copyInput(conn, os.Stdin); errors.Is(err, errDetach) {
			close(detached)
		}
		_ = conn.Close()
	}()
	_, err = io.Copy(os.Stdout, conn)
	select {
	case <-detached:
		fmt.Fprintf(os.Stdout, "\033[2J\033[HDetached from kien session %q.\r\n\033]0;kien\a", args[0])
		return nil
	default:
	}
	return err
}

func copyInput(destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 1024)
	for {
		count, readErr := source.Read(buffer)
		if count > 0 {
			input := buffer[:count]
			if detachAt := bytes.IndexByte(input, detachKey); detachAt >= 0 {
				if detachAt > 0 {
					if _, err := destination.Write(input[:detachAt]); err != nil {
						return err
					}
				}
				return errDetach
			}
			if _, err := destination.Write(input); err != nil {
				return err
			}
		}
		if readErr != nil {
			return readErr
		}
	}
}

func listSessions(args []string) error {
	if len(args) != 0 {
		return errors.New(usage)
	}
	directory, err := runtimeDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".sock")
		if entry.Type()&os.ModeSocket == 0 || name == entry.Name() || !validName(name) {
			continue
		}
		if _, err := request(filepath.Join(directory, entry.Name()), "status"); err != nil {
			_ = os.Remove(filepath.Join(directory, entry.Name()))
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Println(name)
	}
	return nil
}

func killSession(args []string) error {
	if len(args) != 1 || !validName(args[0]) {
		return errors.New("kill requires a valid session name")
	}
	path, err := socketPath(args[0])
	if err != nil {
		return err
	}
	_, err = request(path, "kill")
	return err
}

func resizeSession(name string) error {
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return err
	}
	path, err := socketPath(name)
	if err != nil {
		return err
	}
	_, err = request(path, fmt.Sprintf("resize %d %d", rows, cols))
	return err
}

func serveSession(args []string) error {
	if len(args) != 3 || !validName(args[0]) {
		return errors.New("invalid session server arguments")
	}
	rows, err := strconv.Atoi(args[1])
	if err != nil {
		return err
	}
	cols, err := strconv.Atoi(args[2])
	if err != nil {
		return err
	}
	path, err := socketPath(args[0])
	if err != nil {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	command := exec.Command(shell)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return err
	}
	defer terminal.Close()

	server := sessionServer{terminal: terminal, command: command}
	go server.accept(listener)
	return command.Wait()
}

type sessionServer struct {
	terminal      *os.File
	command       *exec.Cmd
	clients       map[net.Conn]struct{}
	clientsMu     sync.Mutex
	terminalMu    sync.Mutex
	broadcastOnce sync.Once
}

func (s *sessionServer) accept(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *sessionServer) handle(conn net.Conn) {
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}
	command := strings.Fields(line)
	if len(command) == 0 {
		conn.Close()
		return
	}
	switch command[0] {
	case "status":
		_, _ = io.WriteString(conn, "ok\n")
		conn.Close()
	case "kill":
		_ = s.command.Process.Kill()
		_, _ = io.WriteString(conn, "ok\n")
		conn.Close()
	case "resize":
		if len(command) != 3 {
			_, _ = io.WriteString(conn, "error invalid resize\n")
			conn.Close()
			return
		}
		rows, rowErr := strconv.Atoi(command[1])
		cols, colErr := strconv.Atoi(command[2])
		if rowErr != nil || colErr != nil || rows < 1 || cols < 1 {
			_, _ = io.WriteString(conn, "error invalid resize\n")
		} else {
			_ = pty.Setsize(s.terminal, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
			_, _ = io.WriteString(conn, "ok\n")
		}
		conn.Close()
	case "attach":
		_, _ = io.WriteString(conn, "ok\n")
		s.addClient(conn)
		s.broadcastOnce.Do(func() { go s.broadcast() })
		_, _ = io.Copy(writerFunc(s.writeInput), reader)
		s.removeClient(conn)
		conn.Close()
	default:
		_, _ = io.WriteString(conn, "error unknown command\n")
		conn.Close()
	}
}

func (s *sessionServer) broadcast() {
	buffer := make([]byte, 32768)
	for {
		count, err := s.terminal.Read(buffer)
		if count > 0 {
			for _, client := range s.snapshotClients() {
				if _, writeErr := client.Write(buffer[:count]); writeErr != nil {
					s.removeClient(client)
					client.Close()
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *sessionServer) writeInput(input []byte) (int, error) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	return s.terminal.Write(input)
}

func (s *sessionServer) addClient(client net.Conn) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	if s.clients == nil {
		s.clients = make(map[net.Conn]struct{})
	}
	s.clients[client] = struct{}{}
}

func (s *sessionServer) removeClient(client net.Conn) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	delete(s.clients, client)
}

func (s *sessionServer) snapshotClients() []net.Conn {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	clients := make([]net.Conn, 0, len(s.clients))
	for client := range s.clients {
		clients = append(clients, client)
	}
	return clients
}

type writerFunc func([]byte) (int, error)

func (write writerFunc) Write(input []byte) (int, error) {
	return write(input)
}

func dial(name, command string) (net.Conn, error) {
	path, err := socketPath(name)
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("session %q does not exist", name)
	}
	if _, err := io.WriteString(conn, command+"\n"); err != nil {
		conn.Close()
		return nil, err
	}
	response, err := readLine(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if response != "ok\n" {
		conn.Close()
		return nil, errors.New(strings.TrimSpace(response))
	}
	return conn, nil
}

func request(path, command string) (string, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return "", errors.New("session does not exist")
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, command+"\n"); err != nil {
		return "", err
	}
	response, err := readLine(conn)
	if err != nil {
		return "", err
	}
	if response != "ok\n" {
		return "", errors.New(strings.TrimSpace(response))
	}
	return response, nil
}

func runtimeDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), "kien-"+strconv.Itoa(os.Getuid()))
	}
	directory := filepath.Join(base, "kien")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	return directory, nil
}

// readLine reads protocol responses without buffering later PTY output.
func readLine(reader io.Reader) (string, error) {
	var line strings.Builder
	buffer := []byte{0}
	for line.Len() < 4096 {
		if _, err := reader.Read(buffer); err != nil {
			return "", err
		}
		line.WriteByte(buffer[0])
		if buffer[0] == '\n' {
			return line.String(), nil
		}
	}
	return "", errors.New("protocol response is too large")
}

func socketPath(name string) (string, error) {
	directory, err := runtimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, name+".sock"), nil
}

func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, character := range name {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}
