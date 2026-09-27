package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// runSplit runs cmd with separate stdout and stderr, so --json tests can
// parse stdout alone.
func runSplit(t *testing.T, cmd *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	// Root silences usage and errors; do the same for a bare subcommand.
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

type jsonOutcome struct {
	Outcome string           `json:"outcome"`
	Result  client.Result    `json:"result"`
	Request envelope.Request `json:"request"`
}

func decodeOutcome(t *testing.T, stdout string) jsonOutcome {
	t.Helper()
	var got jsonOutcome
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	return got
}

// wantExit checks the exit code main would use for err, and that an
// outcome exit is silent so stdout holds the whole answer.
func wantExit(t *testing.T, err error, code int) {
	t.Helper()
	got, silent := ExitStatus(err)
	if got != code || !silent {
		t.Fatalf("exit = %d (silent %v) from %v; want %d, silent", got, silent, err, code)
	}
}

// ask --json with a reply prints outcome answered with the reply and its
// attachments, and exits 0.
func TestAskJSONAnswered(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetAttachmentDir(t.TempDir())
	ctx := context.Background()
	muse := m.Client(t, "muse")
	notes := tempFile(t, "notes.txt", []byte("tabby"))
	go func() {
		in, _ := muse.Poll(ctx, 5*time.Second)
		for _, r := range in.Requests {
			_, _ = muse.Claim(ctx, r.ID)
			ups, err := muse.UploadFiles(ctx, []string{notes})
			if err != nil {
				return
			}
			_, _ = muse.ReplyAttached(ctx, r.ID, "a cat", envelope.StatusAnswered, client.AttachmentIDs(ups))
		}
	}()
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	stdout, _, err := runSplit(t, askCmd(), "muse", "what is it", "--json", "--wait", "10s")
	wantExit(t, err, 0)
	got := decodeOutcome(t, stdout)
	if got.Outcome != "answered" || got.Result.Reply == nil || got.Result.Reply.Body != "a cat" || got.Result.Status != envelope.StatusAnswered {
		t.Fatalf("outcome = %+v", got)
	}
	if a := got.Result.Reply.Attachments; len(a) != 1 || a[0].Name != "notes.txt" {
		t.Fatalf("reply attachments = %+v", a)
	}
}

// countingProxy forwards to target and counts GETs of a single request.
func countingProxy(t *testing.T, target string) (string, *atomic.Int32) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	p := httputil.NewSingleHostReverseProxy(u)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/requests/") {
			n.Add(1)
		}
		p.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL, &n
}

// ask --json --wait 0 reports pending without asking the relay again, and
// exits 2.
func TestAskJSONPending(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	proxy, gets := countingProxy(t, m.URL("grokbot"))
	useConfig(t, client.Config{Relay: proxy, Agent: "grokbot"})
	stdout, stderr, err := runSplit(t, askCmd(), "muse", "later", "--json", "--wait", "0s")
	wantExit(t, err, 2)
	got := decodeOutcome(t, stdout)
	if got.Outcome != "pending" || got.Result.Request.ID == "" || got.Result.Reply != nil {
		t.Fatalf("outcome = %+v", got)
	}
	if n := gets.Load(); n != 0 {
		t.Fatalf("a --wait 0 ask made %d get calls", n)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
}

// get --json on a request that ended without an answer reports failed and
// exits 1: declined, cancelled and expired alike.
func TestGetJSONEndedWithoutAnswer(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, m *testrelay.Mesh, id string)
		want envelope.Status
	}{
		{"declined", func(t *testing.T, m *testrelay.Mesh, id string) {
			muse := m.Client(t, "muse")
			if _, err := muse.Claim(ctx, id); err != nil {
				t.Fatal(err)
			}
			if _, err := muse.Reply(ctx, id, "not mine", envelope.StatusDeclined); err != nil {
				t.Fatal(err)
			}
		}, envelope.StatusDeclined},
		{"cancelled", func(t *testing.T, m *testrelay.Mesh, id string) {
			if err := m.Client(t, "grokbot").Cancel(ctx, id); err != nil {
				t.Fatal(err)
			}
		}, envelope.StatusCancelled},
		{"expired", func(t *testing.T, m *testrelay.Mesh, _ string) {
			time.Sleep(50 * time.Millisecond)
			m.Server.Sweep(ctx)
		}, envelope.StatusExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ttl := time.Hour
			if tc.want == envelope.StatusExpired {
				ttl = 10 * time.Millisecond
			}
			m := testrelay.New(t, relay.Config{RequestTTL: ttl})
			req, err := m.Client(t, "grokbot").Send(ctx, "muse", "do it", envelope.KindAsk, "")
			if err != nil {
				t.Fatal(err)
			}
			tc.end(t, m, req.ID)
			useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
			stdout, _, err := runSplit(t, getCmd(), req.ID, "--json")
			wantExit(t, err, 1)
			got := decodeOutcome(t, stdout)
			if got.Outcome != "failed" || got.Result.Status != tc.want {
				t.Fatalf("outcome = %+v, want failed with status %s", got, tc.want)
			}

			// Without --json the same request prints text and exits 0.
			stdout, _, err = runSplit(t, getCmd(), req.ID)
			if want := client.FormatResult(got.Result); err != nil || stdout != want {
				t.Fatalf("text get = %q, %v; want %q", stdout, err, want)
			}
		})
	}
}

// get --json on an answered request exits 0.
func TestGetJSONAnswered(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	req := answered(t, m, "call the garage", "Tue 3pm works")
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	stdout, _, err := runSplit(t, getCmd(), req.ID, "--json")
	wantExit(t, err, 0)
	if got := decodeOutcome(t, stdout); got.Outcome != "answered" || got.Result.Reply == nil || got.Result.Reply.Body != "Tue 3pm works" {
		t.Fatalf("outcome = %+v", got)
	}
}

// A transport error with --json keeps the stderr message and exit 1, with
// nothing on stdout.
func TestGetJSONTransportError(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	stdout, _, err := runSplit(t, getCmd(), "no-such-request", "--json")
	if code, silent := ExitStatus(err); code != 1 || silent || err == nil {
		t.Fatalf("exit = %d, silent %v, err %v; want 1 with a message", code, silent, err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
}

// ask --notify --json prints outcome sent with the request, and exits 0.
func TestAskNotifyJSON(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	stdout, _, err := runSplit(t, askCmd(), "muse", "fyi", "--notify", "--json")
	wantExit(t, err, 0)
	got := decodeOutcome(t, stdout)
	if got.Outcome != "sent" || got.Request.ID == "" || got.Request.Body != "fyi" {
		t.Fatalf("outcome = %+v", got)
	}
}

// Without --json, ask and get print exactly the text they always have and
// exit 0 whatever the outcome.
func TestAskGetTextUnchanged(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	req := answered(t, m, "call the garage", "Tue 3pm works")
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	res, err := m.Client(t, "grokbot").Get(ctx, req.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runSplit(t, getCmd(), req.ID)
	if err != nil || stdout != client.FormatResult(res) {
		t.Fatalf("get = %q, %v; want %q", stdout, err, client.FormatResult(res))
	}

	stdout, _, err = runSplit(t, askCmd(), "muse", "later", "--wait", "0s")
	id := regexp.MustCompile(`Request id (\S+) `).FindStringSubmatch(stdout)
	if err != nil || id == nil {
		t.Fatalf("ask = %q, %v", stdout, err)
	}
	want := client.FormatResult(client.Result{Request: envelope.Request{ID: id[1], To: "muse"}, Status: envelope.StatusQueued})
	if stdout != want {
		t.Fatalf("pending ask = %q, want %q", stdout, want)
	}
}

type inboxJSONOut struct {
	Requests []struct {
		ID         string `json:"id"`
		Body       string `json:"body"`
		Claimed    bool   `json:"claimed"`
		ClaimError string `json:"claim_error"`
	} `json:"requests"`
	Replies []client.Result `json:"replies"`
}

// inbox --json claims each request itself, flags one another session
// handled between the poll and the claim, lists replies, then acks them.
func TestInboxJSON(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	req := answered(t, m, "call the garage", "Tue 3pm works")
	instinct := m.Client(t, "instinct")
	mine, _ := instinct.Send(ctx, "grokbot", "summarize the report", envelope.KindAsk, "")
	taken, _ := instinct.Send(ctx, "grokbot", "already handled", envelope.KindAsk, "")
	grok := m.Client(t, "grokbot")
	in, err := grok.Poll(ctx, 0)
	if err != nil || len(in.Requests) != 2 || len(in.Replies) != 1 {
		t.Fatalf("poll = %+v, %v", in, err)
	}
	// Another grokbot session claims and answers one before this one does.
	if _, err := grok.Claim(ctx, taken.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := grok.Reply(ctx, taken.ID, "done", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := printInboxJSON(ctx, grok, in, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var got inboxJSONOut
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	if len(got.Replies) != 1 || got.Replies[0].Request.ID != req.ID || got.Replies[0].Reply == nil || got.Replies[0].Reply.Body != "Tue 3pm works" {
		t.Fatalf("replies = %+v", got.Replies)
	}
	byID := map[string]int{}
	for i, r := range got.Requests {
		byID[r.ID] = i
	}
	if len(got.Requests) != 2 {
		t.Fatalf("requests = %+v", got.Requests)
	}
	if r := got.Requests[byID[mine.ID]]; r.ID != mine.ID || !r.Claimed || r.ClaimError != "" || r.Body != "summarize the report" {
		t.Fatalf("claimed request = %+v", r)
	}
	if r := got.Requests[byID[taken.ID]]; r.ID != taken.ID || r.Claimed || r.ClaimError == "" {
		t.Fatalf("request handled elsewhere = %+v, want claimed false with claim_error", r)
	}
	if n := m.Server.UnseenReplies("grokbot"); n != 0 {
		t.Fatalf("printed replies are acked: unseen = %d", n)
	}
}

// inbox --json acks replies only after printing them.
func TestInboxJSONAcksOnlyAfterPrinting(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	answered(t, m, "call the garage", "Tue 3pm works")
	grok := m.Client(t, "grokbot")
	if err := checkInboxJSON(ctx, grok, 0, failWriter{}, io.Discard); err == nil {
		t.Fatal("want the write error")
	}
	if n := m.Server.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("an unprinted reply must stay unseen: unseen = %d", n)
	}
	var out bytes.Buffer
	if err := checkInboxJSON(ctx, grok, 0, &out, io.Discard); err != nil || !strings.Contains(out.String(), "Tue 3pm works") {
		t.Fatalf("retry inbox = %q, %v", out.String(), err)
	}
	if n := m.Server.UnseenReplies("grokbot"); n != 0 {
		t.Fatalf("a printed reply is acked: unseen = %d", n)
	}
}

// An empty inbox --json is still one document with empty lists.
func TestInboxJSONEmpty(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	stdout, _, err := runSplit(t, inboxCmd(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil || string(raw["requests"]) != "[]" || string(raw["replies"]) != "[]" {
		t.Fatalf("empty inbox = %s, %v", stdout, err)
	}
}
