package history

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	InputChunkBytes    = 96 << 10
	MaxInputFiles      = 4
	MaxInputFileBytes  = 10 << 20
	MaxInputTotalBytes = 20 << 20
)

// ImageInputCapability describes loaded code, not the manifest version.
type ImageInputCapability struct {
	Version int      `json:"version"`
	Sites   []string `json:"sites"`
}

type InputFile struct {
	Name   string `json:"name"`
	MIME   string `json:"mime"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// InputArgs is separate from comparable OpArgs. Connection is assigned by
// the host, never accepted from a service request.
type InputArgs struct {
	Connection string      `json:"connection,omitempty"`
	Files      []InputFile `json:"files,omitempty"`
	Token      string      `json:"token,omitempty"`
	Seq        int         `json:"seq,omitempty"`
	Data       string      `json:"data,omitempty"`
}

func inputOp(op Op) bool {
	switch op {
	case "chatgpt.input_begin", "chatgpt.input_chunk", "chatgpt.input_abort", "chatgpt.send_images":
		return true
	}
	return false
}

func validateInputFiles(files []InputFile) error {
	if len(files) < 1 || len(files) > MaxInputFiles {
		return errors.New("expected one to four PNG/JPEG images")
	}
	var total int64
	for _, f := range files {
		if f.Name == "" || len(f.Name) > 255 || !utf8.ValidString(f.Name) || strings.ContainsAny(f.Name, `/\`) || strings.ContainsFunc(f.Name, unicode.IsControl) {
			return errors.New("invalid image name")
		}
		if f.MIME != "image/png" && f.MIME != "image/jpeg" {
			return fmt.Errorf("%q: expected PNG or JPEG", f.Name)
		}
		if f.Size <= 0 || f.Size > MaxInputFileBytes {
			return fmt.Errorf("%q: image exceeds 10 MiB or is empty", f.Name)
		}
		hash, err := hex.DecodeString(f.SHA256)
		if err != nil || len(hash) != 32 || strings.ToLower(f.SHA256) != f.SHA256 {
			return errors.New("invalid image checksum")
		}
		total += f.Size
	}
	if total > MaxInputTotalBytes {
		return errors.New("images exceed 20 MiB total")
	}
	return nil
}

func validateInputRequest(req NativeRequest) error {
	a := req.Input
	if !inputOp(req.Op) || a == nil || a.Connection != "" {
		return errors.New("invalid image operation")
	}
	if req.Op == "chatgpt.send_images" {
		if err := ValidateOp(OpChatGPTSend, req.Args); err != nil {
			return err
		}
	} else if req.Args != (OpArgs{}) {
		return errors.New("unexpected image arguments")
	}
	if req.Op == "chatgpt.input_begin" {
		if a.Token != "" || a.Seq != 0 || a.Data != "" {
			return errors.New("unexpected begin arguments")
		}
		return validateInputFiles(a.Files)
	}
	token, err := hex.DecodeString(a.Token)
	if err != nil || len(token) != 16 || strings.ToLower(a.Token) != a.Token || len(a.Files) != 0 {
		return errors.New("invalid image token")
	}
	if req.Op == "chatgpt.input_chunk" {
		if a.Seq < 0 || len(a.Data) == 0 || len(a.Data) > base64.StdEncoding.EncodedLen(InputChunkBytes) {
			return errors.New("invalid image chunk")
		}
		b, err := base64.StdEncoding.Strict().DecodeString(a.Data)
		if err != nil || len(b) > InputChunkBytes || base64.StdEncoding.EncodeToString(b) != a.Data {
			return errors.New("invalid image encoding")
		}
	} else if a.Seq != 0 || a.Data != "" {
		return errors.New("unexpected image arguments")
	}
	return nil
}

func (s ExtensionStatus) supportsInput(src Source) bool {
	site := siteFor(src)
	return site != nil && s.Hello && s.ImageInput.Version == 1 && slices.Contains(s.ImageInput.Sites, site.opPrefix)
}

func (c *Client) requireImageInput(ctx context.Context, src Source) error {
	site, err := lookupSite(src)
	if err != nil {
		return err
	}
	if !site.imageInput {
		return fmt.Errorf("%s image input awaits live acceptance; send text alone", site.label)
	}
	status, err := c.ExtensionStatus(ctx)
	if err != nil {
		if errors.Is(err, errNoHostStatus) {
			return errors.New("upgrade tincan and the Chrome extension for image input")
		}
		return err
	}
	if !status.supportsInput(src) {
		return errors.New("upgrade tincan and the Chrome extension for image input")
	}
	return nil
}

// inputSession is used only for a single ask. Its exchanges are sequential.
type inputSession struct{ conn net.Conn }

func (s *inputSession) request(ctx context.Context, req NativeRequest) (json.RawMessage, error) {
	req.ID = requestSeq.Add(1)
	stop := context.AfterFunc(ctx, func() { _ = s.conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = s.conn.SetDeadline(dl)
	}
	if err := WriteMessage(s.conn, req, maxServiceRequest); err != nil {
		return nil, ctxErr(ctx, err)
	}
	body, err := ReadMessage(s.conn, MaxHostMessage)
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	var r NativeResponse
	if json.Unmarshal(body, &r) != nil || r.ID != req.ID || r.Chunk != nil {
		return nil, errors.New("invalid image transfer response")
	}
	if r.Error != nil {
		err := fromNativeError(SourceChatGPT, r.Error)
		// Preserve explicit no-submit evidence even for not_found, whose
		// legacy text path returns a bare sentinel wrapper.
		if _, ok := errors.AsType[*UnavailableError](err); !ok {
			err = &UnavailableError{Source: SourceChatGPT, Kind: err, Detail: r.Error.Message, Clicked: r.Error.Clicked, Uploaded: r.Error.Uploaded}
		}
		return nil, err
	}
	if !r.OK || len(r.Result) == 0 {
		return nil, errors.New("image operation failed")
	}
	return r.Result, nil
}

// sendImages uploads staged files in order. beforeSend durably records intent
// after transfer, immediately before the click-capable operation.
func (c *Client) sendImages(ctx context.Context, src Source, args OpArgs, files []stagedInput, beforeSend func() error) (SendResult, error) {
	if err := c.requireImageInput(ctx, src); err != nil {
		return SendResult{}, err
	}
	socket, ok := c.Channel.(*SocketChannel)
	if !ok {
		return SendResult{}, errors.New("image input requires a persistent native socket")
	}
	ctx, cancel := context.WithTimeout(ctx, SendClientTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket.Path)
	if err != nil {
		return SendResult{}, err
	}
	defer conn.Close()
	s := inputSession{conn: conn}
	meta := make([]InputFile, len(files))
	for i, f := range files {
		meta[i] = f.InputFile
	}
	begin := NativeRequest{Op: "chatgpt.input_begin", Input: &InputArgs{Files: meta}}
	if err := validateInputRequest(begin); err != nil {
		return SendResult{}, err
	}
	raw, err := s.request(ctx, begin)
	if err != nil {
		return SendResult{}, err
	}
	var ack struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &ack) != nil || len(ack.Token) != 32 {
		return SendResult{}, errors.New("invalid image transfer token")
	}
	// Disconnect also aborts at the host, including cancellation and panic.
	seq := 0
	for _, f := range files {
		err = func() error {
			file, err := os.Open(f.path)
			if err != nil {
				return err
			}
			defer file.Close()
			buf := make([]byte, InputChunkBytes)
			for {
				n, err := file.Read(buf)
				if n > 0 {
					raw, sendErr := s.request(ctx, NativeRequest{Op: "chatgpt.input_chunk", Input: &InputArgs{Token: ack.Token, Seq: seq, Data: base64.StdEncoding.EncodeToString(buf[:n])}})
					if sendErr != nil {
						return sendErr
					}
					var receipt struct {
						Seq *int `json:"seq"`
					}
					if json.Unmarshal(raw, &receipt) != nil || receipt.Seq == nil || *receipt.Seq != seq {
						return errors.New("image chunk not acknowledged")
					}
					seq++
				}
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
			}
		}()
		if err != nil {
			return SendResult{}, err
		}
	}
	if err := beforeSend(); err != nil {
		return SendResult{}, err
	}
	raw, err = s.request(ctx, NativeRequest{Op: "chatgpt.send_images", Args: args, Input: &InputArgs{Token: ack.Token}})
	if err != nil {
		return SendResult{}, err
	}
	var result SendResult
	if json.Unmarshal(raw, &result) != nil || !validNativeID(result.ConversationID) || !validNativeID(result.MessageID) || result.InputCount != len(files) || result.SubmittedAt <= 0 {
		return SendResult{}, errors.New("image submission is uncertain; do not retry automatically")
	}
	return result, nil
}

// abortInput only releases worker memory. It cannot delete vendor uploads.
func (h *NativeHost) abortInput(connection, token string) {
	h.outMu.Lock()
	defer h.outMu.Unlock()
	_ = WriteMessage(h.Out, NativeRequest{Op: "chatgpt.input_abort", Input: &InputArgs{Connection: connection, Token: token}}, MaxHostMessage)
}

// serveInput owns the sole socket reader for this path. It never uses the
// text path's one-byte disconnect probe, which would eat the next frame.
func (h *NativeHost) serveInput(ctx context.Context, conn net.Conn, first NativeRequest) {
	connection := fmt.Sprintf("input-%d", requestSeq.Add(1))
	token := ""
	started := false
	defer func() {
		if started {
			h.abortInput(connection, "")
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	frames := make(chan []byte)
	go func() {
		defer cancel()
		for {
			body, err := ReadMessage(conn, maxServiceRequest)
			if err != nil {
				return
			}
			select {
			case frames <- body:
			case <-ctx.Done():
				return
			}
		}
	}()
	req := first
	for {
		fail := func(message string) {
			_ = WriteMessage(conn, NativeResponse{ID: req.ID, Error: &NativeError{Code: "bad_request", Message: message}}, MaxHostMessage)
		}
		if err := validateInputRequest(req); err != nil {
			fail(err.Error())
			return
		}
		if !siteFor(SourceChatGPT).imageInput || !h.status().supportsInput(SourceChatGPT) {
			fail("image input awaits live acceptance or requires upgrading tincan and the extension")
			return
		}
		if req.Op == "chatgpt.input_begin" {
			if token != "" {
				fail("one transfer per connection")
				return
			}
		} else if token == "" || req.Input.Token != token {
			fail("unknown image transfer on this connection")
			return
		}
		clientID := req.ID
		ch := make(chan []byte, 64)
		h.mu.Lock()
		h.seq++
		req.ID = h.seq
		h.pending[req.ID] = ch
		h.mu.Unlock()
		req.Input.Connection = connection
		started = true
		h.outMu.Lock()
		err := WriteMessage(h.Out, req, MaxHostMessage)
		h.outMu.Unlock()
		if err != nil {
			h.forget(req.ID)
			return
		}
		limit := h.RequestTimeout
		if limit <= 0 {
			limit = 2 * time.Minute
		}
		if req.Op == "chatgpt.send_images" {
			limit = max(limit, SendHostTimeout)
		}
		timer := time.NewTimer(limit)
		var body []byte
		select {
		case body = <-ch:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		h.forget(req.ID)
		if len(body) == 0 {
			return
		}
		var r NativeResponse
		if json.Unmarshal(body, &r) != nil || !r.final() {
			return
		}
		if req.Op == "chatgpt.input_begin" && r.OK {
			var ack struct {
				Token string `json:"token"`
			}
			if json.Unmarshal(r.Result, &ack) != nil || len(ack.Token) != 32 {
				return
			}
			token = ack.Token
		}
		r.ID = clientID
		if err := WriteMessage(conn, r, MaxHostMessage); err != nil {
			return
		}
		if !r.OK || req.Op == "chatgpt.send_images" || req.Op == "chatgpt.input_abort" {
			return
		}
		select {
		case body = <-frames:
			req = NativeRequest{}
			if err := decodeNativeRequest(body, &req); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
