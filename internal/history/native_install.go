package history

// Installing the native host: the wrapper script Chrome runs and the host
// manifest that names it, per OS. See native.go for how the host works.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// HostManifest is Chrome's native messaging host manifest.
type HostManifest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Path           string   `json:"path"`
	Type           string   `json:"type"`
	AllowedOrigins []string `json:"allowed_origins"`
}

// NativeManifestDir is where Chrome looks for per-user native messaging
// host manifests. Windows finds manifests through the registry, so the
// directory there is Tincan's own and a registry entry must point at it.
func NativeManifestDir(goos, home string) (string, error) {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts"), nil
	case "linux":
		return filepath.Join(home, ".config", "google-chrome", "NativeMessagingHosts"), nil
	case "windows":
		return filepath.Join(home, "AppData", "Local", "AgentTincan", "NativeMessagingHosts"), nil
	}
	return "", fmt.Errorf("native messaging is not supported on %s", goos)
}

var extensionIDPattern = regexp.MustCompile(`^[a-p]{32}$`)

// ValidExtensionID reports whether id has Chrome's extension id shape.
func ValidExtensionID(id string) bool { return extensionIDPattern.MatchString(id) }

// ExtensionIDFromKey derives a Chrome extension id from a manifest "key".
func ExtensionIDFromKey(key string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil || len(der) == 0 {
		return "", errors.New("manifest key is not base64")
	}
	sum := sha256.Sum256(der)
	h := hex.EncodeToString(sum[:16])
	id := make([]byte, len(h))
	for i := range len(h) {
		c := h[i]
		if c <= '9' {
			id[i] = 'a' + (c - '0')
		} else {
			id[i] = 'a' + 10 + (c - 'a')
		}
	}
	return string(id), nil
}

// InstallOptions configure InstallNativeHost. Empty fields default to the
// running system.
type InstallOptions struct {
	GOOS        string
	Home        string
	Binary      string
	ExtensionID string
	NativeDir   string
	// ExtensionDir is the unpacked extension directory. When set, the
	// wrapper passes it to the host, which then reloads the extension
	// whenever the files there change.
	ExtensionDir string
}

// InstallResult says what InstallNativeHost wrote.
type InstallResult struct {
	ManifestPath string
	WrapperPath  string
	// Note is a step the user must still take (the Windows registry
	// entry), empty when none.
	Note string
}

// InstallNativeHost writes the native host wrapper script and the host
// manifest for the current user. Chrome passes the extension origin as an
// argument and cannot add its own, so the manifest points at a wrapper that
// runs `tincan history native-host`.
func InstallNativeHost(o InstallOptions) (InstallResult, error) {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return InstallResult{}, err
		}
		o.Home = h
	}
	if o.ExtensionID == "" {
		o.ExtensionID = DefaultExtensionID
	}
	if !ValidExtensionID(o.ExtensionID) {
		return InstallResult{}, fmt.Errorf("invalid extension id %q (want 32 letters a-p)", o.ExtensionID)
	}
	if o.Binary == "" {
		b, err := os.Executable()
		if err != nil {
			return InstallResult{}, err
		}
		o.Binary = b
	}
	if o.NativeDir == "" {
		o.NativeDir = NativeDir()
	}
	if o.ExtensionDir != "" {
		if !filepath.IsAbs(o.ExtensionDir) || strings.ContainsAny(o.ExtensionDir, "\"'\n\r\x00%") {
			return InstallResult{}, fmt.Errorf("extension dir %q must be an absolute path without quotes, %% or newlines", o.ExtensionDir)
		}
		if _, err := os.Stat(filepath.Join(o.ExtensionDir, "manifest.json")); err != nil {
			return InstallResult{}, fmt.Errorf("extension dir %s has no manifest.json", o.ExtensionDir)
		}
	}
	mdir, err := NativeManifestDir(o.GOOS, o.Home)
	if err != nil {
		return InstallResult{}, err
	}
	if err := os.MkdirAll(o.NativeDir, 0o700); err != nil {
		return InstallResult{}, err
	}
	if err := os.Chmod(o.NativeDir, 0o700); err != nil {
		return InstallResult{}, err
	}
	var res InstallResult
	var script string
	if o.GOOS == "windows" {
		res.WrapperPath = filepath.Join(o.NativeDir, "native-host.bat")
		script = "@echo off\r\n"
		if o.ExtensionDir != "" {
			script += "set \"" + ExtensionDirEnv + "=" + o.ExtensionDir + "\"\r\n"
		}
		script += "\"" + o.Binary + "\" history native-host %*\r\n"
	} else {
		res.WrapperPath = filepath.Join(o.NativeDir, "native-host")
		env := ""
		if o.ExtensionDir != "" {
			env = "export " + ExtensionDirEnv + "=" + shellQuote(o.ExtensionDir) + "\n"
		}
		script = "#!/bin/sh\n" + env + "exec " + shellQuote(o.Binary) + " history native-host \"$@\"\n"
	}
	if err := writeFileAtomic(res.WrapperPath, []byte(script), 0o700); err != nil {
		return InstallResult{}, err
	}
	m := HostManifest{
		Name:           NativeHostName,
		Description:    "Agent Tincan history bridge",
		Path:           res.WrapperPath,
		Type:           "stdio",
		AllowedOrigins: []string{"chrome-extension://" + o.ExtensionID + "/"},
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return InstallResult{}, err
	}
	if err := os.MkdirAll(mdir, 0o755); err != nil {
		return InstallResult{}, err
	}
	res.ManifestPath = filepath.Join(mdir, NativeHostName+".json")
	if err := writeFileAtomic(res.ManifestPath, append(b, '\n'), 0o644); err != nil {
		return InstallResult{}, err
	}
	if o.GOOS == "windows" {
		res.Note = `Chrome on Windows finds native hosts through the registry. Run: reg add "HKCU\Software\Google\Chrome\NativeMessagingHosts\` +
			NativeHostName + `" /ve /t REG_SZ /d "` + res.ManifestPath + `" /f`
	}
	return res, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
