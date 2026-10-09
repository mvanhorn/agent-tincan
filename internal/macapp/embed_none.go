//go:build !macapp

package macapp

// embeddedZip is empty in builds without the macapp tag (development builds,
// Linux): services are written as before and doctor says so on macOS.
var embeddedZip []byte
