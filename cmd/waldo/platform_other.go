//go:build !windows

package main

// platformCheck reports whether waldo can run here.
//
// Every Unix waldo builds for provides what it needs: execve, symlinks, and an
// ssh client with ControlMaster. Linux and macOS are tested on every commit,
// against both GNU and BSD userlands; see the platform table in the README for
// what that covers on the target side.
func platformCheck() error { return nil }

// execUnsupported reports whether an execve failure means the platform has no
// execve at all, rather than that this particular call went wrong. On Unix it
// always has one, so any failure is worth telling the operator about.
func execUnsupported(error) bool { return false }
