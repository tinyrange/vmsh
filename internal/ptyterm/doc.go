// Package ptyterm owns a child process PTY and maintains a structured terminal
// snapshot for vmsh debugging and future multiplexer work.
//
// The emulator intentionally covers a conservative subset of terminal behavior.
// It is useful for process ownership, stdin routing, resize handling, scrollback,
// alternate-screen tracking, basic attributes, and snapshot/restore experiments.
// It is not a complete xterm-compatible renderer; tests should cover user-visible
// PTY behavior and snapshot contracts, with a rendering oracle added when the
// multiplexer UI starts depending on exact terminal output.
package ptyterm
