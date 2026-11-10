//go:build windows

package main

import (
	"os"
	"syscall"
)

// consoleUTF8 reports whether the Windows console can render Unicode box
// drawing characters. The console output code page decides: 65001 (UTF-8)
// can, the legacy code pages (437 / 1252) cannot - and would print the
// box characters as garbage. Windows Terminal always uses UTF-8.
func consoleUTF8() bool {
	if os.Getenv("WT_SESSION") != "" {
		return true // Windows Terminal
	}
	cp, err := consoleOutputCP()
	return err == nil && cp == 65001
}

// consoleOutputCP reads the console output code page from kernel32.
func consoleOutputCP() (uint32, error) {
	mod := syscall.NewLazyDLL("kernel32.dll")
	proc := mod.NewProc("GetConsoleOutputCP")
	r, _, err := proc.Call()
	if r == 0 {
		return 0, err
	}
	return uint32(r), nil
}
