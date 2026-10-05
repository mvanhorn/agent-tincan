package history

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mvanhorn/agent-tincan/internal/client"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const maxInputPixels = 40_000_000

type stagedInput struct {
	InputFile
	path string
}

type inputWriter struct {
	w         io.Writer
	remaining int64
}

func (w *inputWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("image exceeds the remaining byte budget")
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func validateInputMetadata(attachments []envelope.Attachment) error {
	if len(attachments) < 1 || len(attachments) > MaxInputFiles {
		return errors.New("expected one to four PNG/JPEG images")
	}
	var total int64
	for _, a := range attachments {
		if a.ID == "" {
			return fmt.Errorf("%q: missing relay attachment id", a.Name)
		}
		// Reuse transport validation, with a placeholder hash before download.
		if err := validateInputFiles([]InputFile{{Name: a.Name, MIME: a.MIME, Size: a.Size, SHA256: strings.Repeat("0", 64)}}); err != nil {
			return fmt.Errorf("%q: %w", a.Name, err)
		}
		total += a.Size
		if total > MaxInputTotalBytes {
			return fmt.Errorf("%q: images exceed 20 MiB total", a.Name)
		}
	}
	return nil
}

func validateInputImage(data []byte, declared string) error {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return errors.New("corrupt or unsupported image")
	}
	if (format != "png" || declared != "image/png") && (format != "jpeg" || declared != "image/jpeg") {
		return errors.New("image bytes do not match PNG/JPEG MIME type")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxInputPixels {
		return errors.New("image exceeds 40 million pixels")
	}
	if format == "png" {
		// APNG has the PNG signature too. Walk chunk lengths, not byte searches
		// in compressed pixels, to reject its animation control chunk.
		for pos := 8; pos+12 <= len(data); {
			n := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
			if n > int64(len(data)-pos-12) {
				return errors.New("corrupt PNG chunk")
			}
			if string(data[pos+4:pos+8]) == "acTL" {
				return errors.New("animated PNG is unsupported")
			}
			pos += int(n) + 12
		}
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return errors.New("corrupt image pixels")
	}
	return nil
}

// stageInputs is called only after claim, chain authorization, threading,
// journal and capability checks. Sender filenames never become paths.
func (w *WebAgent) stageInputs(ctx context.Context, attachments []envelope.Attachment) ([]stagedInput, func(), error) {
	if err := validateInputMetadata(attachments); err != nil {
		return nil, func() {}, err
	}
	caps, err := w.Relay.Capabilities(ctx)
	if err != nil || !caps.Attachments {
		return nil, func() {}, errors.New("relay image capabilities unavailable; upgrade the relay")
	}
	if caps.MaxAttachments > 0 && len(attachments) > caps.MaxAttachments {
		return nil, func() {}, fmt.Errorf("%q: request exceeds the relay's attachment count limit", attachments[caps.MaxAttachments].Name)
	}
	for _, a := range attachments {
		if caps.MaxAttachmentBytes > 0 && a.Size > caps.MaxAttachmentBytes {
			return nil, func() {}, fmt.Errorf("%q: exceeds the relay's attachment byte limit", a.Name)
		}
	}
	dir, err := os.MkdirTemp(w.TempDir, "tincan-input-*")
	if err != nil {
		return nil, func() {}, errors.New("could not stage input images")
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	ok := false
	defer func() {
		if !ok {
			cleanup()
		}
	}()
	var out []stagedInput
	remaining := int64(MaxInputTotalBytes)
	for i, a := range attachments {
		path := filepath.Join(dir, fmt.Sprintf("%d", i))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, cleanup, fmt.Errorf("%q: could not stage image", a.Name)
		}
		downloaded, err := func() (out client.DownloadedAttachment, err error) {
			defer func() {
				if closeErr := f.Close(); err == nil {
					err = closeErr
				}
			}()
			return w.Relay.DownloadAttachment(ctx, a.ID, &inputWriter{w: f, remaining: min(remaining, int64(MaxInputFileBytes))})
		}()
		if err != nil {
			return nil, cleanup, fmt.Errorf("%q: %s", a.Name, inputDownloadFailure(err))
		}
		if !downloaded.Verified {
			return nil, cleanup, fmt.Errorf("%q: relay did not supply an image checksum", a.Name)
		}
		actualMIME, _, err := mime.ParseMediaType(downloaded.MIME)
		if err != nil || actualMIME != a.MIME || downloaded.Size != a.Size {
			return nil, cleanup, fmt.Errorf("%q: downloaded size or MIME differs from relay metadata", a.Name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, cleanup, fmt.Errorf("%q: could not read staged image", a.Name)
		}
		if err := validateInputImage(data, a.MIME); err != nil {
			return nil, cleanup, fmt.Errorf("%q: %w", a.Name, err)
		}
		remaining -= downloaded.Size
		out = append(out, stagedInput{InputFile: InputFile{Name: a.Name, MIME: a.MIME, Size: downloaded.Size, SHA256: downloaded.SHA256}, path: path})
	}
	ok = true
	return out, cleanup, nil
}

// Reconcile only uniquely identifiable human-image evidence in the current
// branch of a known conversation. New-chat intents cannot be located safely.
func (w *WebAgent) reconcileInput(ctx context.Context, message string, files []envelope.Attachment, e webSend) (webSend, bool) {
	if w.Site != SourceChatGPT || e.ConversationID == "" || e.SubmittedAt.IsZero() {
		return e, false
	}
	raw, err := w.Native.Request(ctx, OpChatGPTDetail, OpArgs{ID: e.ConversationID})
	if err != nil {
		return e, false
	}
	var d cgDetail
	if json.Unmarshal(raw, &d) != nil {
		return e, false
	}
	seen := map[string]bool{}
	matches := []string{}
	reachedPrevious := e.PrevUserID == ""
	for id := d.CurrentNode; id != "" && !seen[id]; {
		seen[id] = true
		node, ok := d.Mapping[id]
		if !ok {
			break
		}
		m := node.Message
		if m != nil && m.ID == e.PrevUserID {
			reachedPrevious = true
			break
		}
		if m != nil && m.Author.Role == "user" && !m.CreateTime.Before(e.SubmittedAt.Add(-time.Second)) {
			text, pointers := m.parts()
			if text == message && len(pointers) == len(files) && len(m.Metadata.Attachments) == len(files) {
				match := true
				for i, f := range files {
					a := m.Metadata.Attachments[i]
					if a.Name != f.Name || a.MimeType != f.MIME || chatgptPointerID(pointers[i]) != a.ID {
						match = false
					}
				}
				if match && validNativeID(m.ID) {
					matches = append(matches, m.ID)
				}
			}
		}
		id = node.Parent
	}
	if !reachedPrevious || len(matches) != 1 {
		return e, false
	}
	e.UserMessageID, e.State = matches[0], webSendSent
	return e, true
}

func inputDownloadFailure(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "download cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "download timed out"
	case client.IsStatus(err, http.StatusNotFound):
		return "relay attachment is missing or deleted"
	case client.IsStatus(err, http.StatusForbidden), client.IsStatus(err, http.StatusUnauthorized):
		return "relay attachment access denied"
	case strings.Contains(err.Error(), "sha256"):
		return "relay checksum mismatch"
	case strings.Contains(err.Error(), "larger than"), strings.Contains(err.Error(), "budget"):
		return "image exceeds byte limit"
	default:
		return "relay attachment download or staging failed"
	}
}

func inputNames(files []envelope.Attachment) string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = fmt.Sprintf("%q", f.Name)
	}
	return strings.Join(names, ", ")
}
