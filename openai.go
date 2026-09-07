package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Formato OpenAI-compatible — o mínimo para clientes padrão funcionarem,
// com o envelope de erros e campos que esses clientes já reconhecem.

// gemini-web mantém o modo atual da UI; os demais ids vêm do registro
// geminiModes (gemini.go), alimentado pela tabela do comando `bifrost modes`.
const geminiWebModel = "gemini-web"

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatCompletionRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options"`
}

type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Logprobs     any     `json:"logprobs"` // null; presente porque clientes esperam a chave
}

// Usage é estimado (~4 chars/token): a UI web do Gemini não expõe contagem
// de tokens real.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// Chunks de streaming — chat.completion.chunk, o formato que os clientes
// SSE de OpenAI já sabem consumir.

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type chunkChoice struct {
	Index        int         `json:"index"`
	Delta        chunkDelta  `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type chatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type modelsList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// modelsCreated fixa o "created" dos modelos no boot do servidor.
var modelsCreated = time.Now().Unix()

// Erros no envelope OpenAI: {"error": {message, type, param, code}}.
type apiErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

type apiError struct {
	Error apiErrorBody `json:"error"`
}

const (
	errInvalidRequest = "invalid_request_error"
	errAPIError       = "api_error"
)

// ---------------------------------------------------------------------------
// Handlers.
// ---------------------------------------------------------------------------

func handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, modelsList{Object: "list", Data: modelObjects()})
}

func handleModel(w http.ResponseWriter, r *http.Request) {
	m, ok := modelObjectFor(chi.URLParam(r, "model"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, errInvalidRequest, "model_not_found",
			"modelo desconhecido: "+strconv.Quote(chi.URLParam(r, "model"))+"; válidos: "+validModelIDs())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func modelObjects() []modelObject {
	objs := []modelObject{{ID: geminiWebModel, Object: "model", Created: modelsCreated, OwnedBy: "bifrost"}}
	for _, m := range geminiModes {
		objs = append(objs, modelObject{ID: m.ID, Object: "model", Created: modelsCreated, OwnedBy: "bifrost"})
	}
	return objs
}

func modelObjectFor(id string) (modelObject, bool) {
	for _, m := range modelObjects() {
		if m.ID == id {
			return m, true
		}
	}
	return modelObject{}, false
}

// validModelIDs lista os ids aceitos (mensagem de 404 e validação de
// BIFROST_MODEL).
func validModelIDs() string {
	ids := make([]string, 0, len(geminiModes)+1)
	ids = append(ids, geminiWebModel)
	for _, m := range geminiModes {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ", ")
}

func handleHealth(gw *Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, gw.status())
	}
}

func handleChat(gw *Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			writeAPIError(w, http.StatusUnsupportedMediaType, errInvalidRequest, "unsupported_media_type",
				"Content-Type deve ser application/json")
			return
		}
		var req ChatCompletionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, errInvalidRequest, "invalid_request",
				"corpo inválido: "+err.Error())
			return
		}
		// drena o resto do body: sem ler até o EOF, o net/http não inicia a
		// leitura de fundo da conexão — e a desconexão do cliente nunca
		// cancela r.Context() (a geração seguiria até o fim no vácuo)
		_, _ = io.Copy(io.Discard, r.Body)
		if len(req.Messages) == 0 {
			writeAPIError(w, http.StatusBadRequest, errInvalidRequest, "empty_messages",
				"messages não pode ser vazio")
			return
		}
		if req.Model == "" {
			req.Model = gw.cfg.Model // BIFROST_MODEL
		}
		if req.Model == "" {
			req.Model = geminiWebModel
		}
		if _, ok := modelObjectFor(req.Model); !ok {
			writeAPIError(w, http.StatusNotFound, errInvalidRequest, "model_not_found",
				"modelo desconhecido: "+strconv.Quote(req.Model)+"; válidos: "+validModelIDs())
			return
		}

		// fila limitada: 1 executando + 4 esperando; além disso, 429
		// na cara em vez de cliente preso por minutos
		leave, ok := gw.tryAdmit()
		if !ok {
			writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", "queue_full",
				"fila cheia; tente novamente em instantes")
			return
		}
		defer leave()

		// toda chamada ao Gemini tem timeout; r.Context() morre se o
		// cliente desconectar
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()

		g, release, err := gw.acquire(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				writeAPIError(w, http.StatusGatewayTimeout, "rate_limit_error", "queue_timeout",
					"tempo esgotado aguardando a vez na fila")
			} else {
				writeAPIError(w, http.StatusServiceUnavailable, errAPIError, "browser_unavailable", err.Error())
			}
			return
		}
		defer release()

		promptText := SerializeMessages(req.Messages)
		slog.Info("request received", "messages", len(req.Messages), "model", req.Model, "stream", req.Stream)

		if req.Stream {
			streamChatCompletion(w, ctx, g, req, promptText)
			return
		}

		text, err := g.Complete(ctx, req.Messages, modeByID(req.Model))
		if err != nil {
			slog.Error("completion falhou", "err", err)
			writeCompletionError(w, err)
			return
		}

		writeJSON(w, http.StatusOK, ChatCompletionResponse{
			ID:      "chatcmpl-bifrost-" + randomID(),
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []Choice{{
				Index:        0,
				Message:      Message{Role: "assistant", Content: text},
				FinishReason: "stop",
				Logprobs:     nil,
			}},
			Usage: estimateUsage(promptText, text),
		})
	}
}

// completionErrorInfo classifica um erro do Gemini em status HTTP + envelope
// OpenAI — compartilhado pelo caminho comum e pelo de streaming.
func completionErrorInfo(err error) (status int, errType, code, msg string) {
	switch {
	case errors.Is(err, ErrGeminiNotLoggedIn):
		return http.StatusServiceUnavailable, errAPIError, "gemini_not_logged_in", err.Error()
	case errors.Is(err, ErrBrowserClosed):
		return http.StatusServiceUnavailable, errAPIError, "browser_closed", err.Error()
	case errors.Is(err, ErrGenerationTimeout):
		return http.StatusGatewayTimeout, errAPIError, "generation_timeout", err.Error()
	case errors.Is(err, ErrPromptNotFound), errors.Is(err, ErrResponseNotFound):
		return http.StatusBadGateway, errAPIError, "gemini_dom_error", err.Error()
	default:
		return http.StatusInternalServerError, errAPIError, "internal_error", err.Error()
	}
}

func writeCompletionError(w http.ResponseWriter, err error) {
	status, errType, code, msg := completionErrorInfo(err)
	writeAPIError(w, status, errType, code, msg)
}

// sseWriter escreve eventos SSE ("data: ...\n\n") com flush por evento.
type sseWriter struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func (s *sseWriter) event(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("serializar chunk", "err", err)
		return
	}
	s.raw("data: " + string(b) + "\n\n")
}

func (s *sseWriter) raw(msg string) {
	if _, err := io.WriteString(s.w, msg); err != nil {
		return // cliente foi embora; o ctx do request aborta a geração
	}
	s.fl.Flush()
}

// streamChatCompletion responde em SSE: chunk de papel, chunks de conteúdo
// conforme a geração avança, chunk final com finish_reason, usage opcional
// (stream_options.include_usage) e [DONE]. Erros ANTES do primeiro byte
// saem como status HTTP normais; depois dele, como evento de erro + [DONE]
// — o protocolo não permite trocar o status no meio do stream.
func streamChatCompletion(w http.ResponseWriter, ctx context.Context, g *Gemini, req ChatCompletionRequest, promptText string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, errAPIError, "stream_unavailable",
			"streaming indisponível neste servidor")
		return
	}

	s := &sseWriter{w: w, fl: flusher}
	id := "chatcmpl-bifrost-" + randomID()
	created := time.Now().Unix()
	chunk := func(delta chunkDelta, finish *string) {
		s.event(chatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []chunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		})
	}

	started := false
	onStart := func() error {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache, no-transform")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		chunk(chunkDelta{Role: "assistant"}, nil)
		started = true
		return nil
	}
	onDelta := func(d string) {
		chunk(chunkDelta{Content: d}, nil)
	}

	finalText, err := g.CompleteStream(ctx, req.Messages, modeByID(req.Model), onStart, onDelta)
	if err != nil {
		slog.Error("completion falhou (stream)", "err", err)
		if !started {
			writeCompletionError(w, err)
			return
		}
		_, errType, code, msg := completionErrorInfo(err)
		s.event(apiError{Error: apiErrorBody{Message: msg, Type: errType, Code: code}})
		s.raw("data: [DONE]\n\n")
		return
	}

	finish := "stop"
	chunk(chunkDelta{}, &finish)
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		u := estimateUsage(promptText, finalText)
		s.event(chatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []chunkChoice{},
			Usage:   &u,
		})
	}
	s.raw("data: [DONE]\n\n")
	slog.Info("stream finished", "chars", len(finalText))
}

func estimateUsage(prompt, completion string) Usage {
	p := (len(prompt) + 3) / 4
	c := (len(completion) + 3) / 4
	return Usage{PromptTokens: p, CompletionTokens: c, TotalTokens: p + c}
}

// ---------------------------------------------------------------------------
// Middleware e utilidades HTTP.
// ---------------------------------------------------------------------------

// statusWriter captura o status code para o access log.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wrote {
		sw.status = code
		sw.wrote = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wrote {
		sw.status = http.StatusOK
		sw.wrote = true
	}
	return sw.ResponseWriter.Write(b)
}

// Flush repassa o flush — sem isto, o wrapper esconderia o http.Flusher
// do handler de streaming e o SSE não sairia chunk a chunk.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// apiMiddleware dá o "cara de REST": request-id, CORS, access log com
// duração e recover de pânico respondendo JSON.
func apiMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := "req-" + randomID()
		w.Header().Set("X-Request-Id", reqID)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				slog.Error("panic no handler", "panic", p, "path", r.URL.Path)
				if !sw.wrote {
					writeAPIError(sw, http.StatusInternalServerError, errAPIError, "internal_error", "erro interno")
				}
			}
			slog.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"dur", time.Since(start).Round(time.Millisecond),
				"req_id", reqID,
			)
		}()
		next.ServeHTTP(sw, r)
	})
}

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, code int, errType, errCode, msg string) {
	writeJSON(w, code, apiError{Error: apiErrorBody{
		Message: msg,
		Type:    errType,
		Code:    errCode,
	}})
}

// authMiddleware exige API key (Bearer ou X-Api-Key) quando
// BIFROST_API_KEY está definida. /health e preflight CORS ficam abertos.
func authMiddleware(apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if apiKey == "" || r.URL.Path == "/health" || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				got = r.Header.Get("X-Api-Key")
			}
			if got != apiKey {
				writeAPIError(w, http.StatusUnauthorized, errInvalidRequest, "invalid_api_key",
					"api key ausente ou inválida (Authorization: Bearer ou X-Api-Key)")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
