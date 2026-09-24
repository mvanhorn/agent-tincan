package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// uploadPath is the one route exempt from the relay-wide body cap; the
// upload handler applies its own.
const uploadPath = "/v1/attachments"

// AttachmentConfig tunes attachment storage. Zero values take the defaults.
type AttachmentConfig struct {
	MaxFileBytes  int64         // per file (10 MB)
	PerAgentBytes int64         // kept per uploader (200 MB)
	TotalBytes    int64         // kept relay-wide (1 GB)
	MinFreeBytes  int64         // uploads are refused when free disk would drop below this (512 MB)
	OrphanAge     time.Duration // an upload on no message is deleted after this (24h)
	KeepAfterDone time.Duration // files are deleted this long after their request is final (7 days)
	SweepEvery    time.Duration // retention pass interval (1h)
	UploadTimeout time.Duration // read and write deadline for one upload or download (5m)
	// FreeBytes reports free disk space under dir; nil uses the file
	// system. A negative result means unknown, and skips the check.
	FreeBytes func(dir string) (int64, error)
}

func (c *AttachmentConfig) defaults() {
	if c.MaxFileBytes == 0 {
		c.MaxFileBytes = 10 << 20
	}
	if c.PerAgentBytes == 0 {
		c.PerAgentBytes = 200 << 20
	}
	if c.TotalBytes == 0 {
		c.TotalBytes = 1 << 30
	}
	if c.MinFreeBytes == 0 {
		c.MinFreeBytes = 512 << 20
	}
	if c.OrphanAge == 0 {
		c.OrphanAge = 24 * time.Hour
	}
	if c.KeepAfterDone == 0 {
		c.KeepAfterDone = 7 * 24 * time.Hour
	}
	if c.SweepEvery == 0 {
		c.SweepEvery = time.Hour
	}
	if c.UploadTimeout == 0 {
		c.UploadTimeout = 5 * time.Minute
	}
	if c.FreeBytes == nil {
		c.FreeBytes = freeDiskBytes
	}
}

// tempPrefix names in-flight upload files in the attachment directory.
// Finished files are named by id alone, which never starts with a dot.
const tempPrefix = ".upload-"

var (
	errAttachmentsOff = errors.New("attachments are not enabled on this relay")
	errNoAttachment   = errors.New("no such attachment")
	errLowDisk        = errors.New("relay disk space is low; attachment refused")
	attachmentID      = regexp.MustCompile(`^[0-9a-f]{20}$`)
)

// SetAttachmentDir keeps attachment files in dir, created 0700 on first
// use. A relay on a database file defaults to an "attachments" directory
// beside it; an in-memory relay has none until this is called. An empty dir
// turns attachments off, and the relay then stops advertising them.
func (s *Server) SetAttachmentDir(dir string) { s.blobs = dir }

// defaultAttachmentDir is the attachments directory beside the store's
// database file, or "" for an in-memory store.
func defaultAttachmentDir(st *store.Store) string {
	if st.Path() == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(st.Path()), "attachments")
}

// handleCapabilities tells a client what this relay supports, so it sends
// attachments only to a relay that will keep them. An older relay answers
// 404 here, which clients read as no support.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) && s.agent(w, r) == "" {
		return
	}
	caps := client.Capabilities{}
	if s.blobs != "" {
		caps = client.Capabilities{Attachments: true, MaxAttachmentBytes: s.cfg.Attachments.MaxFileBytes, MaxAttachments: envelope.MaxAttachments}
	}
	writeJSON(w, http.StatusOK, caps)
}

// handleUpload stores one attachment for the calling agent. The body is the
// raw file; Content-Type is its media type (detected when missing or
// unusable) and the name query parameter its display name. The quota is
// reserved before any byte is written, the file streams to a temp file under
// the per-file cap while it is hashed, and is renamed to its id once whole.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	dir := s.blobs
	if dir == "" {
		writeErr(w, http.StatusNotFound, errAttachmentsOff)
		return
	}
	cfg := s.cfg.Attachments
	if r.ContentLength > cfg.MaxFileBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("attachment is larger than %d bytes", cfg.MaxFileBytes))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(cfg.UploadTimeout)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	reserve := cfg.MaxFileBytes
	if r.ContentLength >= 0 {
		reserve = r.ContentLength
	}
	if free, err := cfg.FreeBytes(dir); err != nil {
		log.Printf("attachment disk check: %v", err)
	} else if free >= 0 && free-reserve < cfg.MinFreeBytes {
		writeErr(w, http.StatusInsufficientStorage, errLowDisk)
		return
	}
	display := cleanName(r.URL.Query().Get("name"))
	id, err := s.store.ReserveAttachment(r.Context(), name, display, reserve, store.AttachmentQuota{PerAgent: cfg.PerAgentBytes, Total: cfg.TotalBytes})
	if err != nil {
		writeErr(w, attachmentStatus(err, http.StatusInternalServerError), err)
		return
	}
	up, code, err := s.writeBlob(r, dir, id)
	if err != nil {
		// Never leave a reservation behind; the file is already gone.
		if derr := s.store.DropAttachment(context.WithoutCancel(r.Context()), id); derr != nil {
			log.Printf("drop attachment %s: %v", id, derr)
		}
		writeErr(w, code, err)
		return
	}
	up.ID, up.Name = id, display
	s.record(r.Context(), "attachment_uploaded", "", "", name, store.DetailJSON(map[string]any{
		"id": id, "name": display, "mime": up.MIME, "size": up.Size, "sha256": up.SHA256,
	}))
	writeJSON(w, http.StatusCreated, up)
}

// writeBlob streams the upload into dir/id and commits its metadata. On
// failure it removes whatever it wrote and returns the HTTP status to send.
func (s *Server) writeBlob(r *http.Request, dir, id string) (client.UploadedAttachment, int, error) {
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return client.UploadedAttachment{}, http.StatusInternalServerError, err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	head := &headBuffer{}
	n, err := io.Copy(io.MultiWriter(tmp, h, head), r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return client.UploadedAttachment{}, http.StatusRequestEntityTooLarge, fmt.Errorf("attachment is larger than %d bytes", tooBig.Limit)
		}
		return client.UploadedAttachment{}, http.StatusBadRequest, fmt.Errorf("read upload: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return client.UploadedAttachment{}, http.StatusInternalServerError, err
	}
	if err := tmp.Close(); err != nil {
		return client.UploadedAttachment{}, http.StatusInternalServerError, err
	}
	final := filepath.Join(dir, id)
	if err := os.Rename(tmp.Name(), final); err != nil {
		return client.UploadedAttachment{}, http.StatusInternalServerError, err
	}
	done = true
	up := client.UploadedAttachment{MIME: mediaType(r.Header.Get("Content-Type"), head.b), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
	if err := s.store.CommitAttachment(context.WithoutCancel(r.Context()), id, up.Size, up.MIME, up.SHA256); err != nil {
		os.Remove(final)
		return client.UploadedAttachment{}, http.StatusInternalServerError, err
	}
	return up, 0, nil
}

// handleFetch serves an attachment to its uploader, to the sender and
// target of the request that carries it (on the request or its reply), and
// to admins. Anyone else gets the same 404 as for an unknown id.
func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	admin := s.isAdmin(r)
	caller := "admin"
	if !admin {
		if caller = s.agent(w, r); caller == "" {
			return
		}
	}
	dir := s.blobs
	if dir == "" {
		writeErr(w, http.StatusNotFound, errAttachmentsOff)
		return
	}
	id := r.PathValue("id")
	if !attachmentID.MatchString(id) {
		writeErr(w, http.StatusNotFound, errNoAttachment)
		return
	}
	rec, err := s.store.Attachment(r.Context(), id)
	if errors.Is(err, store.ErrAttachmentNotFound) || (err == nil && !rec.Ready) {
		writeErr(w, http.StatusNotFound, errNoAttachment)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !admin && !s.mayFetch(r.Context(), rec, caller) {
		writeErr(w, http.StatusNotFound, errNoAttachment)
		return
	}
	if !rec.DeletedAt.IsZero() {
		writeErr(w, http.StatusGone, store.ErrAttachmentDeleted)
		return
	}
	f, err := os.Open(filepath.Join(dir, rec.ID))
	if err != nil {
		log.Printf("attachment %s: %v", rec.ID, err)
		writeErr(w, http.StatusNotFound, errNoAttachment)
		return
	}
	defer f.Close()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.cfg.Attachments.UploadTimeout))
	ct := rec.MIME
	if ct == "" {
		ct = "application/octet-stream"
	}
	hd := w.Header()
	hd.Set("Content-Type", ct)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set(client.AttachmentSHA256Header, rec.SHA256)
	disposition := "attachment"
	if rec.Name != "" {
		if v := mime.FormatMediaType("attachment", map[string]string{"filename": rec.Name}); v != "" {
			disposition = v
		}
	}
	hd.Set("Content-Disposition", disposition)
	s.record(r.Context(), "attachment_fetched", rec.RequestID, "", caller, store.DetailJSON(map[string]any{"id": rec.ID}))
	http.ServeContent(w, r, "", rec.CreatedAt, f)
}

// mayFetch reports whether agent uploaded the attachment or is the sender
// or target of the request that carries it.
func (s *Server) mayFetch(ctx context.Context, rec store.AttachmentRecord, agent string) bool {
	if rec.Uploader == agent {
		return true
	}
	if rec.RequestID == "" {
		return false
	}
	req, _, err := s.store.Request(ctx, rec.RequestID)
	if err != nil {
		return false
	}
	return req.From == agent || req.To == agent
}

// SweepAttachments applies attachment retention once: the store picks the
// orphans and expired files, and their files are removed here, along with
// temp files left by uploads a crash cut off.
func (s *Server) SweepAttachments(ctx context.Context) {
	dir := s.blobs
	if dir == "" {
		return
	}
	cfg := s.cfg.Attachments
	ids, err := s.store.SweepAttachments(ctx, cfg.OrphanAge, cfg.KeepAfterDone)
	if err != nil {
		log.Printf("attachment sweep: %v", err)
		return
	}
	for _, id := range ids {
		if !attachmentID.MatchString(id) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("attachment sweep: remove %s: %v", id, err)
		}
	}
	if len(ids) > 0 {
		log.Printf("attachment sweep: removed %d files", len(ids))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("attachment sweep: %v", err)
		}
		return
	}
	cutoff := time.Now().Add(-cfg.UploadTimeout)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// attachmentStatus maps attachment errors to HTTP statuses, or returns def.
func attachmentStatus(err error, def int) int {
	switch {
	case errors.Is(err, store.ErrAttachmentNotFound):
		return http.StatusBadRequest
	case errors.Is(err, store.ErrAttachmentNotYours):
		return http.StatusForbidden
	case errors.Is(err, store.ErrAttachmentInUse):
		return http.StatusConflict
	case errors.Is(err, store.ErrAttachmentDeleted):
		return http.StatusGone
	case errors.Is(err, store.ErrAgentQuota):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, store.ErrRelayQuota):
		return http.StatusInsufficientStorage
	}
	return def
}

// mediaType is the client's declared type when it parses and says
// something, else the type detected from the first bytes.
func mediaType(declared string, head []byte) string {
	if declared != "" {
		if mt, _, err := mime.ParseMediaType(declared); err == nil && mt != "application/octet-stream" {
			return mt
		}
	}
	mt, _, err := mime.ParseMediaType(http.DetectContentType(head))
	if err != nil {
		return "application/octet-stream"
	}
	return mt
}

// maxNameBytes bounds a display name.
const maxNameBytes = 200

// cleanName reduces an uploader's file name to display text: its last path
// element, without control characters, at most maxNameBytes. It never
// builds a path; files are stored by id.
func cleanName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "." || name == ".." {
		return ""
	}
	for len(name) > maxNameBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// headBuffer keeps the first 512 bytes written, for content detection.
type headBuffer struct{ b []byte }

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := 512 - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
