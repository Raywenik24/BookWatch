//go:build portable

package main

// portable is true for the bookwatch-portable.exe build (go build -tags
// portable) — no-arg (double-click) starts the server instead of a check.
const portable = true
