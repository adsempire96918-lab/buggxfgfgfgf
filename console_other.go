//go:build !windows

package main

// consoleUTF8 reports whether the terminal can render Unicode box drawing
// characters. Everything except Windows handles UTF-8 natively.
func consoleUTF8() bool { return true }
