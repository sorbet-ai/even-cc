// Package events defines the app-facing SSE event shapes. Field order matches
// even-terminal's JSON key order exactly (docs/even-protocol.md §5).
package events

import "encoding/json"

const Provider = "claude"

type UserPrompt struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func NewUserPrompt(text string) UserPrompt { return UserPrompt{Type: "user_prompt", Text: text} }

type Status struct {
	Type      string `json:"type"`
	State     string `json:"state"`
	SessionID string `json:"sessionId"`
	Provider  string `json:"provider"`
}

func NewStatus(state, sessionID string) Status {
	return Status{Type: "status", State: state, SessionID: sessionID, Provider: Provider}
}

type TextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolStart struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	ToolID string `json:"toolId"`
}

type ToolDetail struct {
	Input  json.RawMessage `json:"input"`
	Output *string         `json:"output,omitempty"`
}

type ToolEnd struct {
	Type    string     `json:"type"`
	Name    string     `json:"name"`
	ToolID  string     `json:"toolId"`
	Summary string     `json:"summary"`
	Detail  ToolDetail `json:"detail"`
}

type Option struct {
	Text string `json:"text"`
	Key  string `json:"key"`
}

type PermissionRequest struct {
	Type        string          `json:"type"`
	ToolName    string          `json:"toolName"`
	Description string          `json:"description"`
	Detail      string          `json:"detail"`
	ToolUseID   string          `json:"toolUseId"`
	Options     []Option        `json:"options"`
	Suggestions json.RawMessage `json:"suggestions"` // raw array or null
}

type PermissionResult struct {
	Type     string `json:"type"`
	ToolName string `json:"toolName"`
	Summary  string `json:"summary"`
	Decision string `json:"decision"` // allowed | always | denied
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview"`
}

type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	MultiSelect bool             `json:"multiSelect"`
	Options     []QuestionOption `json:"options"`
}

type UserQuestion struct {
	Type      string     `json:"type"`
	Questions []Question `json:"questions"`
	ToolUseID string     `json:"toolUseId"`
}

type QuestionAnswer struct {
	Type    string          `json:"type"`
	Answers json.RawMessage `json:"answers"`
}

type Notification struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

func NewNotification(title, message string) Notification {
	return Notification{Type: "notification", Title: title, Message: message}
}

type RunningStats struct {
	Type         string `json:"type"`
	DurationMs   int64  `json:"durationMs"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
}

type Result struct {
	Type         string  `json:"type"`
	Success      bool    `json:"success"`
	Text         string  `json:"text"`
	SessionID    string  `json:"sessionId"`
	CostUsd      float64 `json:"costUsd"`
	Provider     string  `json:"provider"`
	Turns        int64   `json:"turns"`
	DurationMs   int64   `json:"durationMs"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
}

type TaskProgress struct {
	Type      string `json:"type"`
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
	Current   string `json:"current"`
}

type Error struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func NewError(msg string) Error { return Error{Type: "error", Message: msg} }
