package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// This file is the local side of attachments: reading files to attach and
// saving received ones. The uploader's name is display metadata only and
// never shapes a local path; saved files are named by attachment id.

var (
	// safeID is what an attachment id must look like before it names a
	// local file.
	safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// safeAgent is what an agent name must look like to name a directory.
	safeAgent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

// extByMIME is the allowlist of media types saved with their own
// extension. Anything else is saved as .bin.
var extByMIME = map[string]string{
	"image/png":        ".png",
	"image/jpeg":       ".jpg",
	"image/gif":        ".gif",
	"image/webp":       ".webp",
	"image/heic":       ".heic",
	"application/pdf":  ".pdf",
	"application/json": ".json",
	"application/zip":  ".zip",
	"text/plain":       ".txt",
	"text/markdown":    ".md",
	"text/csv":         ".csv",
	"audio/mpeg":       ".mp3",
	"audio/wav":        ".wav",
	"video/mp4":        ".mp4",
}

// inlineImages are the image types an agent is shown as images.
var inlineImages = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// MediaType is m without parameters, lowercased.
func MediaType(m string) string {
	t, _, err := mime.ParseMediaType(m)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(m))
	}
	return t
}

// ExtForMIME is the file extension for media type m from the allowlist, or
// ".bin".
func ExtForMIME(m string) string {
	if ext, ok := extByMIME[MediaType(m)]; ok {
		return ext
	}
	return ".bin"
}

// InlineImage reports the image type data should be shown as, when both the
// declared type and the content itself say it is an allowed image.
func InlineImage(declared string, data []byte) (string, bool) {
	d, sniffed := MediaType(declared), MediaType(http.DetectContentType(data))
	if inlineImages[d] && d == sniffed {
		return d, true
	}
	return "", false
}

// AttachmentDir is the fixed directory where this agent keeps attachments
// it receives: attachments/<agent> beside its config file. An agent name
// that is not a plain name falls back to "default".
func AttachmentDir(cfg Config) string {
	name := cfg.Agent
	if !safeAgent.MatchString(name) || name == "." || name == ".." {
		name = "default"
	}
	return filepath.Join(filepath.Dir(ConfigPath()), "attachments", name)
}

// SaveAttachmentFile writes data into dir as <id><ext>, ext chosen from
// the allowlist by media type. dir is created 0700 (and tightened to 0700
// if it exists) and the file is 0600. It returns the file's path.
func SaveAttachmentFile(dir, id, mediaType string, data []byte) (string, error) {
	if !safeID.MatchString(id) {
		return "", fmt.Errorf("attachment id %q is not safe to use as a file name", id)
	}
	if err := ensurePrivateDir(dir); err != nil {
		return "", err
	}
	p := filepath.Join(dir, id+ExtForMIME(mediaType))
	return p, writePrivate(p, data)
}

// WritePrivateFile writes data to path with mode 0600, replacing any file
// there by rename so a reader never sees half a file.
func WritePrivateFile(path string, data []byte) error { return writePrivate(path, data) }

func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

func writePrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tincan-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// FetchAttachment downloads attachment id into memory, up to
// MaxAttachmentBytes.
func (r *Relay) FetchAttachment(ctx context.Context, id string) ([]byte, DownloadedAttachment, error) {
	var buf bytes.Buffer
	d, err := r.DownloadAttachment(ctx, id, &buf)
	if err != nil {
		return nil, d, err
	}
	return buf.Bytes(), d, nil
}

// UploadFiles uploads local files as this agent and returns them in order,
// for SendAttached, AskAttached or ReplyAttached. Everything is checked
// before the first upload: the relay must support attachments, the list
// must fit in one message, and each path must be a regular file within the
// relay's per-file cap. Each file's display name is its base name. The
// capability check is made once for the whole batch.
func (r *Relay) UploadFiles(ctx context.Context, paths []string) ([]UploadedAttachment, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	caps, err := r.requireAttachments(ctx)
	if err != nil {
		return nil, err
	}
	most := caps.MaxAttachments
	if most <= 0 {
		most = envelope.MaxAttachments
	}
	if len(paths) > most {
		return nil, fmt.Errorf("%w: %d files, at most %d per message", envelope.ErrTooManyAttachments, len(paths), most)
	}
	limit := caps.MaxAttachmentBytes
	if limit <= 0 {
		limit = MaxAttachmentBytes
	}
	sizes := make([]int64, len(paths))
	for i, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("attach %s: %w", p, err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("attach %s: not a regular file", p)
		}
		if st.Size() > limit {
			return nil, fmt.Errorf("attach %s: file is larger than %d bytes", p, limit)
		}
		sizes[i] = st.Size()
	}
	out := make([]UploadedAttachment, 0, len(paths))
	for i, p := range paths {
		up, err := r.uploadFile(ctx, p, sizes[i])
		if err != nil {
			return nil, fmt.Errorf("attach %s: %w", p, err)
		}
		out = append(out, up)
	}
	return out, nil
}

func (r *Relay) uploadFile(ctx context.Context, path string, size int64) (UploadedAttachment, error) {
	f, err := os.Open(path)
	if err != nil {
		return UploadedAttachment{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return UploadedAttachment{}, err
	}
	if st.Size() != size {
		return UploadedAttachment{}, errors.New("file changed while attaching")
	}
	return r.upload(ctx, filepath.Base(path), MediaType(mime.TypeByExtension(filepath.Ext(path))), f, size)
}

// AttachmentIDs returns the ids of uploaded attachments.
func AttachmentIDs(ups []UploadedAttachment) []string {
	ids := make([]string, len(ups))
	for i, u := range ups {
		ids[i] = u.ID
	}
	return ids
}
