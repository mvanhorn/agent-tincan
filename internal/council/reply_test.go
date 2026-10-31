package council

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// completedCouncil is a finished council with a verdict: five asked, one
// absent, two excluded, and a four-way peer ranking with a tie.
func completedCouncil() Finished {
	return Finished{
		RequestID: "req-123",
		Question:  "Should we shard the relay database?\nWe run 3 nodes and hold 40 GB.",
		Asked:     []string{"claude-web", "chatgpt-web", "gemini-web", "grok-web", "copilot-web"},
		Excluded: []Exclusion{
			{Name: "codex", Reason: ReasonInChain},
			{Name: "claude-code", Reason: ReasonLiveSession},
		},
		At: time.Date(2026, 9, 29, 14, 41, 0, 0, time.UTC),
		Outcome: Outcome{
			State: CouncilCompleted,
			Answers: []Answer{
				{Member: "claude-web", Body: "Keep one node and add a read replica.", Elapsed: 70 * time.Second},
				{Member: "chatgpt-web", Body: "Take backups first, then decide.", Elapsed: 95 * time.Second},
				{Member: "gemini-web", Body: "One node handles 40 GB fine.", Elapsed: 50 * time.Second},
				{Member: "grok-web", Body: "Shard now by tenant.", Elapsed: 40 * time.Second},
			},
			Standings: []Standing{
				{Member: "claude-web", Score: 0.75, Ballots: 4, Placement: 1},
				{Member: "gemini-web", Score: 0.5, Ballots: 4, Placement: 2},
				{Member: "chatgpt-web", Score: 0.25, Ballots: 4, Placement: 3},
				{Member: "grok-web", Score: 0.25, Ballots: 4, Placement: 3},
			},
			Absent: []Absence{{Member: "copilot-web", Stage: StageAnswer, Reason: AbsentTimedOut}},
			Verdict: Verdict{
				Chairman:       "claude-web",
				Category:       "architecture",
				Recommendation: "Do not shard yet; Answer B is right that one node handles 40 GB.",
				Agreement:      "All answers agree the data fits on one node today.",
				Disagreement:   "Answer C wants to shard now.",
				Minority:       "Answer D says to take backups first.",
				Labels:         map[string]string{"A": "claude-web", "B": "gemini-web", "C": "grok-web", "D": "chatgpt-web"},
			},
			AnswerTime: 3*time.Minute + 12*time.Second,
			ReviewTime: 2*time.Minute + 5*time.Second,
			ChairTime:  48 * time.Second,
		},
		ReportPath: "/tmp/r/req-123.html",
		CardPath:   "/tmp/r/req-123.png",
	}
}

// resultBlock parses the reply's council-result block.
func resultBlock(t *testing.T, reply string) map[string]any {
	t.Helper()
	const open = "```council-result\n"
	_, rest, found := strings.CutLast(reply, open)
	if !found {
		t.Fatalf("no council-result block in reply:\n%s", reply)
	}
	raw, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		t.Fatalf("council-result block is not closed:\n%s", reply)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("council-result block does not parse: %v\n%s", err, raw)
	}
	return got
}

func TestReplyCompletedMatchesGolden(t *testing.T) {
	f := completedCouncil()
	text, status := Reply(f)
	if status != envelope.StatusAnswered {
		t.Errorf("status = %q, want answered", status)
	}
	want, err := os.ReadFile("testdata/reply_completed.golden")
	if err != nil {
		t.Fatal(err)
	}
	if text != string(want) {
		t.Errorf("reply differs from testdata/reply_completed.golden\n--- got ---\n%s\n--- want ---\n%s", text, want)
	}

	res := resultBlock(t, text)
	if res["status"] != "answered" {
		t.Errorf("block status = %v, want answered", res["status"])
	}
	ranking, _ := res["ranking"].([]any)
	var members []string
	for _, r := range ranking {
		members = append(members, r.(map[string]any)["member"].(string))
	}
	if got := strings.Join(members, ","); got != "claude-web,gemini-web,chatgpt-web,grok-web" {
		t.Errorf("block ranking = %s", got)
	}
}

func TestReplyNoQuorumFailsWithReceivedAnswers(t *testing.T) {
	f := Finished{
		RequestID: "req-9",
		Question:  "Which queue should we use?",
		Asked:     []string{"claude-web", "chatgpt-web", "gemini-web", "grok-web"},
		Outcome: Outcome{
			State:  CouncilFailed,
			Reason: "only 2 of 4 members answered in time, and a council needs at least 3",
			Answers: []Answer{
				{Member: "claude-web", Body: "Use SQS for the managed retries."},
				{Member: "gemini-web", Body: "Use NATS JetStream."},
			},
			Absent: []Absence{
				{Member: "chatgpt-web", Stage: StageAnswer, Reason: AbsentHeld},
				{Member: "grok-web", Stage: StageAnswer, Reason: AbsentTimedOut},
			},
		},
	}
	text, status := Reply(f)
	if status != envelope.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	for _, want := range []string{
		"only 2 of 4 members answered in time",
		"Use SQS for the managed retries.",
		"Use NATS JetStream.",
		"4 asked, 2 answered, 2 absent",
		"chatgpt-web (answer stage): held by a gate",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("reply lacks %q:\n%s", want, text)
		}
	}
	res := resultBlock(t, text)
	if res["status"] != "failed" {
		t.Errorf("block status = %v, want failed", res["status"])
	}
	answers, _ := res["answers"].([]any)
	if len(answers) != 2 || answers[0].(map[string]any)["body"] != "Use SQS for the managed retries." {
		t.Errorf("block answers = %v, want both received answers", res["answers"])
	}
}

func TestReplyStatusMapping(t *testing.T) {
	for state, want := range map[CouncilState]envelope.Status{
		CouncilCompleted: envelope.StatusAnswered,
		CouncilFailed:    envelope.StatusFailed,
		CouncilDeclined:  envelope.StatusDeclined,
	} {
		f := Finished{RequestID: "r", Question: "q", Outcome: Outcome{State: state, Reason: "only 2 eligible member(s)"}}
		if _, got := Reply(f); got != want {
			t.Errorf("%s: status = %q, want %q", state, got, want)
		}
	}
}

func TestReplyVerdictUnavailableKeepsRanking(t *testing.T) {
	f := completedCouncil()
	f.Outcome.Verdict = Verdict{Category: Uncategorized, Unavailable: true, Reason: "no chairman candidate delivered a verdict within 4m0s"}
	text, status := Reply(f)
	if status != envelope.StatusAnswered {
		t.Errorf("status = %q, want answered", status)
	}
	for _, want := range []string{"Verdict unavailable: no chairman candidate delivered a verdict within 4m0s", "1. claude-web 0.75 (4 ballots)"} {
		if !strings.Contains(text, want) {
			t.Errorf("reply lacks %q:\n%s", want, text)
		}
	}
	if res := resultBlock(t, text); res["category"] != Uncategorized || res["verdict"] != nil {
		t.Errorf("block category = %v, verdict = %v; want uncategorized and no verdict", res["category"], res["verdict"])
	}
}

func TestReplyNoQuorumFitsRelayBodyLimit(t *testing.T) {
	long := strings.Repeat("<b>a long web answer</b> ", 64<<10/25)
	f := Finished{
		RequestID: "req-10",
		Question:  "Which queue should we use?",
		Asked:     []string{"claude-web", "chatgpt-web", "gemini-web", "grok-web"},
		Outcome: Outcome{
			State:  CouncilFailed,
			Reason: "only 2 of 4 members answered in time, and a council needs at least 3",
			Answers: []Answer{
				{Member: "claude-web", Body: long},
				{Member: "gemini-web", Body: long},
			},
		},
		ReportPath: "/tmp/report.html",
	}
	text, _ := Reply(f)
	if len(text) > envelope.DefaultMaxBody {
		t.Fatalf("reply is %d bytes, over the relay's %d byte body limit", len(text), envelope.DefaultMaxBody)
	}
	if !strings.Contains(text, "[truncated:") {
		t.Errorf("reply does not say the answers were cut")
	}
	res := resultBlock(t, text)
	if answers, _ := res["answers"].([]any); len(answers) != 2 {
		t.Errorf("block answers = %d, want 2", len(answers))
	}
}

func TestReplyCompletedLongVerdictFitsRelayBodyLimit(t *testing.T) {
	f := completedCouncil()
	f.Outcome.Verdict.Recommendation = strings.Repeat("<b>\"a long verdict\"</b> ", 200<<10/24)
	f.Outcome.Verdict.Minority = strings.Repeat("minority \"view\" ", 20<<10/16)
	text, status := Reply(f)
	if status != envelope.StatusAnswered {
		t.Fatalf("status = %s, want answered", status)
	}
	if len(text) > envelope.DefaultMaxBody {
		t.Fatalf("reply is %d bytes, over the relay's %d byte body limit", len(text), envelope.DefaultMaxBody)
	}
	if !strings.Contains(text, "[truncated:") {
		t.Errorf("reply does not say the verdict was cut")
	}
	res := resultBlock(t, text)
	v, _ := res["verdict"].(map[string]any)
	if rec, _ := v["recommendation"].(string); !strings.Contains(rec, "[truncated:") {
		t.Errorf("block recommendation was not cut: %d bytes", len(rec))
	}
	if agr, _ := v["agreement"].(string); agr != "All answers agree the data fits on one node today." {
		t.Errorf("short agreement changed: %q", agr)
	}
}
