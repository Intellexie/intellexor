package session

import "encoding/json"

// Version is the release version. Overridable at build time via
// -ldflags "-X github.com/Intellexie/intellexor/internal/session.Version=<tag>"
// (the release workflow injects the git tag).
var Version = "1.15.0"

type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

type CacheTokens struct {
	Read  float64 `json:"read"`
	Write float64 `json:"write"`
}

type Tokens struct {
	Total     *float64    `json:"total,omitempty"`
	Input     float64     `json:"input"`
	Output    float64     `json:"output"`
	Reasoning float64     `json:"reasoning"`
	Cache     CacheTokens `json:"cache"`
}

type Share struct {
	URL string `json:"url"`
}

type Path struct {
	Cwd  string `json:"cwd"`
	Root string `json:"root"`
}

type PermissionRule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     string `json:"action"`
}

type SessionTime struct {
	Created    int64  `json:"created"`
	Updated    int64  `json:"updated"`
	Compacting *int64 `json:"compacting,omitempty"`
	Archived   *int64 `json:"archived,omitempty"`
}

type FileDiff struct {
	File      string `json:"file,omitempty"`
	Patch     string `json:"patch,omitempty"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Status    string `json:"status,omitempty"`
}

type SessionSummary struct {
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
	Files     int        `json:"files"`
	Diffs     []FileDiff `json:"diffs,omitempty"`
}

type Session struct {
	ID         string           `json:"id"`
	Slug       string           `json:"slug"`
	ProjectID  string           `json:"projectID"`
	Directory  string           `json:"directory"`
	Path       string           `json:"path,omitempty"`
	ParentID   string           `json:"parentID,omitempty"`
	Title      string           `json:"title"`
	Version    string           `json:"version"`
	Agent      string           `json:"agent,omitempty"`
	Model      *ModelRef        `json:"model,omitempty"`
	Cost       float64          `json:"cost"`
	Tokens     Tokens           `json:"tokens"`
	Context    float64          `json:"context,omitempty"`
	Share      *Share           `json:"share,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
	Time       SessionTime      `json:"time"`
	Permission []PermissionRule `json:"permission,omitempty"`
	Summary    *SessionSummary  `json:"summary,omitempty"`
	Revert     *RevertState     `json:"revert,omitempty"`
}

type RevertState struct {
	MessageID string `json:"messageID"`
	PartID    string `json:"partID,omitempty"`
	Snapshot  string `json:"snapshot,omitempty"`
	Diff      string `json:"diff,omitempty"`
}

// ---- messages ----

type MsgTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed,omitempty"`
}

type UserMsgTime struct {
	Created int64 `json:"created"`
}

type UserModel struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
	Variant    string `json:"variant,omitempty"`
}

type UserSummary struct {
	Title string     `json:"title,omitempty"`
	Body  string     `json:"body,omitempty"`
	Diffs []FileDiff `json:"diffs"`
}

type NamedError struct {
	Name string         `json:"name"`
	Data map[string]any `json:"data"`
}

func NewNamedError(name string, data map[string]any) *NamedError {
	if data == nil {
		data = map[string]any{}
	}
	return &NamedError{Name: name, Data: data}
}

type UserMessage struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	Role      string          `json:"role"`
	Time      UserMsgTime     `json:"time"`
	Agent     string          `json:"agent"`
	Model     UserModel       `json:"model"`
	Format    json.RawMessage `json:"format,omitempty"`
	Summary   *UserSummary    `json:"summary,omitempty"`
	System    string          `json:"system,omitempty"`
	Tools     map[string]bool `json:"tools,omitempty"`
}

type AssistantMessage struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionID"`
	Role       string          `json:"role"`
	ParentID   string          `json:"parentID"`
	Time       MsgTime         `json:"time"`
	Error      *NamedError     `json:"error,omitempty"`
	ModelID    string          `json:"modelID"`
	ProviderID string          `json:"providerID"`
	Mode       string          `json:"mode"`
	Agent      string          `json:"agent"`
	Path       Path            `json:"path"`
	Summary    bool            `json:"summary,omitempty"`
	Cost       float64         `json:"cost"`
	Tokens     Tokens          `json:"tokens"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Variant    string          `json:"variant,omitempty"`
	Finish     string          `json:"finish,omitempty"`
}

type WithParts struct {
	Info  any    `json:"info"`
	Parts []Part `json:"parts"`
}

// ---- parts ----

type PartTime struct {
	Start int64  `json:"start"`
	End   *int64 `json:"end,omitempty"`
}

type ToolTime struct {
	Start     int64  `json:"start"`
	End       *int64 `json:"end,omitempty"`
	Compacted *int64 `json:"compacted,omitempty"`
}

type ToolState struct {
	Status      string         `json:"status"`
	Input       map[string]any `json:"input,omitempty"`
	Raw         string         `json:"raw,omitempty"`
	Title       string         `json:"title,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Output      string         `json:"output,omitempty"`
	Error       string         `json:"error,omitempty"`
	Time        *ToolTime      `json:"time,omitempty"`
	Attachments []any          `json:"attachments,omitempty"`
}

type FileSource struct {
	Type       string          `json:"type"`
	Path       string          `json:"path,omitempty"`
	Text       *SourceText     `json:"text,omitempty"`
	Name       string          `json:"name,omitempty"`
	Kind       int             `json:"kind,omitempty"`
	Range      json.RawMessage `json:"range,omitempty"`
	ClientName string          `json:"clientName,omitempty"`
	URI        string          `json:"uri,omitempty"`
}

type SourceText struct {
	Value string  `json:"value"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type Part struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`

	Text      string         `json:"text,omitempty"`
	Synthetic bool           `json:"synthetic,omitempty"`
	Ignored   bool           `json:"ignored,omitempty"`
	Time      *PartTime      `json:"time,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`

	// reasoning
	// (shares text/time/metadata)

	// tool
	CallID string     `json:"callID,omitempty"`
	Tool   string     `json:"tool,omitempty"`
	State  *ToolState `json:"state,omitempty"`

	// step-finish
	Reason   string   `json:"reason,omitempty"`
	Snapshot string   `json:"snapshot,omitempty"`
	Cost     *float64 `json:"cost,omitempty"`
	Tokens   *Tokens  `json:"tokens,omitempty"`

	// patch
	Hash  string   `json:"hash,omitempty"`
	Files []string `json:"files,omitempty"`

	// file
	Mime     string      `json:"mime,omitempty"`
	Filename string      `json:"filename,omitempty"`
	URL      string      `json:"url,omitempty"`
	Source   *FileSource `json:"source,omitempty"`

	// agent
	Name string `json:"name,omitempty"`

	// subtask
	Prompt      string    `json:"prompt,omitempty"`
	Description string    `json:"description,omitempty"`
	Agent       string    `json:"agent,omitempty"`
	Model       *ModelRef `json:"model,omitempty"`
	Command     string    `json:"command,omitempty"`

	// retry
	Attempt int         `json:"attempt,omitempty"`
	Error   *NamedError `json:"error,omitempty"`

	// compaction
	Auto        *bool  `json:"auto,omitempty"`
	Overflow    *bool  `json:"overflow,omitempty"`
	TailStartID string `json:"tail_start_id,omitempty"`
}

// Attachment helper: data-URI file part used for tool attachments
func FileAttachment(id, sessionID, messageID, mime, dataURL, filename string) Part {
	return Part{
		ID: id, SessionID: sessionID, MessageID: messageID, Type: "file",
		Mime: mime, URL: dataURL, Filename: filename,
	}
}

type ToolCallRef struct {
	MessageID string `json:"messageID"`
	CallID    string `json:"callID"`
}

type PermissionRequest struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"sessionID"`
	Permission string         `json:"permission"`
	Patterns   []string       `json:"patterns"`
	Metadata   map[string]any `json:"metadata"`
	Always     []string       `json:"always"`
	Tool       *ToolCallRef   `json:"tool,omitempty"`
}
