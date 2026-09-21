//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
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
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const usage = `usage:
  kien new <name> [-- command [args...]]
  kien attach <name>
  kien state <name>
  kien send <name> <text>
  kien list
  kien kill <name>`

var errDetach = errors.New("detach")

const detachKey = 0x00

// RIS resets the terminal emulator, including xterm.js's scrollback buffer.
const clearTerminal = "\033c\033[2J\033[H"

const alternateScreenEnter = "\033[?1049h"
const alternateScreenExit = "\033[?1049l"

// historyLimit lets a newly attached terminal reconstruct full-screen programs
// that rendered before it connected, without allowing an idle session to grow
// memory without bound.
const historyLimit = 4 << 20

// A disconnected terminal can otherwise stop the PTY reader for every client.
const clientWriteTimeout = time.Second

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
	case "state":
		err = printSessionState(os.Args[2:])
	case "send":
		err = sendSession(os.Args[2:])
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
	if len(args) == 0 || !validName(args[0]) {
		return errors.New("new requires a name containing only letters, numbers, _ or -")
	}
	program := args[1:]
	if len(program) > 0 && (program[0] != "--" || len(program) == 1) {
		return errors.New("command must follow --")
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
	serverArgs := []string{"serve", name, strconv.Itoa(rows), strconv.Itoa(cols)}
	if len(program) > 0 {
		serverArgs = append(serverArgs, program[1:]...)
	}
	command := exec.Command(executable, serverArgs...)
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
		if sessionReady(path) {
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
	// Clear both the screen and its scrollback before replaying the session.
	fmt.Fprint(os.Stdout, clearTerminal)
	conn, err := dial(args[0], "attach")
	if err != nil {
		return err
	}
	defer conn.Close()

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
		fmt.Fprintf(os.Stdout, "\033[2J\033[HDetached from kien session %q.\r\n", args[0])
		return nil
	default:
	}
	return err
}

func printSessionState(args []string) error {
	if len(args) != 1 || !validName(args[0]) {
		return errors.New("state requires a valid session name")
	}
	path, err := socketPath(args[0])
	if err != nil {
		return err
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return errors.New("session does not exist")
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "state\n"); err != nil {
		return err
	}
	state, err := readLine(conn)
	if err != nil {
		return err
	}
	if state != "idle\n" && state != "active\n" {
		return errors.New(strings.TrimSpace(state))
	}
	fmt.Print(state)
	return nil
}

func sendSession(args []string) error {
	if len(args) != 2 || !validName(args[0]) {
		return errors.New("send requires a valid session name and text")
	}
	path, err := socketPath(args[0])
	if err != nil {
		return err
	}
	input := base64.StdEncoding.EncodeToString([]byte(args[1] + "\n"))
	_, err = request(path, "send "+input)
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

func killProcessSession(sessionID int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		_ = unix.Kill(-sessionID, syscall.SIGKILL)
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		currentSessionID, err := unix.Getsid(pid)
		if err == nil && currentSessionID == sessionID {
			_ = unix.Kill(pid, syscall.SIGKILL)
		}
	}
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
	if len(args) < 3 || !validName(args[0]) {
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

	program := args[3:]
	if len(program) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		program = []string{shell}
	}
	command := exec.Command(program[0], program[1:]...)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return err
	}
	defer terminal.Close()
	defer killProcessSession(command.Process.Pid)

	server := sessionServer{terminal: terminal, command: command}
	go server.broadcast()
	go server.accept(listener)
	return command.Wait()
}

type sessionServer struct {
	terminal        *os.File
	command         *exec.Cmd
	clients         map[*sessionClient]struct{}
	clientsMu       sync.Mutex
	terminalMu      sync.Mutex
	outputMu        sync.Mutex
	history         []byte
	escapeTail      []byte
	alternateScreen bool
}

type sessionClient struct {
	conn    net.Conn
	writeMu sync.Mutex
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
		_, _ = io.WriteString(conn, "ok\n")
		conn.Close()
		killProcessSession(s.command.Process.Pid)
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
	case "state":
		foreground, err := unix.IoctlGetInt(int(s.terminal.Fd()), unix.TIOCGPGRP)
		if err != nil {
			_, _ = io.WriteString(conn, "error unable to read session state\n")
		} else {
			_, _ = io.WriteString(conn, sessionState(foreground, s.command.Process.Pid)+"\n")
		}
		conn.Close()
	case "send":
		if len(command) != 2 {
			_, _ = io.WriteString(conn, "error invalid input\n")
			conn.Close()
			return
		}
		input, err := base64.StdEncoding.DecodeString(command[1])
		if err != nil {
			_, _ = io.WriteString(conn, "error invalid input\n")
		} else if _, err := s.writeInput(input); err != nil {
			_, _ = io.WriteString(conn, "error unable to send input\n")
		} else {
			_, _ = io.WriteString(conn, "ok\n")
		}
		conn.Close()
	case "attach":
		_, _ = io.WriteString(conn, "ok\n")
		client, needsRedraw, err := s.addClient(conn)
		if err != nil {
			conn.Close()
			return
		}
		if needsRedraw {
			_ = s.command.Process.Signal(syscall.SIGWINCH)
		}
		_, _ = io.Copy(writerFunc(s.writeInput), reader)
		s.removeClient(client)
		conn.Close()
	default:
		_, _ = io.WriteString(conn, "error unknown command\n")
		conn.Close()
	}
}

func sessionState(foreground, shell int) string {
	if foreground == shell {
		return "idle"
	}
	return "active"
}

func (s *sessionServer) broadcast() {
	buffer := make([]byte, 32768)
	for {
		count, err := s.terminal.Read(buffer)
		if count > 0 {
			s.outputMu.Lock()
			s.appendHistory(buffer[:count])
			clients := s.snapshotClients()
			s.outputMu.Unlock()
			for _, client := range clients {
				writeErr := client.write(buffer[:count])
				if writeErr != nil {
					s.removeClient(client)
					client.conn.Close()
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

func (s *sessionServer) addClient(conn net.Conn) (*sessionClient, bool, error) {
	client := &sessionClient{conn: conn}
	// Hold this lock until the replay is complete so live output cannot overtake it.
	client.writeMu.Lock()
	defer client.writeMu.Unlock()

	s.outputMu.Lock()
	needsRedraw := s.alternateScreen
	history := append([]byte(nil), s.history...)
	s.clientsMu.Lock()
	if s.clients == nil {
		s.clients = make(map[*sessionClient]struct{})
	}
	s.clients[client] = struct{}{}
	s.clientsMu.Unlock()
	s.outputMu.Unlock()

	if needsRedraw {
		if err := client.writeLocked([]byte(alternateScreenEnter)); err != nil {
			s.removeClient(client)
			return nil, false, err
		}
	} else if len(history) > 0 {
		if err := client.writeLocked(history); err != nil {
			s.removeClient(client)
			return nil, false, err
		}
	}
	return client, needsRedraw, nil
}

func (s *sessionServer) appendHistory(output []byte) {
	s.updateScreenMode(output)
	if len(output) >= historyLimit {
		s.history = append(s.history[:0], output[len(output)-historyLimit:]...)
		return
	}
	overflow := len(s.history) + len(output) - historyLimit
	if overflow > 0 {
		copy(s.history, s.history[overflow:])
		s.history = s.history[:len(s.history)-overflow]
	}
	s.history = append(s.history, output...)
}

func (s *sessionServer) updateScreenMode(output []byte) {
	sequence := append(append([]byte{}, s.escapeTail...), output...)
	enter := bytes.LastIndex(sequence, []byte(alternateScreenEnter))
	exit := bytes.LastIndex(sequence, []byte(alternateScreenExit))
	if enter >= 0 || exit >= 0 {
		s.alternateScreen = enter > exit
	}
	const maxSequencePrefix = len(alternateScreenEnter) - 1
	if len(sequence) > maxSequencePrefix {
		sequence = sequence[len(sequence)-maxSequencePrefix:]
	}
	s.escapeTail = append(s.escapeTail[:0], sequence...)
}

func (s *sessionServer) removeClient(client *sessionClient) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	delete(s.clients, client)
}

func (s *sessionServer) snapshotClients() []*sessionClient {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	clients := make([]*sessionClient, 0, len(s.clients))
	for client := range s.clients {
		clients = append(clients, client)
	}
	return clients
}

func (client *sessionClient) write(output []byte) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	return client.writeLocked(output)
}

func (client *sessionClient) writeLocked(output []byte) error {
	_ = client.conn.SetWriteDeadline(time.Now().Add(clientWriteTimeout))
	_, err := client.conn.Write(output)
	_ = client.conn.SetWriteDeadline(time.Time{})
	return err
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

func sessionReady(path string) bool {
	conn, err := net.DialTimeout("unix", path, 25*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		return false
	}
	if _, err := io.WriteString(conn, "status\n"); err != nil {
		return false
	}
	response, err := readLine(conn)
	return err == nil && response == "ok\n"
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
