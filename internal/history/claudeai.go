package history

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// ClaudeAI reads claude.ai through the Tincan Chrome extension. The
// extension finds the organization with /api/organizations, then calls
// /api/organizations/<org>/chat_conversations (list), the conversation
// detail with tree=True&rendering_mode=messages, and
// /api/<org>/files/<file>/preview for images.
type ClaudeAI struct {
	Client *Client
	Window Window
	Now    func() time.Time
}

// NewClaudeAI returns a claude.ai reader over c.
func NewClaudeAI(c *Client) *ClaudeAI { return &ClaudeAI{Client: c} }

// Source implements Reader.
func (r *ClaudeAI) Source() Source { return SourceClaudeAI }

func (r *ClaudeAI) live() *live {
	return &live{
		source:      SourceClaudeAI,
		client:      r.Client,
		window:      r.Window,
		now:         r.Now,
		listOp:      OpClaudeAIList,
		detailOp:    OpClaudeAIDetail,
		parseList:   parseClaudeAIList,
		parseDetail: parseClaudeAIDetail,
		fileArgs:    claudeAIFileArgs,
	}
}

// List implements Reader.
func (r *ClaudeAI) List(ctx context.Context, count int, opts Options) ([]Conversation, error) {
	return r.live().List(ctx, count, opts)
}

// Read implements Reader.
func (r *ClaudeAI) Read(ctx context.Context, q Query, opts Options) ([]Conversation, error) {
	return r.live().Read(ctx, q, opts)
}

type caConversation struct {
	UUID         string      `json:"uuid"`
	Name         string      `json:"name"`
	UpdatedAt    flexTime    `json:"updated_at"`
	CreatedAt    flexTime    `json:"created_at"`
	CurrentLeaf  string      `json:"current_leaf_message_uuid"`
	ChatMessages []caMessage `json:"chat_messages"`
}

type caMessage struct {
	UUID      string   `json:"uuid"`
	Text      string   `json:"text"`
	Sender    string   `json:"sender"`
	Index     int      `json:"index"`
	CreatedAt flexTime `json:"created_at"`
	Parent    string   `json:"parent_message_uuid"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Files   []caFile `json:"files"`
	FilesV2 []caFile `json:"files_v2"`
	// StopReason is set on a finished assistant message when the site
	// reports it (the only completion field in the detail shape); the web
	// agent otherwise waits for the text to settle.
	StopReason string `json:"stop_reason"`
}

type caFile struct {
	FileKind string `json:"file_kind"`
	FileUUID string `json:"file_uuid"`
	FileName string `json:"file_name"`
}

const claudeFileScheme = "claudeai-file://"

func (m *caMessage) text() string {
	var parts []string
	for _, c := range m.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, c.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return m.Text
}

// images returns placeholders for the message's image files; files and
// files_v2 describe the same uploads and are deduplicated.
func (m *caMessage) images() []Image {
	var out []Image
	seen := map[string]bool{}
	for _, f := range append(append([]caFile{}, m.Files...), m.FilesV2...) {
		if f.FileKind != "image" || !validNativeID(f.FileUUID) || seen[f.FileUUID] {
			continue
		}
		seen[f.FileUUID] = true
		name := f.FileName
		if name == "" {
			name = f.FileUUID
		}
		out = append(out, imageRef(claudeFileScheme+f.FileUUID, name))
	}
	return out
}

func claudeAIFileArgs(_, pointer string) (Op, OpArgs, bool) {
	id, ok := strings.CutPrefix(pointer, claudeFileScheme)
	if !ok || !validNativeID(id) {
		return "", OpArgs{}, false
	}
	return OpClaudeAIFile, OpArgs{FileID: id}, true
}

func parseClaudeAIList(raw json.RawMessage) ([]Conversation, error) {
	var l []caConversation
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	var out []Conversation
	for _, c := range l {
		if !validNativeID(c.UUID) {
			continue
		}
		up := c.UpdatedAt.Time
		if up.IsZero() {
			up = c.CreatedAt.Time
		}
		out = append(out, Conversation{Source: SourceClaudeAI, ID: c.UUID, Title: c.Name, UpdatedAt: up})
	}
	return out, nil
}

// path returns the messages on the current branch, oldest first: walked
// back from the current leaf when the tree is present, else by index.
func (c *caConversation) path() []caMessage {
	byID := map[string]int{}
	for i, m := range c.ChatMessages {
		byID[m.UUID] = i
	}
	if i, ok := byID[c.CurrentLeaf]; ok && c.CurrentLeaf != "" {
		var rev []caMessage
		seen := map[string]bool{}
		for ok && !seen[c.ChatMessages[i].UUID] {
			m := c.ChatMessages[i]
			seen[m.UUID] = true
			rev = append(rev, m)
			i, ok = byID[m.Parent]
		}
		out := make([]caMessage, len(rev))
		for j, m := range rev {
			out[len(rev)-1-j] = m
		}
		return out
	}
	out := append([]caMessage{}, c.ChatMessages...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out
}

func parseClaudeAIDetail(id string, raw json.RawMessage) (thread, error) {
	var c caConversation
	if err := json.Unmarshal(raw, &c); err != nil {
		return thread{}, err
	}
	cid := c.UUID
	if cid == "" {
		cid = id
	}
	th := thread{conv: Conversation{Source: SourceClaudeAI, ID: cid, Title: c.Name, UpdatedAt: c.UpdatedAt.Time}}
	var cur *turn
	for _, m := range c.path() {
		switch m.Sender {
		case "human":
			if cur != nil {
				th.turns = append(th.turns, *cur)
			}
			cur = &turn{prompt: Message{Role: RoleUser, Text: m.text(), Time: m.CreatedAt.Time, Images: m.images()}, promptID: m.UUID}
		case "assistant":
			if cur == nil {
				continue
			}
			if t := m.text(); t != "" {
				if cur.reply.Text != "" {
					cur.reply.Text += "\n\n"
				}
				cur.reply.Text += t
				cur.reply.Time = m.CreatedAt.Time
			}
			cur.replyImages = append(cur.replyImages, m.images()...)
		}
	}
	if cur != nil {
		th.turns = append(th.turns, *cur)
	}
	return th, nil
}
