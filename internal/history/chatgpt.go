package history

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ChatGPT reads chatgpt.com through the Tincan Chrome extension. The
// extension calls GET /api/auth/session inside the browser for the access
// token (it never leaves the extension), then /backend-api/conversations
// (list), /backend-api/conversation/<id> (detail) and the files download
// endpoint for image pointers.
type ChatGPT struct {
	Client *Client
	Window Window
	Now    func() time.Time
	// AgentChats is the web agents' used list; those conversations are
	// left out unless all is asked for (DefaultWebUsedPath from New).
	AgentChats string
}

// NewChatGPT returns a ChatGPT reader over c.
func NewChatGPT(c *Client) *ChatGPT {
	return &ChatGPT{Client: c, AgentChats: DefaultWebUsedPath(SourceChatGPT)}
}

// Source implements Reader.
func (r *ChatGPT) Source() Source { return SourceChatGPT }

func (r *ChatGPT) live() *live {
	return &live{
		source:      SourceChatGPT,
		client:      r.Client,
		window:      r.Window,
		now:         r.Now,
		agentChats:  r.AgentChats,
		listOp:      OpChatGPTList,
		detailOp:    OpChatGPTDetail,
		parseList:   parseChatGPTList,
		parseDetail: parseChatGPTDetail,
		fileArgs:    chatgptFileArgs,
	}
}

// List implements Reader.
func (r *ChatGPT) List(ctx context.Context, count int, opts Options) ([]Conversation, error) {
	return r.live().List(ctx, count, opts)
}

// Read implements Reader.
func (r *ChatGPT) Read(ctx context.Context, q Query, opts Options) ([]Conversation, error) {
	return r.live().Read(ctx, q, opts)
}

type cgList struct {
	Items []struct {
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		UpdateTime flexTime `json:"update_time"`
		CreateTime flexTime `json:"create_time"`
	} `json:"items"`
}

func parseChatGPTList(raw json.RawMessage) ([]Conversation, error) {
	var l cgList
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	if l.Items == nil {
		return nil, errors.New("no items")
	}
	var out []Conversation
	for _, it := range l.Items {
		if !validNativeID(it.ID) {
			continue
		}
		up := it.UpdateTime.Time
		if up.IsZero() {
			up = it.CreateTime.Time
		}
		out = append(out, Conversation{Source: SourceChatGPT, ID: it.ID, Title: it.Title, UpdatedAt: up})
	}
	return out, nil
}

type cgDetail struct {
	Title          string            `json:"title"`
	UpdateTime     flexTime          `json:"update_time"`
	ConversationID string            `json:"conversation_id"`
	CurrentNode    string            `json:"current_node"`
	Mapping        map[string]cgNode `json:"mapping"`
}

type cgNode struct {
	Message *cgMessage `json:"message"`
	Parent  string     `json:"parent"`
}

type cgMessage struct {
	ID     string `json:"id"`
	Author struct {
		Role string `json:"role"`
		Name string `json:"name"`
	} `json:"author"`
	CreateTime flexTime `json:"create_time"`
	Content    struct {
		ContentType string            `json:"content_type"`
		Parts       []json.RawMessage `json:"parts"`
	} `json:"content"`
	Metadata struct {
		Attachments []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			MimeType string `json:"mime_type"`
		} `json:"attachments"`
		Hidden        bool            `json:"is_visually_hidden_from_conversation"`
		FinishDetails json.RawMessage `json:"finish_details"`
	} `json:"metadata"`
	Recipient string `json:"recipient"`
	// Status is "in_progress" while a reply is written and
	// "finished_successfully" after; EndTurn is true on a turn's last
	// assistant message. The web agent uses them to tell a finished reply.
	Status  string `json:"status"`
	EndTurn *bool  `json:"end_turn"`
}

type cgPart struct {
	ContentType  string `json:"content_type"`
	AssetPointer string `json:"asset_pointer"`
}

// parts splits a message's parts into its text and its image pointers.
func (m *cgMessage) parts() (string, []string) {
	var texts, pointers []string
	for _, p := range m.Content.Parts {
		var s string
		if json.Unmarshal(p, &s) == nil {
			if strings.TrimSpace(s) != "" {
				texts = append(texts, s)
			}
			continue
		}
		var part cgPart
		if json.Unmarshal(p, &part) == nil && part.ContentType == "image_asset_pointer" && chatgptPointerID(part.AssetPointer) != "" {
			pointers = append(pointers, part.AssetPointer)
		}
	}
	return strings.Join(texts, "\n"), pointers
}

// chatgptPointerID returns the file id of a file-service:// or sediment://
// pointer, or "" if it is neither or the id is malformed.
func chatgptPointerID(p string) string {
	for _, scheme := range []string{"file-service://", "sediment://"} {
		if id, ok := strings.CutPrefix(p, scheme); ok && validNativeID(id) {
			return id
		}
	}
	return ""
}

func chatgptFileArgs(convID, pointer string) (Op, OpArgs, bool) {
	id := chatgptPointerID(pointer)
	if id == "" {
		return "", OpArgs{}, false
	}
	a := OpArgs{FileID: id}
	if validNativeID(convID) {
		a.ConversationID = convID
	}
	return OpChatGPTFile, a, true
}

// path returns the messages on the current branch, root first.
func (d *cgDetail) path() []*cgMessage {
	var rev []*cgMessage
	seen := map[string]bool{}
	for id := d.CurrentNode; id != "" && !seen[id]; {
		seen[id] = true
		n, ok := d.Mapping[id]
		if !ok {
			break
		}
		if n.Message != nil {
			rev = append(rev, n.Message)
		}
		id = n.Parent
	}
	out := make([]*cgMessage, len(rev))
	for i, m := range rev {
		out[len(rev)-1-i] = m
	}
	return out
}

// parseChatGPTDetail builds the turns of the current branch. A turn's
// images are the images attached to Matt's prompt plus images generated in
// the reply (image pointers in tool or assistant messages before the next
// prompt).
func parseChatGPTDetail(id string, raw json.RawMessage) (thread, error) {
	var d cgDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return thread{}, err
	}
	if d.Mapping == nil {
		return thread{}, errors.New("no mapping")
	}
	cid := d.ConversationID
	if cid == "" {
		cid = id
	}
	th := thread{conv: Conversation{Source: SourceChatGPT, ID: cid, Title: d.Title, UpdatedAt: d.UpdateTime.Time}}
	var cur *turn
	var replies []string
	flush := func() {
		if cur != nil {
			cur.reply.Text = strings.Join(replies, "\n\n")
			th.turns = append(th.turns, *cur)
		}
		cur, replies = nil, nil
	}
	for _, m := range d.path() {
		if m.Metadata.Hidden {
			continue
		}
		text, pointers := m.parts()
		switch m.Author.Role {
		case "user":
			if m.Content.ContentType != "text" && m.Content.ContentType != "multimodal_text" {
				continue
			}
			names := map[string]string{}
			for _, a := range m.Metadata.Attachments {
				if strings.HasPrefix(a.MimeType, "image/") && validNativeID(a.ID) {
					names[a.ID] = a.Name
					if !containsPointerID(pointers, a.ID) {
						pointers = append(pointers, "file-service://"+a.ID)
					}
				}
			}
			if text == "" && len(pointers) == 0 {
				continue
			}
			flush()
			cur = &turn{prompt: Message{Role: RoleUser, Text: text, Time: m.CreateTime.Time}, promptID: m.ID}
			for _, p := range pointers {
				pid := chatgptPointerID(p)
				name := names[pid]
				if name == "" {
					name = pid
				}
				cur.prompt.Images = append(cur.prompt.Images, imageRef(p, name))
			}
		case "assistant", "tool":
			if cur == nil {
				continue
			}
			for _, p := range pointers {
				cur.replyImages = append(cur.replyImages, imageRef(p, chatgptPointerID(p)))
			}
			if m.Author.Role != "assistant" || (m.Recipient != "" && m.Recipient != "all") {
				continue
			}
			if m.Content.ContentType != "text" && m.Content.ContentType != "multimodal_text" {
				continue
			}
			if text != "" {
				replies = append(replies, text)
				cur.reply.Time = m.CreateTime.Time
			}
		}
	}
	flush()
	return th, nil
}

func containsPointerID(pointers []string, id string) bool {
	for _, p := range pointers {
		if chatgptPointerID(p) == id {
			return true
		}
	}
	return false
}
