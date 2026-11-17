package openai

import (
	"encoding/json"
	"regexp"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Wire shapes for the OpenAI Chat Completions request. JSON field names are snake_case
// provider names and never cross the port boundary. OpenAI deprecates max_tokens for
// newer models, so the request sends max_completion_tokens instead.

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Tools               []chatTool    `json:"tools,omitempty"`
	ToolChoice          string        `json:"tool_choice"`
	ParallelToolCalls   bool          `json:"parallel_tool_calls"`
	Stream              bool          `json:"stream"`
	Temperature         float64       `json:"temperature"`
	MaxCompletionTokens int           `json:"max_completion_tokens"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
}

type chatTool struct {
	Type     string  `json:"type"`
	Function toolDef `json:"function"`
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function toolCallFn `json:"function"`
}

type toolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// translateTools validates caller tool names/schemas and appends the reserved finish_call
// tool whose parameters are the outcome schema.
func translateTools(tools []domain.Tool, outcomeSchema []byte) ([]chatTool, error) {
	out := make([]chatTool, 0, len(tools)+1)
	seen := map[string]bool{}
	for _, t := range tools {
		if !toolNamePattern.MatchString(t.Name) {
			return nil, errInvalidToolName(t.Name)
		}
		if t.Name == reservedToolName {
			return nil, errReservedToolName
		}
		if seen[t.Name] {
			return nil, errDuplicateToolName(t.Name)
		}
		seen[t.Name] = true
		if !isJSONObjectSchema(t.Parameters) {
			return nil, errInvalidToolSchema(t.Name)
		}
		out = append(out, chatTool{
			Type: "function",
			Function: toolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	out = append(out, chatTool{
		Type: "function",
		Function: toolDef{
			Name:        reservedToolName,
			Description: "Finalize this call with the structured outcome.",
			Parameters:  outcomeSchema,
		},
	})
	return out, nil
}

// isJSONObjectSchema reports whether raw is a JSON object whose "type" is "object".
func isJSONObjectSchema(raw []byte) bool {
	var v struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	if v.Type != "object" {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return len(obj) > 0
}
