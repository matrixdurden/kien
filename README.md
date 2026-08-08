# kien

`kien` is a deliberately small Linux terminal-session manager. It keeps a shell alive in a PTY and forwards terminal bytes unchanged, so full-screen terminal applications such as `opencode`, Vim, htop, and lazygit work normally.

There is no configuration file, prefix key, status bar, pane support, or command mode.

## Commands

```sh
kien new work       # create a session
kien attach work    # connect the current terminal
kien list            # print session names
kien kill work       # terminate a session
```

`new` prints the session name after it starts. The shell comes from `$SHELL`, falling back to `/bin/sh`. A shell exit also ends its kien session.

Any number of terminals may attach to the same session. Every attached terminal sees the same output and may send input, so coordinate with other users when typing. While attached, kien puts the terminal in raw mode and passes input and output through unchanged. Terminal resize events are forwarded to the PTY; the most recently resized terminal determines the shared size.

The terminal title shows `kien | session-name | Ctrl-Space detach`. Press `Ctrl-Space` to detach without ending the shared shell.

## Linux support

Kien supports Linux systems with a POSIX PTY, Unix-domain sockets, and a shell. Its Go build works on common `linux/amd64` and `linux/arm64` systems, including Debian, Ubuntu, Fedora, Arch, Alpine, and NixOS.

It does not currently support macOS or Windows. Session sockets are private to the current user and live in `$XDG_RUNTIME_DIR/kien` or, when that variable is unavailable, `/tmp/kien-UID/kien`.

## Install

Clone the repository, then run one command. Go 1.22 or newer is required.

```sh
git clone https://github.com/matrixdurden/kien.git
cd kien
./install.sh
```

The script builds kien and installs it to `$XDG_BIN_HOME/kien`, or `~/.local/bin/kien` by default. It tells you if that directory is not on `PATH`.

## Remove

From the cloned repository:

```sh
./uninstall.sh
```

It ends the current user's kien sessions, then removes `$XDG_BIN_HOME/kien` or `~/.local/bin/kien`. Any empty leftover socket directory is safe to remove.
