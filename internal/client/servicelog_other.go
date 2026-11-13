//go:build !unix

package client

// stderrIsJournal is false where there is no systemd journal.
func stderrIsJournal() bool { return false }
