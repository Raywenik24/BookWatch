//go:build !portable

package main

// portable is false for the standard bookwatch.exe build — no-arg runs a
// one-shot check (see main.go), same as always.
const portable = false
