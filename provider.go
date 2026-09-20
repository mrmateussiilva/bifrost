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

// StreamHooks são os ganchos de streaming do worker — o que o CompleteStream
// invoca conforme a geração avança.
//
// OnStart roda assim que o envio do prompt está confirmado: o momento certo
// de escrever cabeçalhos SSE, porque erros anteriores (sessão, DOM, envio)
// ainda podem virar status HTTP de verdade.
//
// OnDelta recebe cada acréscimo de texto e pode devolver erro para ABORTAR
// a geração (o handler usa isso para detectar recusa de ferramenta antes de
// qualquer byte chegar ao cliente e retentar com correção).
//
// OnToolCall (opcional) recebe chamadas de ferramenta detectadas
// PRECOCEMENTE: blocos de chamada que fecharam e estabilizaram no meio da
// geração — o cliente pode começar a executar a primeira chamada enquanto o
// resto da resposta ancora. Exige Classify. Pode devolver erro para abortar
// (mesma semântica de OnDelta).
//
// Classify (opcional) decide se o conteúdo de um code block com forma de
// chamada é uma ferramenta DECLARADA no request — fechamento sobre o mapa de
// tools do lado do handler; sem ele o worker não emite chamadas precoces
// (a tradução final continua valendo).
type StreamHooks struct {
	OnStart    func() error
	OnDelta    func(string) error
	OnToolCall func(toolCall) error
	Classify   func(code string) (toolCall, bool)
}

// LLMWorker is the motor that runs inside a specific browser tab to execute generations.
type LLMWorker interface {
	// Complete sends a prompt and waits for the final response.
	Complete(ctx context.Context, messages []Message, model string) (string, error)

	// CompleteStream sends a prompt and streams the response via callbacks.
	CompleteStream(ctx context.Context, messages []Message, model string, hooks StreamHooks) (string, error)
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
