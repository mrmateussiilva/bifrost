package main

import (
	"context"
	"errors"
)

type chatGPTFactory struct{}

func (f *chatGPTFactory) Name() string {
	return "chatgpt"
}

func (f *chatGPTFactory) Open(ctx context.Context) error {
	// TODO: chromedp.Navigate("https://chatgpt.com")
	return errors.New("chatgpt driver not fully implemented")
}

func (f *chatGPTFactory) State(ctx context.Context) (pageState, string, error) {
	// TODO: Evaluate DOM for #prompt-textarea or login button
	return stateUnknown, "https://chatgpt.com", nil
}

func (f *chatGPTFactory) NewWorker(ctx context.Context) LLMWorker {
	return &ChatGPT{ctx: ctx}
}

func (f *chatGPTFactory) Models() []modelObject {
	return []modelObject{
		{ID: "gpt-4o", Object: "model", ContextLength: 128000},
		{ID: "gpt-4o-mini", Object: "model", ContextLength: 128000},
		{ID: "o1-preview", Object: "model", ContextLength: 128000},
	}
}

func (f *chatGPTFactory) DefaultModel() string {
	return "gpt-4o-mini"
}

type ChatGPT struct {
	ctx context.Context
}

func (c *ChatGPT) Complete(ctx context.Context, messages []Message, model string) (string, error) {
	return "", errors.New("chatgpt driver not implemented")
}

func (c *ChatGPT) CompleteStream(ctx context.Context, messages []Message, model string, onStart func() error, onDelta func(string) error) (string, error) {
	return "", errors.New("chatgpt driver not implemented")
}
