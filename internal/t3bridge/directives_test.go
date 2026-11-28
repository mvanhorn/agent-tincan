package t3bridge

import "testing"

func TestExtractThreadID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"28b76160-99e7-4dc4-a9f4-eb2be912a869", "28b76160-99e7-4dc4-a9f4-eb2be912a869"},
		{"https://app.t3.codes/threads/bf651c52-ff81-4c0b-baaf-939189abb9b8/28b76160-99e7-4dc4-a9f4-eb2be912a869", "28b76160-99e7-4dc4-a9f4-eb2be912a869"},
		{"https://app.t3.codes/threads/66559402-8ca1-472c-acea-53b579bc41fb/mcp%3Alaunch%3A896728777dfd5a71a8933f193edc42", "mcp:launch:896728777dfd5a71a8933f193edc42"},
		{"[My Thread](t3-thread://v2/env-1/thr-99)", "thr-99"},
		{"<https://app.t3.codes/threads/env-1/thr-2>.", "thr-2"},
		{"", ""},
		{"https://app.t3.codes/threads/", ""},
		{"bad/path", ""},
	}
	for _, tc := range cases {
		if got := ExtractThreadID(tc.in); got != tc.want {
			t.Errorf("ExtractThreadID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseDirective(t *testing.T) {
	d := ParseDirective("watch https://app.t3.codes/threads/env-1/thr-1 ping me when done", "")
	if d.Kind != DirectiveWatch || d.ThreadID != "thr-1" || d.Note != "ping me when done" || !d.TargetsExistingThread() {
		t.Fatalf("unexpected watch directive: %+v", d)
	}

	d = ParseDirective("unwatch thr-1", "")
	if d.Kind != DirectiveUnwatch || d.ThreadID != "thr-1" || !d.TargetsExistingThread() {
		t.Fatalf("unexpected unwatch directive: %+v", d)
	}

	d = ParseDirective("status [Fix Bug](t3-thread://v2/env-1/thr-7)", "")
	if d.Kind != DirectiveStatus || d.ThreadID != "thr-7" {
		t.Fatalf("unexpected status directive: %+v", d)
	}

	d = ParseDirective("thread: thr-4\nplease run the tests", "")
	if d.Kind != DirectiveSend || d.ThreadID != "thr-4" || d.Message != "please run the tests" {
		t.Fatalf("unexpected thread: directive: %+v", d)
	}

	d = ParseDirective("send https://app.t3.codes/threads/env-1/thr-4: good job", "")
	if d.Kind != DirectiveSend || d.ThreadID != "thr-4" || d.Message != "good job" {
		t.Fatalf("unexpected send directive: %+v", d)
	}

	d = ParseDirective("project: agent-tincan\nsummarize the last 3 commits", "")
	if d.Kind != DirectiveLaunch || d.Project != "agent-tincan" || d.Message != "summarize the last 3 commits" || d.TargetsExistingThread() {
		t.Fatalf("unexpected project directive: %+v", d)
	}

	d = ParseDirective("just fix the build", "my-repo")
	if d.Kind != DirectiveLaunch || d.Project != "my-repo" || d.Message != "just fix the build" {
		t.Fatalf("unexpected default launch directive: %+v", d)
	}
}
