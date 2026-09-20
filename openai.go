package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
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
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Control/Nudge marcam os blocos de sistema INTERNOS do Bifrost — nunca
	// vêm do cliente (json:"-"). São excluídos do casamento de histórico da
	// conversa aderente (gemini.go): o protocolo de tools é infraestrutura
	// reenviada a cada turno; a correção de retratativa é instrução efêmera.
	Control bool `json:"-"`
	Nudge   bool `json:"-"`
}

// UnmarshalJSON aceita "content" nos três formatos que clientes enviam:
// string (padrão), null (mensagens de assistant só com tool_calls) e array
// de parts no formato multi-part do OpenAI ([{"type":"text","text":...}]).
// Sem isto, clientes que usam parts tomam 400 antes de chegar ao Gemini.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []toolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.ToolCalls = raw.ToolCalls
	m.ToolCallID = raw.ToolCallID
	switch {
	case len(raw.Content) == 0 || string(raw.Content) == "null":
		m.Content = ""
	case raw.Content[0] == '"':
		return json.Unmarshal(raw.Content, &m.Content)
	default:
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw.Content, &parts); err != nil {
			return fmt.Errorf("content: esperado string, null ou array de parts: %w", err)
		}
		ts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Text != "" {
				ts = append(ts, p.Text)
			}
		}
		m.Content = strings.Join(ts, "\n")
	}
	return nil
}

// toolCall é a chamada de função no formato OpenAI — tanto no request
// (histórico que o cliente reenvia) quanto na resposta (o que o Bifrost
// devolve ao parsear os blocos tool_call do Gemini). Index só existe nos
// chunks de streaming (pointer: 0 é significativo lá e omitido aqui).
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolDef é a descrição de uma ferramenta no request. Strict espelha o modo
// strict do OpenAI (aderência exata ao schema): o Bifrost não pode garanti-la,
// mas a instrui no protocolo.
type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      bool            `json:"strict,omitempty"`
	} `json:"function"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatCompletionRequest struct {
	Model             string          `json:"model"`
	Messages          []Message       `json:"messages"`
	Stream            bool            `json:"stream"`
	StreamOptions     *streamOptions  `json:"stream_options"`
	Tools             []toolDef       `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
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
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	ContextLength int    `json:"context_length,omitempty"` // janela de contexto em tokens (estimada)
}

// Chunks de streaming — chat.completion.chunk, o formato que os clientes
// SSE de OpenAI já sabem consumir.

type chunkDelta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
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

func handleModels(gw *Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, modelsList{Object: "list", Data: gw.factory.Models()})
	}
}

func handleModel(gw *Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := modelObjectFor(gw, chi.URLParam(r, "model"))
		if !ok {
			writeAPIError(w, http.StatusNotFound, errInvalidRequest, "model_not_found",
				"modelo desconhecido: "+strconv.Quote(chi.URLParam(r, "model"))+"; válidos: "+validModelIDs(gw))
			return
		}
		writeJSON(w, http.StatusOK, m)
	}
}

// conversationKey identifica a conversa entre turnos do mesmo agente: hash
// do início do histórico (primeiras 2 mensagens com conteúdo) — estável
// enquanto o cliente reenvia o histórico crescendo. No multi-profile, a
// chave guia a AFINIDADE: turnos consecutivos caem no shard onde a conversa
// aderente já vive.
func conversationKey(msgs []Message) string {
	h := fnv.New64a()
	n := 0
	for _, m := range msgs {
		if m.Control || m.Nudge || m.Content == "" {
			continue
		}
		fmt.Fprintf(h, "%s\x00%s\x00", m.Role, m.Content)
		if n++; n == 2 {
			break
		}
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// contextLengthFor retorna a janela de contexto estimada para cada modelo.
// O Gemini 2.5 Pro e Flash têm 1M tokens; Flash Lite tem 1M também.
// Valores conservadores — a UI web pode ter limites menores em prática.
func contextLengthFor(id string) int {
	switch {
	case strings.Contains(id, "flash-lite"):
		return 1_000_000
	case strings.Contains(id, "flash"):
		return 1_000_000
	default: // pro, gemini-web
		return 1_000_000
	}
}

func modelObjectFor(gw *Gateway, id string) (modelObject, bool) {
	for _, m := range gw.factory.Models() {
		if m.ID == id {
			return m, true
		}
	}
	return modelObject{}, false
}

func validModelIDs(gw *Gateway) string {
	models := gw.factory.Models()
	ids := make([]string, 0, len(models))
	for _, m := range models {
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
			req.Model = gw.factory.DefaultModel()
		}
		if _, ok := modelObjectFor(gw, req.Model); !ok {
			writeAPIError(w, http.StatusNotFound, errInvalidRequest, "model_not_found",
				"modelo desconhecido: "+strconv.Quote(req.Model)+"; válidos: "+validModelIDs(gw))
			return
		}

		// fila limitada: 1 executando + 4 esperando; além disso, 429
		// na cara em vez de cliente preso por minutos
		leave, ok := gw.tryAdmit()
		if !ok {
			thePanel.reject()
			writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", "queue_full",
				"fila cheia; tente novamente em instantes")
			return
		}
		defer leave()

		// toda chamada ao Gemini tem timeout; r.Context() morre se o
		// cliente desconectar
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()

		shardIdx, g, release, err := gw.acquire(ctx, conversationKey(req.Messages))
		if err != nil {
			thePanel.reject()
			if errors.Is(err, context.DeadlineExceeded) {
				writeAPIError(w, http.StatusGatewayTimeout, "rate_limit_error", "queue_timeout",
					"tempo esgotado aguardando a vez na fila")
			} else {
				writeAPIError(w, http.StatusServiceUnavailable, errAPIError, "browser_unavailable", err.Error())
			}
			return
		}
		defer release()

		// function calling simulado: tool_choice "none" descarta as tools;
		// senão, protocolo + schemas entram como bloco [SYSTEM] no fim do
		// prompt serializado. Com resultados [TOOL] no histórico, ganha a
		// instrução anti-repetição (o loop multi-turn depende dela: sem o
		// aviso, o modelo re-emite chamadas cujo resultado já chegou).
		var msgs []Message
		if len(req.Tools) > 0 && string(req.ToolChoice) != `"none"` {
			parallelOK := req.ParallelToolCalls == nil || *req.ParallelToolCalls
			strictAny := false
			for _, t := range req.Tools {
				if t.Function.Strict {
					strictAny = true
					break
				}
			}
			toolPrompt := serializeTools(req.Tools, req.ToolChoice, parallelOK, strictAny)
			for _, m := range req.Messages {
				if m.Role == "tool" {
					toolPrompt += "\n" + multiTurnToolInstruction
					break
				}
			}
			msgs = make([]Message, 0, len(req.Messages)+1)
			msgs = append(msgs, req.Messages...)
			msgs = append(msgs, Message{Role: "system", Content: toolPrompt, Control: true})
		} else {
			msgs = req.Messages
		}

		promptText := SerializeMessages(msgs)
		slog.Info("request received", "messages", len(msgs), "model", req.Model, "stream", req.Stream, "tools", len(req.Tools))

		// rastro para o painel: começa quando o request ganhou a vez
		rec := thePanel.begin(req.Model, req.Stream, len(req.Tools), promptPreview(promptText, 110))
		defer thePanel.end(rec)

		if req.Stream {
			streamChatCompletion(w, ctx, gw, shardIdx, g, req, msgs, promptText, rec)
			return
		}

		text, err := g.Complete(ctx, msgs, req.Model)
		if err != nil {
			slog.Error("completion falhou", "err", err)
			if errors.Is(err, ErrGeminiNotLoggedIn) {
				gw.markNoSession(shardIdx) // fora da rotação até login
			} else if errors.Is(err, ErrPageUnresponsive) {
				gw.markBrowserSuspect(shardIdx) // relanço no próximo acquire
			}
			_, _, code, msg := completionErrorInfo(err)
			thePanel.mutate(rec, func(r *reqRecord) {
				r.Status = code
				r.Err = msg
				r.FullPrompt = promptText
			})
			writeCompletionError(w, err)
			return
		}

		declared := make(map[string]bool, len(req.Tools))
		for _, t := range req.Tools {
			declared[t.Function.Name] = true
		}
		calls, content, _ := parseToolCalls(text, declared)

		// recusa probabilística do Gemini web: sem chamada e abrindo com
		// recusa, re-executa com correção — a geração recusada fica no
		// vácuo (conversa nova a cada requisição). Ladder igual ao do
		// streaming: segunda recusa ganha uma última tentativa, cujo
		// resultado é aceito como vier (o agente reage à recusa visível).
		// Chamada como texto puro NÃO tem retratativa aqui: o parser final
		// extrai o JSON bruto de qualquer posição (scanRawToolCalls).
		if len(calls) == 0 && len(req.Tools) > 0 && looksLikeRefusal(content) {
			slog.Warn("recusa de ferramenta detectada; retentando com correção")
			thePanel.noteRetry(rec)
			retryMsgs := append(append(make([]Message, 0, len(msgs)+1), msgs...),
				Message{Role: "system", Content: refusalCorrection, Nudge: true})
			if text2, err2 := g.Complete(ctx, retryMsgs, req.Model); err2 == nil {
				calls2, content2, _ := parseToolCalls(text2, declared)
				if len(calls2) == 0 && looksLikeRefusal(content2) {
					slog.Warn("recusa persistente; última tentativa sem inspeção")
					thePanel.noteRetry(rec)
					// mesma correção, resultado aceito como vier — espelha
					// o ladder do streaming (a última tentativa lá também
					// reusa as mensagens, só desliga a inspeção)
					if text3, err3 := g.Complete(ctx, retryMsgs, req.Model); err3 == nil {
						calls3, content3, _ := parseToolCalls(text3, declared)
						text, calls, content = text3, calls3, content3
					}
				} else {
					text, calls, content = text2, calls2, content2
				}
			}
		}

		// texto em vez de chamada de ferramenta: modelo gerou o conteúdo
		// (documentação, código, etc.) como texto puro quando deveria ter
		// chamado a ferramenta de escrita. Re-executa com correção específica.
		if len(calls) == 0 && len(req.Tools) > 0 && looksLikeMissedToolCall(content, req.Tools) {
			slog.Warn("modelo gerou texto em vez de chamar ferramenta de escrita; retentando")
			thePanel.noteRetry(rec)
			retryMsgs := append(append(make([]Message, 0, len(msgs)+1), msgs...),
				Message{Role: "system", Content: missedToolCallCorrection, Nudge: true})
			if text2, err2 := g.Complete(ctx, retryMsgs, req.Model); err2 == nil {
				calls2, content2, _ := parseToolCalls(text2, declared)
				text, calls, content = text2, calls2, content2
			}
		}

		calls = dedupeCalls(calls)

		if len(calls) > 0 {
			slog.Info("tool calls parsed", "calls", len(calls))
			thePanel.mutate(rec, func(r *reqRecord) {
				r.Status = "tool_calls"
				r.ToolCalls = len(calls)
				r.Chars = len(content)
				r.FullPrompt = promptText
				r.FullResponse = text
			})
			writeJSON(w, http.StatusOK, ChatCompletionResponse{
				ID:      "chatcmpl-bifrost-" + randomID(),
				Object:  "chat.completion",
				Created: time.Now().Unix(),
				Model:   req.Model,
				Choices: []Choice{{
					Index:        0,
					Message:      Message{Role: "assistant", Content: content, ToolCalls: calls},
					FinishReason: "tool_calls",
					Logprobs:     nil,
				}},
				Usage: estimateUsage(promptText, text),
			})
			return
		}

		thePanel.mutate(rec, func(r *reqRecord) {
			r.Status = "ok"
			r.Chars = len(text)
			r.FullPrompt = promptText
			r.FullResponse = text
		})
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
	case errors.Is(err, ErrPageUnresponsive):
		// renderer engasgado (conversa/prompt gigante): 503 — o cliente
		// pode retentar; o reload da aba já foi tentado dentro do complete
		return http.StatusServiceUnavailable, errAPIError, "gemini_page_unresponsive", err.Error()
	case errors.Is(err, ErrBrowserClosed):
		return http.StatusServiceUnavailable, errAPIError, "browser_closed", err.Error()
	case errors.Is(err, ErrGenerationTimeout):
		return http.StatusGatewayTimeout, errAPIError, "generation_timeout", err.Error()
	case errors.Is(err, ErrGenerationFailed):
		// UI do Gemini exibiu erro (botão "Tentar novamente" etc.): 502 com
		// código específico — permite ao cliente retentativas imediatas.
		return http.StatusBadGateway, errAPIError, "gemini_generation_failed", err.Error()
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
// conforme a geração avança, chunks delta.tool_calls — precoces, assim que
// um bloco de chamada fecha e estabiliza no meio da geração (Classify +
// OnToolCall), e o restante na tradução final — chunk final com
// finish_reason, usage opcional (stream_options.include_usage) e [DONE].
// Os fences de chamada nunca vazam como conteúdo (o loop de streaming os
// retém); blocos com forma de chamada mas nome não-declarado chegam como
// delta tardio. Erros ANTES do primeiro byte saem como status HTTP
// normais; depois dele, como evento de erro + [DONE] — o protocolo não
// permite trocar o status no meio do stream.
func streamChatCompletion(w http.ResponseWriter, ctx context.Context, gw *Gateway, shardIdx int, g LLMWorker, req ChatCompletionRequest, msgs []Message, promptText string, rec *reqRecord) {
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
		if started {
			return nil // retratativa: cabeçalhos e chunk de papel já foram
		}
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

	// recusa de ferramenta: o PRIMEIRO delta de conteúdo é inspecionado
	// antes de ir ao cliente — recusa aborta a emissão com nada vazado, e a
	// retratativa emenda na mesma conexão SSE. Também detecta "missed tool
	// call" (modelo começa a narrar o conteúdo em vez de chamar a
	// ferramenta de escrita) e "plain text call" (chamada emitida como JSON
	// solto no texto, fora de code block — sem isso ela vaza como conteúdo
	// E vira tool_calls, duplicada no cliente). Se uma chamada JÁ foi
	// emitida precocemente, o modelo atendeu ao protocolo: as inspeções
	// desistem (espelha o não-stream, onde calls>0 pula as retratativas).
	declared := make(map[string]bool, len(req.Tools))
	for _, t := range req.Tools {
		declared[t.Function.Name] = true
	}

	// emissão de chamadas: um índice sequencial compartilhado entre a
	// emissão PRECOCE (bloco fechou no meio da geração, via OnToolCall) e a
	// tradução final — chamada já emitida não é re-enviada no fim (dedupe
	// por conteúdo), e duplicatas exatas do mesmo turno são descartadas
	// (bug clássico do Gemini web de re-emitir a mesma chamada).
	callSeq := 0
	seenCalls := map[string]bool{}
	emitCall := func(c toolCall) {
		key := c.Function.Name + "\x00" + c.Function.Arguments
		if seenCalls[key] {
			slog.Warn("chamada duplicada descartada (stream)", "tool", c.Function.Name)
			return
		}
		seenCalls[key] = true
		idx := callSeq
		c.Index = &idx
		callSeq++
		chunk(chunkDelta{ToolCalls: []toolCall{c}}, nil)
	}

	firstDelta := true
	checkRefusal := true
	callsOut := 0 // chamadas emitidas precocemente nesta tentativa
	onDelta := func(d string) error {
		if firstDelta {
			firstDelta = false
			if checkRefusal && len(req.Tools) > 0 && callsOut == 0 {
				if c, _ := scanRawToolCalls(d, declared); len(c) > 0 {
					return errPlainTextCall
				}
				if looksLikeRefusal(d) {
					return errRefusalDetected
				}
				if looksLikeMissedToolCall(d, req.Tools) {
					return errMissedToolCall
				}
			}
		}
		chunk(chunkDelta{Content: d}, nil)
		thePanel.addChars(rec, len(d))
		return nil
	}
	hooks := StreamHooks{
		OnStart: onStart,
		OnDelta: onDelta,
		Classify: func(code string) (toolCall, bool) {
			return tryBuildToolCall(code, declared)
		},
		OnToolCall: func(c toolCall) error {
			callsOut++
			emitCall(c)
			return nil
		},
	}
	attempt := func(msgs []Message, check bool) (string, error) {
		firstDelta = true
		checkRefusal = check
		callsOut = 0
		callSeq = 0
		seenCalls = map[string]bool{} // tentativa abortada não deixa rastro
		return g.CompleteStream(ctx, msgs, req.Model, hooks)
	}

	// Retratativa 1: recusa explícita de ferramenta
	finalText, err := attempt(msgs, true)
	if errors.Is(err, errRefusalDetected) {
		slog.Warn("recusa de ferramenta detectada (stream); retentando com correção")
		thePanel.noteRetry(rec)
		retryMsgs := make([]Message, 0, len(msgs)+1)
		retryMsgs = append(retryMsgs, msgs...)
		retryMsgs = append(retryMsgs, Message{Role: "system", Content: refusalCorrection, Nudge: true})
		finalText, err = attempt(retryMsgs, true)
		if errors.Is(err, errRefusalDetected) {
			// insistiu na recusa: última tentativa sem inspeção — o que
			// vier vai ao cliente (recusa visível, o agente reage)
			slog.Warn("recusa persistente; última tentativa sem inspeção")
			thePanel.noteRetry(rec)
			finalText, err = attempt(retryMsgs, false)
		}
	}

	// Retratativa 2: chamada de ferramenta como texto puro (sem code block)
	if errors.Is(err, errPlainTextCall) {
		slog.Warn("chamada de ferramenta como texto puro (stream); retentando com correção")
		thePanel.noteRetry(rec)
		retryMsgsP := make([]Message, 0, len(msgs)+1)
		retryMsgsP = append(retryMsgsP, msgs...)
		retryMsgsP = append(retryMsgsP, Message{Role: "system", Content: plainTextCallCorrection, Nudge: true})
		finalText, err = attempt(retryMsgsP, true)
		if errors.Is(err, errPlainTextCall) {
			// insistiu: última tentativa sem inspeção — o parser final
			// extrai a chamada crua do texto (scanRawToolCalls), o cliente
			// recebe o tool_call mesmo com o conteúdo redundante
			slog.Warn("plain text call persistente; última tentativa sem inspeção")
			thePanel.noteRetry(rec)
			finalText, err = attempt(retryMsgsP, false)
		}
	}

	// Retratativa 3: texto em vez de chamada de ferramenta de escrita
	if errors.Is(err, errMissedToolCall) {
		slog.Warn("modelo gerou texto em vez de chamar ferramenta de escrita (stream); retentando")
		thePanel.noteRetry(rec)
		retryMsgs2 := make([]Message, 0, len(msgs)+1)
		retryMsgs2 = append(retryMsgs2, msgs...)
		retryMsgs2 = append(retryMsgs2, Message{Role: "system", Content: missedToolCallCorrection, Nudge: true})
		finalText, err = attempt(retryMsgs2, true)
		if errors.Is(err, errMissedToolCall) {
			// insistiu: última tentativa sem inspeção
			slog.Warn("missed tool call persistente; última tentativa sem inspeção")
			thePanel.noteRetry(rec)
			finalText, err = attempt(retryMsgs2, false)
		}
	}
	if err != nil {
		slog.Error("completion falhou (stream)", "err", err)
		if errors.Is(err, ErrGeminiNotLoggedIn) {
			gw.markNoSession(shardIdx) // fora da rotação até login
		} else if errors.Is(err, ErrPageUnresponsive) {
			gw.markBrowserSuspect(shardIdx) // relanço no próximo acquire
		}
		_, errType, code, msg := completionErrorInfo(err)
		// rastro completo do erro no painel: código, mensagem e o prompt
		// que foi enviado — sem isso o histórico só mostra "erro" seco
		thePanel.mutate(rec, func(r *reqRecord) {
			r.Status = code
			r.Err = msg
			r.FullPrompt = promptText
		})
		if !started {
			writeCompletionError(w, err)
			return
		}
		s.event(apiError{Error: apiErrorBody{Message: msg, Type: errType, Code: code}})
		s.raw("data: [DONE]\n\n")
		return
	}

	// chamadas de ferramenta: blocos retidos com forma de chamada mas nome
	// não-declarado chegam como delta tardio de conteúdo; os declarados
	// viram chunks delta.tool_calls — os ainda não emitidos precocemente
	// (emitCall deduplica por conteúdo e mantém a sequência de índices).
	// Morph raro de part já emitida: prevalece a versão precoce (o cliente
	// já a tem); a divergência segue para o log.
	calls, _, late := parseToolCalls(finalText, declared)
	if late != "" {
		chunk(chunkDelta{Content: late}, nil)
	}
	for _, c := range calls {
		emitCall(c)
	}
	if callSeq > 0 {
		slog.Info("tool calls parsed (stream)", "calls", callSeq)
		finish := "tool_calls"
		chunk(chunkDelta{}, &finish)
		thePanel.mutate(rec, func(r *reqRecord) {
			r.Status = "tool_calls"
			r.ToolCalls = callSeq
			r.FullPrompt = promptText
			r.FullResponse = finalText
		})
	} else {
		finish := "stop"
		chunk(chunkDelta{}, &finish)
		thePanel.mutate(rec, func(r *reqRecord) {
			r.Status = "ok"
			r.FullPrompt = promptText
			r.FullResponse = finalText
		})
	}
	thePanel.mutate(rec, func(r *reqRecord) { r.Chars = len(finalText) })
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
// Function calling simulado — a UI web do Gemini não tem protocolo nativo
// de tools, então: schemas + protocolo entram como texto no prompt, o modelo
// emite chamadas como code blocks de JSON (que a extração estrutural entrega
// como fences prontos) e o Bifrost os traduz para o formato tool_calls do
// OpenAI.
// ---------------------------------------------------------------------------

const fence = "```"

// schemaShape lê o essencial de um json schema de parâmetros — só o que o
// protocolo usa: obrigatórios e propriedades (nome → tipo).
type schemaShape struct {
	Type       string                     `json:"type"`
	Required   []string                   `json:"required"`
	Properties map[string]json.RawMessage `json:"properties"`
}

// requiredParams extrai a lista de parâmetros obrigatórios do schema.
func requiredParams(schema json.RawMessage) []string {
	if len(schema) == 0 {
		return nil
	}
	var s schemaShape
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil
	}
	return s.Required
}

// placeholderFor devolve um valor de exemplo do tipo do parâmetro.
func placeholderFor(prop json.RawMessage) any {
	var p struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(prop, &p) != nil {
		return "VALOR"
	}
	switch p.Type {
	case "number", "integer":
		return 1
	case "boolean":
		return true
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	default:
		return "VALOR"
	}
}

// exampleArgs sintetiza argumentos de exemplo a partir do schema
// (top-level): obrigatórios primeiro; sem obrigatórios, os dois primeiros
// nomes de propriedade. Falha de parse → {} — o exemplo segue válido, só
// menos didático.
func exampleArgs(schema json.RawMessage) string {
	if len(schema) == 0 {
		return "{}"
	}
	var s schemaShape
	if err := json.Unmarshal(schema, &s); err != nil {
		return "{}"
	}
	order := s.Required
	if len(order) == 0 {
		for name := range s.Properties {
			order = append(order, name)
		}
		sort.Strings(order)
		if len(order) > 2 {
			order = order[:2]
		}
	}
	args := make(map[string]any, len(order))
	for _, name := range order {
		args[name] = placeholderFor(s.Properties[name])
	}
	out, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// exampleCallJSON monta a linha de exemplo {"name","arguments"} para uma
// ferramenta REAL do request — o modelo casa o exemplo com o schema que
// ele mesmo tem que preencher, em vez de uma ferramenta hipotética.
func exampleCallJSON(t toolDef) string {
	return fmt.Sprintf(`{"name": %q, "arguments": %s}`, t.Function.Name, exampleArgs(t.Function.Parameters))
}

// writeExampleJSON constrói o exemplo do caso CRÍTICO para a ferramenta de
// escrita REAL: parâmetro content-like vira texto markdown com escape
// visível ("# Meu Projeto\n..." — a lição de escape dentro de arguments),
// path-like vira "docs/README.md", os demais seguem o placeholder do tipo.
// Sem parâmetros reconhecíveis cai no exampleCallJSON genérico.
func writeExampleJSON(t toolDef) string {
	if len(t.Function.Parameters) == 0 {
		return exampleCallJSON(t)
	}
	var s schemaShape
	if err := json.Unmarshal(t.Function.Parameters, &s); err != nil || len(s.Required) == 0 {
		return exampleCallJSON(t)
	}
	contentish := []string{"content", "file_text", "file_str", "text", "codigo", "corpo"}
	pathish := []string{"path", "file", "filename", "nome"}
	args := make(map[string]any, len(s.Required))
	for _, name := range s.Required {
		l := strings.ToLower(name)
		switch {
		case containsAny(l, contentish): // antes: "file_str" é content, não path
			args[name] = "# Meu Projeto\n..."
		case containsAny(l, pathish):
			args[name] = "docs/README.md"
		default:
			args[name] = placeholderFor(s.Properties[name])
		}
	}
	out, err := json.Marshal(args)
	if err != nil {
		return exampleCallJSON(t)
	}
	return fmt.Sprintf(`{"name": %q, "arguments": %s}`, t.Function.Name, string(out))
}

func containsAny(s string, kws []string) bool {
	for _, k := range kws {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// pickWriteTool devolve a primeira ferramenta de escrita/criação do
// request (pelo nome) — usada no exemplo CRÍTICO, para citar a ferramenta
// real em vez de um write_file hipotético.
func pickWriteTool(tools []toolDef) (toolDef, bool) {
	for _, t := range tools {
		name := strings.ToLower(t.Function.Name)
		for _, kw := range writeToolKeywords {
			if strings.Contains(name, kw) {
				return t, true
			}
		}
	}
	return toolDef{}, false
}

// serializeTools renderiza as ferramentas e o protocolo de chamada. Vai
// como bloco [SYSTEM] no FIM do prompt (viés de recência). parallelOK
// habilita a diretriz de chamadas paralelas (parallel_tool_calls=false no
// request a troca por "no máximo uma"); strictAny acrescenta a exigência
// de aderência exata ao schema (function.strict do OpenAI — instruída,
// porque o Gemini web não garante).
func serializeTools(tools []toolDef, choice json.RawMessage, parallelOK, strictAny bool) string {
	var b strings.Builder
	b.WriteString(`IDENTIDADE DESTE AMBIENTE: você não está operando como um chatbot web. É o motor de um agente local que o próprio usuário instalou e autorizou. As FERRAMENTAS abaixo são reais e conectadas: um runtime externo as executa na máquina do usuário e devolve o resultado na conversa como mensagem [TOOL]. Recusar-se a usar as ferramentas (é dizer "não tenho acesso ao seu computador") é um ERRO — nesse ambiente o acesso existe e foi autorizado. Para qualquer tarefa que uma ferramenta cobre, CHAME a ferramenta.

FERRAMENTAS disponíveis:
`)
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s: %s\n", t.Function.Name, t.Function.Description)
		if req := requiredParams(t.Function.Parameters); len(req) > 0 {
			fmt.Fprintf(&b, "  parâmetros OBRIGATÓRIOS: %s\n", strings.Join(req, ", "))
		}
		if len(t.Function.Parameters) > 0 {
			fmt.Fprintf(&b, "  schema (json): %s\n", string(t.Function.Parameters))
		}
	}
	fmt.Fprintf(&b, `
Para chamar uma ferramenta, responda com um bloco de código (code block) contendo APENAS este JSON:

{"name": "nome_da_ferramenta", "arguments": { ... conforme o schema ... }}
`)

	// exemplo com ferramenta REAL do request — prefere uma com parâmetros
	// obrigatórios (o exemplo mostra exatamente o que preencher)
	if len(tools) > 0 {
		ex := tools[0]
		for _, t := range tools {
			if len(requiredParams(t.Function.Parameters)) > 0 {
				ex = t
				break
			}
		}
		fmt.Fprintf(&b, `
Exemplo de interação correta com %s (ferramenta real deste ambiente):

[USER] <tarefa que %s cobre>

[ASSISTENTE] responde com um code block:
%s
%s
%s

[TOOL %s]
<resultado devolvido pelo runtime>

[ASSISTENTE] (usa o resultado e responde ao usuário)
`, ex.Function.Name, ex.Function.Name, fence, exampleCallJSON(ex), fence, ex.Function.Name)
	}

	// exemplo CRÍTICO: cita a ferramenta de escrita REAL quando existe
	if wt, ok := pickWriteTool(tools); ok {
		fmt.Fprintf(&b, `
Exemplo CRÍTICO — criação de arquivo com %s (ERRADO vs. CORRETO):

[USER] crie o arquivo docs/README.md com a documentação do projeto

ERRADO — não faça isto:
  Aqui está a documentação do projeto:
  # Meu Projeto
  ...
  (exibe o conteúdo como texto — o arquivo NÃO é criado)

CORRETO — faça assim:
%s
%s
%s
`, wt.Function.Name, fence, writeExampleJSON(wt), fence)
	} else {
		fmt.Fprintf(&b, `
Exemplo CRÍTICO — criação de arquivo (ERRADO vs. CORRETO):

[USER] crie o arquivo docs/README.md com a documentação do projeto

ERRADO — não faça isto:
  Aqui está a documentação do projeto:
  # Meu Projeto
  ...
  (exibe o conteúdo como texto — o arquivo NÃO é criado)

CORRETO — faça assim:
%s
{"name": "write_file", "arguments": {"path": "docs/README.md", "content": "# Meu Projeto\\n..."}}
%s
`, fence, fence)
	}

	b.WriteString(`
Regras:
- "arguments" DEVE ser um objeto JSON válido conforme o schema, e nada além do JSON dentro do bloco.
- Strings com quebras de linha ou aspas precisam de escape JSON (\n, \"); números e booleanos SEM aspas.
`)
	if parallelOK {
		b.WriteString("- Chamadas INDEPENDENTES: emita todas na MESMA resposta, um bloco por chamada — o runtime executa em paralelo.\n- Chamadas DEPENDENTES: uma por vez — espere o resultado [TOOL] antes da próxima.\n")
	} else {
		b.WriteString("- Nesta resposta, emita NO MÁXIMO UMA chamada de ferramenta.\n")
	}
	b.WriteString(`- NUNCA exiba o conteúdo de um arquivo que deveria ser criado/escrito — use a ferramenta.
- Chamou? Pare e espere o resultado — nunca invente nem descreva um resultado que não chegou.
- Se nenhuma ferramenta cobre a tarefa, responda em markdown normal, SEM bloco de chamada.
`)
	if strictAny {
		b.WriteString("- Aderência ESTRITA ao schema: use exatamente os parâmetros declarados, sem campos extras e sem omitir obrigatórios.\n")
	}
	if d := toolChoiceDirective(choice); d != "" {
		b.WriteString("\n" + d + "\n")
	}
	return b.String()
}

// dedupeCalls descarta chamadas idênticas na MESMA resposta (mesma
// ferramenta E mesmos argumentos): o bug clássico do Gemini web re-emite
// a chamada cujo resultado ainda não voltou; executá-la duas vezes só
// desperdiça um turno do agente. A primeira ocorrência fica.
func dedupeCalls(calls []toolCall) []toolCall {
	if len(calls) < 2 {
		return calls
	}
	seen := make(map[string]bool, len(calls))
	out := make([]toolCall, 0, len(calls))
	for _, c := range calls {
		key := c.Function.Name + "\x00" + c.Function.Arguments
		if seen[key] {
			slog.Warn("chamada duplicada descartada", "tool", c.Function.Name)
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

// multiTurnToolInstruction: apêndice ao protocolo quando o histórico já
// contém resultados [TOOL] — sem ele, o modelo tende a repetir chamadas
// cujo resultado já chegou, travando o loop do agente.
const multiTurnToolInstruction = `CONTEXTO DE CONTINUAÇÃO: você está no meio de um loop de agente. Chamadas anteriores que você fez JÁ FORAM EXECUTADAS pelo runtime — os resultados estão no histórico acima como blocos [TOOL nome_da_ferramenta]. NÃO repita uma chamada cujo resultado já está no histórico. Use os resultados recebidos para: (a) responder ao usuário em markdown, se a tarefa está completa; ou (b) fazer a PRÓXIMA chamada necessária (um bloco por chamada). Nunca descreva um resultado que não chegou como [TOOL].`

// toolChoiceDirective traduz tool_choice em instrução extra (vazio = auto).
func toolChoiceDirective(choice json.RawMessage) string {
	if len(choice) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(choice, &s); err == nil {
		switch s {
		case "required":
			return "Nesta resposta você DEVE chamar pelo menos uma das ferramentas."
		}
		return "" // "auto" (e "none", que nem chega aqui: as tools são descartadas)
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(choice, &obj) == nil && obj.Function.Name != "" {
		return "Nesta resposta você DEVE chamar a ferramenta " + obj.Function.Name + "."
	}
	return ""
}

// ---------------------------------------------------------------------------
// Parsing de tool calls — dois bugs clássicos com conteúdo gerado por LLM:
//
//  1. O JSON do tool call contém newlines LITERAIS nas strings (pre.innerText
//     expande \n reais); json.Unmarshal falha em JSON inválido.
//
//  2. O conteúdo do arquivo a ser escrito contém ``` (fences de markdown),
//     que terminam prematuramente o bloco de código — fenceRe quebra.
//
// Solução:
//   a) repairJSON escapa chars de controle literais dentro de strings JSON.
//   b) findCodeBlocks usa um parser linha-a-linha que só fecha o bloco
//      quando encontra ``` numa linha só, sem mais conteúdo depois.
//   c) scanRawToolCalls é o fallback: varre o texto inteiro buscando JSON
//      balanceado com formato de tool call, sem depender de fences.
// ---------------------------------------------------------------------------

// repairJSON escapa chars de controle literais (\n, \r, \t) dentro de strings
// JSON que o modelo gerou sem escapar. Usa máquina de estado para saber se
// está dentro de uma string e não tocar em chars fora delas.
func repairJSON(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 64)
	inStr, esc := false, false
	for _, r := range s {
		switch {
		case esc:
			b.WriteRune(r)
			esc = false
		case r == '\\' && inStr:
			b.WriteRune(r)
			esc = true
		case r == '"':
			b.WriteRune(r)
			inStr = !inStr
		case inStr && r == '\n':
			b.WriteString(`\n`)
		case inStr && r == '\r':
			b.WriteString(`\r`)
		case inStr && r == '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// extractBalancedJSON extrai o primeiro objeto JSON balanceado começando em s[0].
// Retorna o objeto e quantos bytes foram consumidos (0 se s não começa com '{').
func extractBalancedJSON(s string) (obj string, n int) {
	if len(s) == 0 || s[0] != '{' {
		return "", 0
	}
	depth, inStr, esc := 0, false, false
	for i, r := range s {
		switch {
		case esc:
			esc = false
		case r == '\\' && inStr:
			esc = true
		case r == '"':
			inStr = !inStr
		case inStr:
			// dentro de string: não conta chaves
		case r == '{':
			depth++
		case r == '}':
			depth--
			if depth == 0 {
				return s[:i+1], i + 1
			}
		}
	}
	return "", 0
}

// findCodeBlocks substitui fenceRe: extrai code blocks linha-a-linha, fechando
// apenas quando ``` ocupa uma linha inteira (sem mais conteúdo). Isso evita
// fechar prematuramente quando ``` aparece dentro do conteúdo do bloco.
//
// Retorna slices [outerStart, outerEnd, contentStart, contentEnd] em bytes.
func findCodeBlocks(text string) [][4]int {
	var blocks [][4]int
	i := 0
	for i < len(text) {
		// Procura ``` no início de linha
		if !strings.HasPrefix(text[i:], "```") || (i > 0 && text[i-1] != '\n') {
			i++
			continue
		}
		outerStart := i
		i += 3
		// Pula o identificador de linguagem até o fim da linha
		for i < len(text) && text[i] != '\n' {
			i++
		}
		if i < len(text) {
			i++ // consome o '\n' após o identificador
		}
		contentStart := i
		// Procura ``` de fechamento: deve ser a única coisa na linha
		for i < len(text) {
			if strings.HasPrefix(text[i:], "```") && (i == 0 || text[i-1] == '\n') {
				// Verifica que o restante da linha é apenas ``` + espaços
				j := i + 3
				for j < len(text) && text[j] == ' ' {
					j++
				}
				if j >= len(text) || text[j] == '\n' {
					// Fechamento legítimo
					contentEnd := i
					outerEnd := j
					if outerEnd < len(text) && text[outerEnd] == '\n' {
						outerEnd++
					}
					blocks = append(blocks, [4]int{outerStart, outerEnd, contentStart, contentEnd})
					i = outerEnd
					goto nextBlock
				}
			}
			i++
		}
		// Sem fechamento encontrado: não é um bloco completo
		i = contentStart
	nextBlock:
	}
	return blocks
}

// tryBuildToolCall tenta construir um toolCall a partir do conteúdo de um
// code block (ou JSON bruto). Tenta primeiro o JSON como está, depois com
// repairJSON. Desembrulha wrappers comuns ({"tool_call": {...}},
// {"function_call": {...}}) que o modelo emite apesar do protocolo.
// Retorna false se não for um tool call válido e declarado.
func tryBuildToolCall(content string, declared map[string]bool) (toolCall, bool) {
	return tryBuildToolCallDepth(content, declared, 0)
}

func tryBuildToolCallDepth(content string, declared map[string]bool, depth int) (toolCall, bool) {
	content = strings.TrimSpace(content)
	if !looksLikeToolCallJSON(content) {
		return toolCall{}, false
	}
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		// Tenta com reparo de chars de controle literais
		if err2 := json.Unmarshal([]byte(repairJSON(content)), &raw); err2 != nil {
			return toolCall{}, false
		}
	}
	// wrapper de nível único: {"tool_call": {...}} etc. — o valor interno
	// precisa ser objeto; a forma externa já passou por looksLikeToolCallJSON
	if raw.Name == "" && depth < 3 {
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(content), &probe) == nil && len(probe) == 1 {
			for _, v := range probe {
				if inner := strings.TrimSpace(string(v)); strings.HasPrefix(inner, "{") {
					return tryBuildToolCallDepth(inner, declared, depth+1)
				}
			}
		}
	}
	if raw.Name == "" || !declared[raw.Name] {
		return toolCall{}, false
	}
	args := strings.TrimSpace(string(raw.Arguments))
	if args == "" {
		args = "{}"
	}
	if strings.HasPrefix(args, `"`) {
		var s string
		if json.Unmarshal([]byte(args), &s) != nil || !json.Valid([]byte(s)) {
			// Tenta reparar o JSON da string
			fixed := repairJSON(args)
			if json.Unmarshal([]byte(fixed), &s) != nil || !json.Valid([]byte(s)) {
				return toolCall{}, false
			}
		}
		args = s
	} else if !json.Valid([]byte(args)) {
		fixed := repairJSON(args)
		if !json.Valid([]byte(fixed)) {
			return toolCall{}, false
		}
		args = fixed
	}
	var c toolCall
	c.ID = "call-" + randomID()
	c.Type = "function"
	c.Function.Name = raw.Name
	c.Function.Arguments = args
	return c, true
}

// scanRawToolCalls é o fallback de último recurso: varre o texto completo
// buscando objetos JSON balanceados com forma de tool call, sem depender de
// code blocks. Útil quando o modelo emite o JSON sem fence, ou quando o
// fence foi corrompido pelo conteúdo.
func scanRawToolCalls(text string, declared map[string]bool) (calls []toolCall, positions [][2]int) {
	for i := 0; i < len(text); {
		if text[i] != '{' {
			i++
			continue
		}
		obj, n := extractBalancedJSON(text[i:])
		if n == 0 {
			i++
			continue
		}
		if c, ok := tryBuildToolCall(obj, declared); ok {
			calls = append(calls, c)
			positions = append(positions, [2]int{i, i + n})
			i += n
			continue
		}
		i++
	}
	return
}

// parseToolCalls extrai as chamadas de ferramenta do texto da resposta.
// Chamada = code block (ou JSON bruto) com conteúdo JSON {"name","arguments"}
// onde name é uma ferramenta DECLARADA no request. Devolve:
//   - calls: as chamadas no formato OpenAI
//   - stripped: o texto sem os blocos de chamada
//   - late: blocos com forma de chamada mas nome não-declarado (streaming)
func parseToolCalls(text string, declared map[string]bool) (calls []toolCall, stripped, late string) {
	type hit struct {
		start, end int
		calls      []toolCall
		isLate     bool
	}
	var hits []hit
	seenStart := map[int]bool{}

	// Passo 1: code blocks via findCodeBlocks (robusto contra backticks
	// aninhados). Um bloco pode conter VÁRIOS objetos JSON (chamadas
	// paralelas que o modelo juntou num bloco só) — todos são extraídos.
	for _, b := range findCodeBlocks(text) {
		outerStart, outerEnd, cStart, cEnd := b[0], b[1], b[2], b[3]
		if seenStart[outerStart] {
			continue
		}
		content := strings.TrimSpace(text[cStart:cEnd])
		var blockCalls []toolCall
		blockLate := false
		for i := 0; i < len(content); {
			if content[i] != '{' {
				i++
				continue
			}
			obj, n := extractBalancedJSON(content[i:])
			if n == 0 {
				i++
				continue
			}
			if c, ok := tryBuildToolCall(obj, declared); ok {
				blockCalls = append(blockCalls, c)
			} else if looksLikeToolCallJSON(obj) {
				blockLate = true
			}
			i += n
		}
		switch {
		case len(blockCalls) > 0:
			hits = append(hits, hit{outerStart, outerEnd, blockCalls, false})
			seenStart[outerStart] = true
		case blockLate:
			// Tem forma de tool call mas nome não declarado → late
			hits = append(hits, hit{outerStart, outerEnd, nil, true})
			seenStart[outerStart] = true
		}
	}

	// Passo 2: JSON bruto fora de code blocks (fallback)
	// Só busca em regiões que não já foram cobertas por code blocks.
	covered := func(pos int) bool {
		for _, h := range hits {
			if pos >= h.start && pos < h.end {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(text); {
		if covered(i) || text[i] != '{' {
			i++
			continue
		}
		obj, n := extractBalancedJSON(text[i:])
		if n == 0 {
			i++
			continue
		}
		if c, ok := tryBuildToolCall(obj, declared); ok {
			hits = append(hits, hit{i, i + n, []toolCall{c}, false})
		}
		i += max(n, 1)
	}

	// Ordena por posição no texto
	sort.Slice(hits, func(a, b int) bool { return hits[a].start < hits[b].start })

	// Separa calls de lateFences
	var lateFences []string
	var cuts [][2]int
	for _, h := range hits {
		if h.isLate {
			lateFences = append(lateFences, text[h.start:h.end])
			continue
		}
		calls = append(calls, h.calls...)
		cuts = append(cuts, [2]int{h.start, h.end})
	}

	if len(calls) == 0 {
		return nil, text, strings.Join(lateFences, "\n\n")
	}
	var sb strings.Builder
	prev := 0
	for _, cut := range cuts {
		if cut[0] > prev {
			sb.WriteString(text[prev:cut[0]])
		}
		prev = cut[1]
	}
	sb.WriteString(text[prev:])
	stripped = regexp.MustCompile(`\n{3,}`).ReplaceAllString(sb.String(), "\n\n")
	return calls, strings.TrimSpace(stripped), strings.Join(lateFences, "\n\n")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Retratativa de recusa — o prior de segurança do Gemini web ("não tenho
// acesso ao computador do usuário") é probabilístico: com as ferramentas
// declaradas e framing de agente, o modelo ora chama, ora recusa. Quando a
// resposta começa com recusa, o Bifrost aborta (antes de qualquer byte ir
// ao cliente, no streaming) e re-executa com uma correção.
// ---------------------------------------------------------------------------

var errRefusalDetected = errors.New("recusa de ferramenta detectada")
var errMissedToolCall = errors.New("respondeu com texto em vez de chamar a ferramenta")
var errPlainTextCall = errors.New("chamada de ferramenta emitida como texto puro")

// writeToolKeywords: termos no nome de ferramentas que indicam operação de
// escrita/criação de arquivo. Usado para detectar o caso "modelo gerou texto
// em vez de chamar a ferramenta de escrita".
var writeToolKeywords = []string{
	"write", "create", "save", "edit", "insert", "update", "patch",
	"escrever", "criar", "salvar", "editar",
	"str_replace", "new_file", "overwrite",
}

// hasWriteTools verifica se alguma das ferramentas declaradas parece ser de
// escrita/criação de arquivo (pelo nome). Usado para decidir se um "respondeu
// com texto" merece uma retratativa.
func hasWriteTools(tools []toolDef) bool {
	for _, t := range tools {
		name := strings.ToLower(t.Function.Name)
		for _, kw := range writeToolKeywords {
			if strings.Contains(name, kw) {
				return true
			}
		}
	}
	return false
}

// looksLikeMissedToolCall: o modelo gerou uma resposta longa de texto puro
// (sem chamada de ferramenta) quando existem ferramentas de escrita
// disponíveis. Heurística de DOIS gates para não queimar ~6s de
// retratativa em resposta legítima: (1) abertura conversativa/título
// (regex) e >= 150 chars; (2) CORPO com cara de arquivo exibido — fence
// de código ou título markdown em linha própria. Sem o segundo gate,
// "claro, vamos fazer X" + explicação tomava retratativa à toa.
var missedToolCallRe = regexp.MustCompile(`(?i)^(#|##|###|aqui (está|estão)|here (is|are)|claro|certo|ok,|of course|sure,|vou criar|vou escrever|segue|abaixo)`)

// missedToolBodyRe: evidência de conteúdo de arquivo exibido como texto —
// fence de código ou título markdown iniciando linha.
var missedToolBodyRe = regexp.MustCompile("(?m)(^```|^#{1,3} \\S)")

func looksLikeMissedToolCall(content string, tools []toolDef) bool {
	if !hasWriteTools(tools) {
		return false
	}
	c := strings.TrimSpace(content)
	if len(c) < 150 {
		return false
	}
	prefix := c
	if len(prefix) > 200 {
		prefix = prefix[:200]
	}
	if !missedToolCallRe.MatchString(strings.TrimSpace(prefix)) {
		return false
	}
	body := c
	if len(body) > 600 {
		body = body[:600]
	}
	return missedToolBodyRe.MatchString(body)
}

// refusalRe casa as formulações de recusa de acesso/capacidade (pt e en),
// observadas em testes contra o Gemini web.
var refusalRe = regexp.MustCompile(`(?i)(` + strings.Join([]string{
	`n[ãa]o (?:posso|consigo) (?:acessar|ler|escrever|criar|executar|rodar|manipular|interagir|salvar|modificar)`,
	`n[ãa]o (?:posso|consigo) executar comandos`,
	`n[ãa]o (?:tenho|possuo) (?:acesso|ferramentas?|permiss[ãa]o|a capacidade)`,
	`n[ãa]o consigo (?:ler|acessar) arquivos`,
	`n[ãa]o tenho como (?:acessar|ler|executar)`,
	`n[ãa]o tenho acesso ao (?:seu )?(?:computador|sistema|sistema de arquivos|dispositivo)`,
	`sou (?:apenas )?(?:uma IA|um modelo|um assistente virtual|um chatbot)`,
	`(?:can'?t|cannot|unable to) (?:access|read|write|execute|run|create|save|modify)`,
	`don'?t have (?:access|permission|tools|the ability)`,
	`i'?m (?:just )?(?:an AI|a language model|an assistant)`,
	`as an AI(?: language model)?`,
}, `|`) + `)`)

// looksLikeRefusal: recusa no começo da resposta (elas sempre abrem a
// resposta — verificar o início evita falso positivo com discussão sobre
// limitações no meio do texto).
func looksLikeRefusal(content string) bool {
	if len(content) > 300 {
		content = content[:300]
	}
	return refusalRe.MatchString(content)
}

// refusalCorrection é a mensagem [SYSTEM] da retratativa de recusa.
const refusalCorrection = "Sua resposta anterior foi uma RECUSA incorreta e foi descartada. Neste ambiente as ferramentas são REAIS: o usuário as instalou e autorizou, e um runtime externo as executa devolvendo o resultado como [TOOL]. Responda novamente: se a tarefa envolve uma ferramenta, comece com o bloco de chamada; não repita a recusa."

// missedToolCallCorrection: mensagem de correção quando o modelo gerou o
// conteúdo como texto em vez de chamar a ferramenta de escrita.
const missedToolCallCorrection = "Sua resposta anterior exibiu o conteúdo como texto, mas a tarefa exige criar/escrever um arquivo usando a ferramenta disponível. Isso NÃO criou o arquivo. Refazer: use a ferramenta de escrita (write_file, create_file, str_replace_editor ou equivalente) com o conteúdo que você gerou como argumento. NÃO exiba o conteúdo novamente como texto — chame a ferramenta."

// plainTextCallCorrection: mensagem de correção quando o modelo emitiu a
// chamada como JSON solto no texto em vez de um code block.
const plainTextCallCorrection = "Sua resposta anterior emitiu a chamada de ferramenta como TEXTO puro (JSON solto no corpo da resposta), fora de um bloco de código — o runtime não consegue extrair assim de forma confiável. Re-emitir: a chamada deve ser um CODE BLOCK cercado por ``` contendo APENAS o objeto JSON {\"name\": ..., \"arguments\": ...}. Se quiser explicar algo, escreva o texto ANTES do bloco."

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
