package mcpserver

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
)

func frame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStdioNewlineFramingPassesThrough(t *testing.T) {
	in := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"ping\"}\n"
	var out bytes.Buffer
	r, w := newAutoFraming(strings.NewReader(in), &out)
	if got := readAll(t, r); got != in {
		t.Fatalf("read %q", got)
	}
	if _, err := w.Write([]byte("{\"id\":1}\n")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\"id\":1}\n" {
		t.Fatalf("wrote %q", out.String())
	}
}

// Some hosts speak MCP stdio with LSP-style Content-Length headers. The
// first message's framing is kept for the whole session, both ways.
func TestStdioContentLengthFraming(t *testing.T) {
	pretty := "{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 2,\n  \"method\": \"tools/list\"\n}"
	in := frame(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`) + "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n" + frame(pretty)
	var out bytes.Buffer
	r, w := newAutoFraming(strings.NewReader(in), &out)
	got := readAll(t, r)
	want := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n"
	if got != want {
		t.Fatalf("read %q, want one compact line per message", got)
	}
	for _, msg := range []string{`{"id":1}`, `{"id":2,"result":{}}`} {
		if _, err := w.Write([]byte(msg + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	br := bufio.NewReader(&out)
	for _, msg := range []string{`{"id":1}`, `{"id":2,"result":{}}`} {
		line, _ := br.ReadString('\n')
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:")))
		if err != nil || n != len(msg) {
			t.Fatalf("header %q for %q", line, msg)
		}
		if blank, _ := br.ReadString('\n'); blank != "\r\n" {
			t.Fatalf("separator %q", blank)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil || string(body) != msg {
			t.Fatalf("body %q %v", body, err)
		}
	}
}

func TestStdioContentLengthRejectsBadFrames(t *testing.T) {
	for name, in := range map[string]string{
		"no length":  "Content-Type: x\r\n\r\n{}",
		"too large":  "Content-Length: 999999999999\r\n\r\n{}",
		"not json":   frame("hello"),
		"short body": "Content-Length: 50\r\n\r\n{}",
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := newAutoFraming(strings.NewReader(in), io.Discard)
			if _, err := io.ReadAll(r); err == nil {
				t.Fatal("bad frame accepted")
			}
		})
	}
}
