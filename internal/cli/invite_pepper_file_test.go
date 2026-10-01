package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Explicit dependency injection models an opener returning a different file.
// No concurrent path mutation, live state, or secret values are used.
func TestInvitePepperValidatesOpenedDescriptor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invite-pepper")
	other := filepath.Join(dir, "other-fixture")
	fixture := bytes.Repeat([]byte{0x42}, 32)
	for _, p := range []string{path, other} {
		if err := os.WriteFile(p, fixture, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(other, 0644); err != nil {
		t.Fatal(err)
	}
	var opened *os.File
	got, err := readInvitePepperFileWithOpen(path, func(string) (*os.File, error) {
		var openErr error
		opened, openErr = os.Open(other)
		return opened, openErr
	})
	if err == nil || got != nil {
		t.Fatal("accepted an opened file whose permissions violate the contract")
	}
	if opened == nil {
		t.Fatal("opener was not exercised")
	}
	if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("rejected descriptor was not closed")
	}
}

func TestInvitePepperReaderRejectsInvalidFiles(t *testing.T) {
	for _, name := range []string{"empty", "short", "long", "permissive", "directory", "symlink", "dangling"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "invite-pepper")
			fixture := bytes.Repeat([]byte{0x42}, 32)
			var err error
			switch name {
			case "directory":
				err = os.Mkdir(path, 0700)
			case "symlink", "dangling":
				target := filepath.Join(dir, "fixture-target")
				if name == "symlink" {
					if err := os.WriteFile(target, fixture, 0600); err != nil {
						t.Fatal(err)
					}
				}
				err = os.Symlink(target, path)
			default:
				switch name {
				case "empty":
					fixture = nil
				case "short":
					fixture = fixture[:31]
				case "long":
					fixture = append(fixture, 0)
				}
				err = os.WriteFile(path, fixture, 0600)
				if err == nil && name == "permissive" {
					err = os.Chmod(path, 0644)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := readInvitePepperFile(path); err == nil || got != nil {
				t.Fatal("invalid file was accepted")
			}
		})
	}
}

func TestInvitePepperMissingFilePreservesError(t *testing.T) {
	if _, err := readInvitePepperFile(filepath.Join(t.TempDir(), "absent")); !os.IsNotExist(err) {
		t.Fatal("missing-file error no longer allows initialization")
	}
}
