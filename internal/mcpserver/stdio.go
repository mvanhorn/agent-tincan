package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxFrame bounds one Content-Length framed message.
const maxFrame = 64 << 20

// Stdio returns the stdio transport. MCP stdio is newline-delimited JSON,
// but some hosts send LSP-style Content-Length framed messages instead; the
// first message's framing is used for the whole session, both ways.
func Stdio() mcp.Transport { return StdioRecorded(nil) }

// StdioRecorded is Stdio that also notes in rec which framing the app used
// and when it initialized, listed the tools and first called one.
func StdioRecorded(rec *Recorder) mcp.Transport {
	r, w := newAutoFraming(os.Stdin, os.Stdout)
	if rec != nil {
		a := r.(autoReader).a
		r = io.TeeReader(r, rec)
		go func() {
			<-a.ready
			if a.framed {
				rec.SetFraming("content-length")
			} else {
				rec.SetFraming("newline")
			}
		}()
	}
	return &mcp.IOTransport{Reader: io.NopCloser(r), Writer: nopWriteCloser{w}}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// autoFraming reads either framing and hands the SDK newline-delimited
// JSON; its writer puts replies back in the framing the host used.
type autoFraming struct {
	in  *bufio.Reader
	out io.Writer

	detect sync.Once
	framed bool
	ready  chan struct{}
	pend   bytes.Buffer // decoded lines not yet read

	wmu  sync.Mutex
	wbuf []byte
}

func newAutoFraming(in io.Reader, out io.Writer) (io.Reader, io.Writer) {
	a := &autoFraming{in: bufio.NewReader(in), out: out, ready: make(chan struct{})}
	return autoReader{a}, autoWriter{a}
}

// detectFraming looks at the first byte that is not white space: JSON
// starts newline-delimited framing, anything else is a header.
func (a *autoFraming) detectFraming() {
	a.detect.Do(func() {
		defer close(a.ready)
		for {
			b, err := a.in.Peek(1)
			if err != nil {
				return
			}
			switch b[0] {
			case ' ', '\t', '\r', '\n':
				_, _ = a.in.ReadByte()
				continue
			case '{', '[':
				return
			}
			a.framed = true
			return
		}
	})
}

type autoReader struct{ a *autoFraming }

func (r autoReader) Read(p []byte) (int, error) {
	a := r.a
	a.detectFraming()
	if !a.framed {
		return a.in.Read(p)
	}
	if a.pend.Len() == 0 {
		msg, err := a.readFrame()
		if err != nil {
			return 0, err
		}
		a.pend.Write(msg)
		a.pend.WriteByte('\n')
	}
	return a.pend.Read(p)
}

// readFrame reads one Content-Length framed message and returns it as
// compact JSON on one line.
func (a *autoFraming) readFrame() ([]byte, error) {
	length := -1
	sawHeader := false
	for {
		line, err := a.in.ReadString('\n')
		if err != nil {
			if err == io.EOF && !sawHeader && line == "" {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("mcp stdio: reading frame header: %w", unexpectedEOF(err))
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !sawHeader {
				continue
			}
			break
		}
		sawHeader = true
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("mcp stdio: bad frame header %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 || n > maxFrame {
				return nil, fmt.Errorf("mcp stdio: bad Content-Length %q", strings.TrimSpace(value))
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("mcp stdio: frame has no Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(a.in, body); err != nil {
		return nil, fmt.Errorf("mcp stdio: reading frame body: %w", unexpectedEOF(err))
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return nil, fmt.Errorf("mcp stdio: frame body is not JSON: %w", err)
	}
	return compact.Bytes(), nil
}

func unexpectedEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

type autoWriter struct{ a *autoFraming }

// Write passes newline-delimited output through, or, once the host is
// known to use Content-Length framing, frames each complete line.
func (w autoWriter) Write(p []byte) (int, error) {
	a := w.a
	select {
	case <-a.ready:
	default:
		// Nothing has been read yet, so the host's framing is unknown;
		// the MCP default is newline-delimited.
		return a.out.Write(p)
	}
	if !a.framed {
		return a.out.Write(p)
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	a.wbuf = append(a.wbuf, p...)
	for {
		i := bytes.IndexByte(a.wbuf, '\n')
		if i < 0 {
			break
		}
		msg := a.wbuf[:i]
		if _, err := fmt.Fprintf(a.out, "Content-Length: %d\r\n\r\n%s", len(msg), msg); err != nil {
			return 0, err
		}
		a.wbuf = a.wbuf[i+1:]
	}
	return len(p), nil
}
