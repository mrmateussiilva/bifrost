package main

import (
	"context"
)

// ProviderFactory is the singleton factory that manages a specific LLM web provider (e.g., Gemini, ChatGPT).
type ProviderFactory interface {
	// Name returns the provider's identifier (e.g. "gemini", "chatgpt").
	Name() string

	// Open navigates the provided tab context to the provider's URL.
	Open(ctx context.Context) error

	// State inspects the DOM and returns the current authentication/page state.
	State(ctx context.Context) (pageState, string, error)

	// NewWorker instantiates a worker bound to the provided tab context.
	NewWorker(ctx context.Context) LLMWorker

	// Models returns the list of valid models supported by this provider.
	Models() []modelObject

	// DefaultModel returns the default model to use if none is provided.
	DefaultModel() string
}

// LLMWorker is the motor that runs inside a specific browser tab to execute generations.
type LLMWorker interface {
	// Complete sends a prompt and waits for the final response.
	Complete(ctx context.Context, messages []Message, model string) (string, error)

	// CompleteStream sends a prompt and streams the response via callbacks.
	CompleteStream(ctx context.Context, messages []Message, model string, onStart func() error, onDelta func(string) error) (string, error)
}

// GetProvider returns the appropriate ProviderFactory based on the configured name.
func GetProvider(name string) ProviderFactory {
	switch name {
	case "chatgpt":
		return &chatGPTFactory{}
	case "gemini":
		fallthrough
	default:
		return &geminiFactory{}
	}
}
