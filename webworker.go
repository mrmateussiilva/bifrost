package main

// Motor genérico de worker para provedores de UI web (Gemini, ChatGPT).
// TUDO que é ciclo de request mora aqui — verificação de sessão com ladder
// de destravamento, conversa aderente, pré-envio re-executável com limpeza
// de editor, digitação, envio, espera e streaming — parametrizado por
// WebProvider (o que é específico de provedor: seletores, JS de estado,
// extração de parts, seleção de modelo, conversa nova). Extraído do
// gemini.go quando o driver do ChatGPT entrou: duplicar ~800 linhas de
// motor sutil (streaming estável, invariante sentText, ladder anti-wedge)
// seria duas cópias para manter.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// WebProvider é o contrato do driver de um provedor de UI web. O motor
// (WebWorker) roda o ciclo completo do request em cima destes ganchos.
type WebProvider interface {
	// State inspeciona o DOM e devolve o estado de autenticação (usado
	// pelos probes de sessão, status do gateway e fluxo de login do painel).
	State(ctx context.Context) (pageState, string, error)
	// GenState lê o estado da CONVERSA na forma genState (respostas, parts
	// da última resposta, texto do editor, botão parar, erro de UI) — é o
	// que faz todo o polling do motor funcionar igual por provedor.
	GenState(ctx context.Context) (genState, error)
	// ObserverJS devolve o JS do MutationObserver de streaming (protocolo
	// __BIFROST__ com {n: total de respostas, t: texto montado}).
	ObserverJS() string
	// EnsureMode seleciona o modelo na UI (o motor passa o id do request;
	// o driver resolve o registro interno e os cliques; id inválido
	// degrada com warn, não derruba o request).
	EnsureMode(ctx context.Context, model string) error
	// NewChat abre conversa nova.
	NewChat(ctx context.Context) error
	// HomeURL é a URL de página nova (conversa limpa) — usada pelo freshPage
	// da ladder de destravamento.
	HomeURL() string
	// PromptSelector / SendSelector: caixa de prompt e botão de envio.
	PromptSelector() string
	SendSelector() string
}

// WebWorker é o motor: uma aba, um provedor, uma interação por vez (mutex).
// O estado da conversa aderente e o canal do observer moram AQUI — os
// drivers (Gemini, ChatGPT) só conhecem o DOM.
type WebWorker struct {
	provider WebProvider
	ctx      context.Context

	stickyOK      bool
	lastBase      []Message
	lastModel     string
	lastResponses int
	lastProto     string
	dirty         bool

	mu sync.Mutex // uma interação por vez por aba

	streamMu sync.Mutex
	streamCh chan streamChunk
}

// NewWebWorker cria o motor sobre um provedor e liga o listener de chunks
// do observer (console CDP → canal).
func NewWebWorker(ctx context.Context, p WebProvider) *WebWorker {
	w := &WebWorker{provider: p, ctx: ctx}
	w.listenChunks()
	return w
}

// maxStickyResponses: acima disso a conversa aderente reabre — conversas
// muito longas desaceleram a UI do Gemini e acumulam desvio de contexto;
// o histórico completo é reenviado numa conversa nova.
const maxStickyResponses = 30

// stickyProtoReminder substitui o protocolo completo de tools em turnos
// aderentes com o MESMO toolset: o protocolo inteiro já está no início da
// conversa; re-digitar 28 schemas (~60KB) por turno só desperdiça tempo.
const stickyProtoReminder = `LEMBRETE DE PROTOCOLO: as ferramentas declaradas no início desta conversa seguem valendo, com as mesmas regras e schemas. Chamada = um code block contendo APENAS o JSON {"name": ..., "arguments": {...}}; uma chamada por bloco. Resultados chegam como blocos [TOOL nome_da_ferramenta]. Nunca repita chamada cujo resultado já chegou; nunca exiba conteúdo de arquivo como texto — chame a ferramenta. Se nenhuma ferramenta cobre a tarefa, responda em markdown normal.`

// messagesEqual compara mensagens pelo conteúdo que o cliente reenvia.
func messagesEqual(a, b Message) bool {
	if a.Role != b.Role || a.Content != b.Content || a.ToolCallID != b.ToolCallID {
		return false
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		ta, tb := a.ToolCalls[i], b.ToolCalls[i]
		if ta.ID != tb.ID || ta.Function.Name != tb.Function.Name || ta.Function.Arguments != tb.Function.Arguments {
			return false
		}
	}
	return true
}

// prefixMatch: prev é prefixo de cur (mesmas mensagens, cur pode continuar).
func prefixMatch(prev, cur []Message) bool {
	if len(prev) > len(cur) {
		return false
	}
	for i := range prev {
		if !messagesEqual(prev[i], cur[i]) {
			return false
		}
	}
	return true
}

// historyBase devolve o histórico "real" da conversa: as mensagens do
// cliente, sem os blocos de controle do Bifrost (protocolo de tools e
// correções de retratativa — infraestrutura, não conteúdo de conversa).
func historyBase(msgs []Message) []Message {
	base := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Control || m.Nudge {
			continue
		}
		base = append(base, m)
	}
	return base
}

// controlMsgs devolve os blocos de controle (protocolo + correções), na
// ordem original — reaproveitados no prompt de cada turno.
func controlMsgs(msgs []Message) []Message {
	var out []Message
	for _, m := range msgs {
		if m.Control || m.Nudge {
			out = append(out, m)
		}
	}
	return out
}

func hasNudge(msgs []Message) bool {
	for _, m := range msgs {
		if m.Nudge {
			return true
		}
	}
	return false
}

// elideTextAssistants remove as respostas de TEXTO do assistant: na
// conversa aderente o modelo já as emitiu — reenviá-las duplicaria o
// contexto (e são o grosso do payload). Assistant com tool_calls fica:
// o fence é minúsculo, funciona como recap da chamada e mantém o
// mapeamento tool_call_id → nome que rotula os resultados [TOOL nome].
func elideTextAssistants(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			continue
		}
		out = append(out, m)
	}
	return out
}

// toolResultMax: teto por resultado de tool no prompt serializado
// (BIFROST_TOOL_RESULT_MAX, chars; 0 desliga). Resultados gordos — output
// de bash, leitura de arquivo — são os que fazem a digitação do prompt
// destravar a página do Gemini (incidente 2026-09-20: sessão opencode com
// 68 mensagens pendurava o renderer por minutos). Elisão DETERMINÍSTICA
// (função pura do conteúdo): o mesmo histórico reenviado elide igual, e o
// casamento de prefixo da conversa aderente entre turnos não quebra.
var toolResultMax = envNonNegative("BIFROST_TOOL_RESULT_MAX", 8192)

// promptMax: orçamento global do prompt serializado (BIFROST_MAX_PROMPT,
// chars; 0 desliga). Incidente 2026-09-20 v3: prompt de 244KB (sessão
// opencode com 141 mensagens — fences de tool_call carregando arquivos
// inteiros nos argumentos + protocolo de 28 tools + system do opencode)
// wedgava a página instantaneamente e o aperto antigo não alcançava os
// fences. Agora a elisão alcança TUDO que é conversa (resultados,
// argumentos de chamada, texto antigo, working set em último recurso) —
// e o que não couber nem assim vira erro 400 context_length_exceeded em
// vez de digitar e wedgar. System/Control/Nudge nunca são elididos
// (identidade do agente + protocolo). 150KB: cabe o esqueleto do opencode
// (system ~45KB + protocolo ~60KB) + working set orçado.
var promptMax = envNonNegative("BIFROST_MAX_PROMPT", 150000)

// msgContentMax: teto SEMPRE ATIVO por mensagem de user/assistant (texto)
// — paste gigante do usuário no meio da sessão não pode chegar inteiro
// na digitação. Cabeça+marcador+cauda, determinístico.
const msgContentMax = 12288

// toolCallArgsMax: teto SEMPRE ATIVO por argumentos de tool_call
// serializado — chamadas write/edit carregam arquivos inteiros; acima
// disto os argumentos viram um marcador JSON (o fence segue válido, o
// mapa id→nome não depende dos argumentos).
const toolCallArgsMax = 4096

// elideToolResult trunca um conteúdo gordo em cabeça + marcador + cauda,
// cortando em fronteira de rune (o texto vai por CDP: UTF-8 quebrado no
// meio de runa não pode). Total ficado fica abaixo de max.
func elideToolResult(content string, max int) string {
	if max <= 0 || len(content) <= max {
		return content
	}
	head := max * 2 / 3
	tail := max / 6
	if head > len(content) {
		head = len(content)
	}
	for head > 0 && !utf8.RuneStart(content[head]) {
		head--
	}
	tailStart := len(content) - tail
	if tailStart <= head {
		tailStart = len(content) // sem espaço para cauda
	}
	for tailStart < len(content) && !utf8.RuneStart(content[tailStart]) {
		tailStart++
	}
	omitted := tailStart - head
	return content[:head] +
		fmt.Sprintf("\n\n[… bifrost: %d caracteres omitidos …]\n\n", omitted) +
		content[tailStart:]
}

// SerializeMessages transforma o histórico OpenAI em um prompt textual — a
// UI web do Gemini não aceita histórico estruturado. Chamadas de ferramenta
// do assistente viram fences e resultados chegam como mensagens [TOOL nome].
// Orçamentos em três níveis (tudo DETERMINÍSTICO — função pura do conteúdo,
// o mesmo histórico reenviado elide igual e o casamento de prefixo do
// sticky não quebra):
//   - sempre ativo: por-resultado (toolResultMax), por-mensagem de texto
//     (msgContentMax), por-argumentos de chamada (toolCallArgsMax)
//   - orçamento global: aperta o conteúdo ANTIGO (resultados → argumentos
//     de chamada → texto assistant → texto user, só-cabeça de 512)
//   - último recurso: degrada o working set (últimas mensagens) — melhor
//     contexto recente pior do que erro
//     System/Control/Nudge NUNCA são elididos (identidade do agente).
//     Ainda acima do orçamento → ErrPromptTooLarge (400 no cliente) —
//     digitar 244KB wedga a página; erro honesto é melhor.
func SerializeMessages(messages []Message) (string, error) {
	return serializeMessagesCap(messages, toolResultMax, promptMax)
}

// elideArgsMarker substitui argumentos gordos de tool_call por um
// marcador JSON válido (o fence segue parseável; o mapa id→nome do
// histórico usa só o nome da função).
func elideArgsMarker(args string) string {
	return fmt.Sprintf(`{"bifrost_args_omitidos": %d}`, len(args))
}

// serializeMessagesCap é o SerializeMessages com orçamentos explícitos
// (injetáveis em teste).
func serializeMessagesCap(messages []Message, perResult, globalMax int) (string, error) {
	b := serializeRaw(messages, perResult)
	if globalMax <= 0 || len(b) <= globalMax {
		return b, nil
	}
	const squeezeHead = 512
	const keepRecent = 6
	squeezed := make([]Message, len(messages))
	copy(squeezed, messages)
	old := func(i int) bool { return i < len(squeezed)-keepRecent }
	fit := func() (string, bool) {
		nb := serializeRaw(squeezed, perResult)
		return nb, len(nb) <= globalMax
	}
	done := func(stage string) (string, bool) {
		if nb, ok := fit(); ok {
			slog.Warn("prompt: orçamento global estourado; conteúdo apertado",
				"stage", stage, "prompt_chars", len(b), "capped_chars", len(nb), "max", globalMax)
			return nb, true
		}
		return "", false
	}

	// passe 1: resultados de tool antigos → só-cabeça
	for i := range squeezed {
		m := &squeezed[i]
		if m.Role != "tool" || m.Control || m.Nudge || !old(i) || len(m.Content) <= squeezeHead {
			continue
		}
		m.Content = elideToolResult(m.Content, squeezeHead)
		if nb, ok := done("tool-antigo"); ok {
			return nb, nil
		}
	}

	// passe 2: argumentos de tool_call antigos → marcador. É O BURACO DO
	// incidente 244KB: ~70 fences de write/edit carregando arquivos
	// inteiros — assistant com tool_calls era pulado no aperto.
	for i := range squeezed {
		m := &squeezed[i]
		if m.Role != "assistant" || m.Control || m.Nudge || !old(i) || len(m.ToolCalls) == 0 {
			continue
		}
		changed := false
		tcs := make([]toolCall, len(m.ToolCalls))
		copy(tcs, m.ToolCalls) // cópia profunda: o slice original é compartilhado
		for j := range tcs {
			if len(tcs[j].Function.Arguments) > squeezeHead {
				tcs[j].Function.Arguments = elideArgsMarker(tcs[j].Function.Arguments)
				changed = true
			}
		}
		if changed {
			m.ToolCalls = tcs
			if nb, ok := done("args-antigos"); ok {
				return nb, nil
			}
		}
	}

	// passes 3/4: texto antigo de assistant (sem chamadas) e user → só-cabeça
	for _, role := range []string{"assistant", "user"} {
		for i := range squeezed {
			m := &squeezed[i]
			if m.Role != role || m.Control || m.Nudge || !old(i) || len(m.ToolCalls) > 0 || len(m.Content) <= squeezeHead {
				continue
			}
			m.Content = elideToolResult(m.Content, squeezeHead)
			if nb, ok := done("texto-antigo"); ok {
				return nb, nil
			}
		}
	}

	// último recurso: working set degradado — as mensagens RECENTES (mais
	// antiga primeiro) também viram só-cabeça, e os argumentos de chamadas
	// recentes viram marcador. Contexto recente pior > erro para o cliente.
	for i := len(squeezed) - keepRecent; i < len(squeezed); i++ {
		if i < 0 {
			continue
		}
		m := &squeezed[i]
		if m.Control || m.Nudge || m.Role == "system" {
			continue // identidade do agente: nunca, nem em último recurso
		}
		if len(m.Content) > squeezeHead {
			m.Content = elideToolResult(m.Content, squeezeHead)
			if nb, ok := done("working-set"); ok {
				return nb, nil
			}
		}
		if len(m.ToolCalls) > 0 {
			tcs := make([]toolCall, len(m.ToolCalls))
			copy(tcs, m.ToolCalls)
			changed := false
			for j := range tcs {
				if len(tcs[j].Function.Arguments) > squeezeHead {
					tcs[j].Function.Arguments = elideArgsMarker(tcs[j].Function.Arguments)
					changed = true
				}
			}
			if changed {
				m.ToolCalls = tcs
				if nb, ok := done("working-set-args"); ok {
					return nb, nil
				}
			}
		}
	}

	return "", fmt.Errorf("%w: %d chars após elisão completa (orçamento %d) — system/protocolo maiores que o orçamento; compacte o histórico ou suba BIFROST_MAX_PROMPT",
		ErrPromptTooLarge, len(b), globalMax)
}

// serializeRaw serializa sem o orçamento global, aplicando os tetos
// sempre-ativos: por-resultado (tool), por-mensagem (texto de
// user/assistant), por-argumentos de chamada. System/Control/Nudge vão
// INTEIROS — são a identidade do agente e o protocolo de tools.
func serializeRaw(messages []Message, perResult int) string {
	var b strings.Builder
	toolNames := map[string]string{} // tool_call_id → nome da função
	for _, m := range messages {
		switch {
		case m.Role == "tool":
			label := "TOOL"
			if name := toolNames[m.ToolCallID]; name != "" {
				label = "TOOL " + name
			}
			content := m.Content
			if elided := elideToolResult(content, perResult); len(elided) < len(content) {
				slog.Debug("prompt: resultado de tool elidado", "tool", label, "orig", len(content), "kept", len(elided))
				content = elided
			}
			fmt.Fprintf(&b, "[%s]\n%s\n\n", label, content)
		case m.Content == "" && len(m.ToolCalls) == 0:
			// mensagem vazia: nada a serializar
		default:
			fmt.Fprintf(&b, "[%s]\n", strings.ToUpper(m.Role))
			if m.Content != "" {
				content := m.Content
				if m.Role != "system" && !m.Control && !m.Nudge {
					if elided := elideToolResult(content, msgContentMax); len(elided) < len(content) {
						content = elided
					}
				}
				fmt.Fprintf(&b, "%s\n", content)
			}
			for _, tc := range m.ToolCalls {
				toolNames[tc.ID] = tc.Function.Name
				args := tc.Function.Arguments
				if !json.Valid([]byte(args)) {
					args = "{}"
				}
				if len(args) > toolCallArgsMax {
					args = elideArgsMarker(args)
				}
				fmt.Fprintf(&b, "%stool_call\n{\"name\": %q, \"arguments\": %s}\n%s\n", fence, tc.Function.Name, args, fence)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// genPart é um bloco da última resposta: parágrafo/lista/título ("text") ou
// code block ("code" — rótulo da linguagem do header e código puro do pre).
type genPart struct {
	Kind string `json:"k"`
	Text string `json:"text"`
	Lang string `json:"lang"`
	Code string `json:"code"`
}

type genState struct {
	URL             string    `json:"url"`
	Responses       int       `json:"responses"`
	Parts           []genPart `json:"parts"`
	EditorText      string    `json:"editorText"`
	StopButton      bool      `json:"stopButton"`
	GenerationError bool      `json:"generationError"` // botão de retry ou erro de UI visível
}

// looksLikeToolCallJSON: conteúdo de code block com forma de chamada de
// ferramenta (JSON com "name" e "arguments"). O rótulo de linguagem NÃO
// serve para detectar chamadas — a UI do Gemini substitui rótulos
// desconhecidos por um genérico localizado ("Snippet de código").
func looksLikeToolCallJSON(code string) bool {
	c := strings.TrimSpace(code)
	if !strings.HasPrefix(c, "{") {
		return false
	}
	return strings.Contains(c, `"name"`) && strings.Contains(c, `"arguments"`)
}

// isToolCallPart: code block com forma de chamada de ferramenta — retido na
// emissão de streaming (a classificação final, com o nome declarado, é do
// parser do handler; a retenção é só pela forma, e é segura porque part
// só é emitida estável e completa).
func isToolCallPart(p genPart) bool {
	return p.Kind == "code" && looksLikeToolCallJSON(p.Code)
}

// assembleText monta o texto da resposta a partir das parts: parágrafos
// preservados, code blocks como fences de markdown (sem o rótulo da
// linguagem vazando). skipToolCalls=true omite os code blocks com forma de
// chamada de ferramenta — na emissão de streaming eles são protocolo
// (traduzidos para delta.tool_calls no fim), nunca conteúdo; o texto final
// os inclui para o parser classificar.
func assembleText(parts []genPart, skipToolCalls bool) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == "code" {
			if strings.TrimSpace(p.Lang) == "" && strings.TrimSpace(p.Code) == "" {
				continue
			}
			if skipToolCalls && isToolCallPart(p) {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			fmt.Fprintf(&b, "```%s\n%s\n```", strings.TrimSpace(p.Lang), strings.Trim(p.Code, "\n"))
			continue
		}
		t := strings.TrimSpace(p.Text)
		if t == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(t)
	}
	return b.String()
}

// Complete envia as mensagens ao Gemini e devolve o texto da última
// resposta do modelo. Uma chamada por vez (mutex). ctx controla o timeout
// de toda a operação — e, se morrer antes do fim (cliente desconectado),
// aborta a espera. model seleciona o modo da UI.
func (w *WebWorker) Complete(ctx context.Context, messages []Message, model string) (string, error) {
	return w.complete(ctx, messages, model, StreamHooks{})
}

// CompleteStream é o Complete com ganchos de streaming (StreamHooks): o
// worker emite conteúdo via OnDelta, chamadas de ferramenta fechadas no
// meio da geração via OnToolCall (classificadas por hooks.Classify) e
// dispara OnStart assim que o envio do prompt está confirmado.
func (w *WebWorker) CompleteStream(ctx context.Context, messages []Message, model string, hooks StreamHooks) (string, error) {
	return w.complete(ctx, messages, model, hooks)
}

func (w *WebWorker) complete(ctx context.Context, messages []Message, model string, hooks StreamHooks) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.ctx.Err() != nil {
		return "", ErrBrowserClosed
	}

	// As ações chromedp precisam derivar do contexto da aba (é onde mora a
	// sessão), mas respeitando o deadline do chamador — e a desconexão dele:
	// se o request morrer no meio, a espera é abortada na hora. Um ÚNICO
	// contexto com cancel e deadline: o cancel do watcher abaixo tem de
	// cancelar exatamente o contexto que os loops de espera usam.
	dl, hasDl := ctx.Deadline()
	if !hasDl {
		dl = time.Now().Add(10 * time.Minute) // chamador sem deadline: teto
	}
	runCtx, cancel := context.WithTimeout(w.ctx, time.Until(dl))
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-runCtx.Done():
		}
	}()

	// verificar sessão com teto curto: página engasgada (conversa/prompt
	// gigante) pendura o Evaluate no main thread DELA — sem teto, o request
	// só morria no deadline de 3 minutos do handler. Estourou → ladder de
	// destravamento: reload (mesma conversa) → página nova via /app (a
	// conversa gigante re-renderizada volta a engasgar; página nova a
	// abandona). Nenhum nível veio → o processo do Chromium não executa
	// comandos — o handler marca o browser para relanço.
	sessCtx, sessCancel := context.WithTimeout(runCtx, 30*time.Second)
	state, _, err := w.provider.State(sessCtx)
	sessDead := sessCtx.Err() != nil // deadline bateu (checado antes do cancel)
	sessCancel()
	if err != nil {
		if sessDead && runCtx.Err() == nil && w.ctx.Err() == nil {
			slog.Warn("página não responde ao probe de sessão em 30s; destravando", "err", err)
			if ok, _ := w.unwedge(runCtx); ok {
				// unwedge só devolve ok com probe logado confirmado
				state, err = stateLoggedIn, nil
			} else {
				return "", fmt.Errorf("%w: nem reload nem página nova destravaram a aba", ErrPageUnresponsive)
			}
		} else {
			return "", fmt.Errorf("verificar sessão: %w", err)
		}
	}
	if state != stateLoggedIn {
		return "", ErrGeminiNotLoggedIn
	}

	// --- conversa aderente ---------------------------------------------
	// O cliente reenvia o histórico completo a cada turno. Se ele é
	// continuação do que já está na conversa do Gemini (prefixo casa,
	// mesmo modelo, mesma contagem de respostas — ninguém digitou nada
	// lá no meio), só o delta é digitado: o histórico permanece na
	// conversa, cached do lado do Google, e o prompt de cada turno fica
	// minúsculo em vez de crescer linearmente com o loop do agente.
	base := historyBase(messages)
	controls := controlMsgs(messages)

	// proto: o protocolo de tools normalizado (sem a instrução multi-turn,
	// que entra e sai conforme o histórico) — identifica o toolset do
	// turno para decidir entre protocolo inteiro e lembrete compacto.
	proto := ""
	for _, m := range controls {
		if m.Control {
			proto = strings.Replace(m.Content, "\n"+multiTurnToolInstruction, "", 1)
		}
	}

	// ler estado com teto curto — mesma razão do probe de sessão: Evaluate
	// em página engasgada pendura; 30s falha rápido em vez de comer o
	// deadline do request.
	nowCtx, nowCancel := context.WithTimeout(runCtx, 30*time.Second)
	now, err := w.provider.GenState(nowCtx)
	nowCancel()
	if err != nil {
		return "", fmt.Errorf("ler estado da conversa: %w", err)
	}

	sticky := w.stickyOK && !w.dirty && w.lastModel == model &&
		now.Responses == w.lastResponses && now.Responses > 0 &&
		now.Responses <= maxStickyResponses &&
		prefixMatch(w.lastBase, base)

	// geração do turno anterior ainda correndo (cliente abortou o stream,
	// a UI continuou): espera assentar antes de digitar — enviar durante
	// a geração pode enfileirar ou perder a mensagem.
	if sticky && now.StopButton {
		if !w.waitForIdle(runCtx, 90*time.Second) {
			slog.Warn("conversa aderente: geração anterior não assentou; conversa nova")
			w.dirty = true
			sticky = false
		} else if st, serr := w.provider.GenState(runCtx); serr == nil {
			now = st
			sticky = sticky && now.Responses == w.lastResponses
		}
	}

	// devolveRespostaAtual: caminho de cache — a resposta pedida já está
	// na conversa (reenvio idêntico do cliente, ou delta que não sobrou
	// nada a dizer). Extrai a última resposta (esperando assentar se ainda
	// gera) e devolve sem enviar nada.
	devolveRespostaAtual := func() (string, bool) {
		cacheCtx, ccancel := context.WithTimeout(runCtx, 90*time.Second)
		defer ccancel()
		text, werr := w.waitResponse(cacheCtx, now.Responses-1)
		if werr != nil {
			slog.Warn("conversa aderente: extração da resposta atual falhou; conversa nova", "err", werr)
			w.dirty = true
			sticky = false
			if st, serr := w.provider.GenState(runCtx); serr == nil {
				now = st
			}
			return "", false
		}
		w.lastResponses = now.Responses
		slog.Info("conversa aderente: resposta já na conversa devolvida", "chars", len(text))
		return text, true
	}

	// reenvio idêntico sem correção → idempotência: a resposta já está lá.
	if sticky && len(base) == len(w.lastBase) && !hasNudge(controls) {
		if text, ok := devolveRespostaAtual(); ok {
			return text, nil
		}
	}

	var prompt string
	if sticky {
		// delta: o que o histórico ganhou desde o último turno, sem as
		// respostas de TEXTO do assistant (o modelo já as emitiu — reenviar
		// duplicaria o contexto), com os blocos de controle no fim (viés de
		// recência). Toolset idêntico ao do turno que abriu a conversa →
		// lembrete compacto no lugar do protocolo inteiro (com 28 tools,
		// ~60KB poupados por turno; o protocolo completo já está na conversa).
		delta := elideTextAssistants(base[len(w.lastBase):])
		send := make([]Message, 0, len(delta)+len(controls))
		send = append(send, delta...)
		compactProto := false
		for _, m := range controls {
			if m.Control && proto != "" && proto == w.lastProto {
				compactProto = true
				compact := stickyProtoReminder
				if strings.Contains(m.Content, multiTurnToolInstruction) {
					compact += "\n" + multiTurnToolInstruction
				}
				send = append(send, Message{Role: "system", Content: compact, Control: true})
				continue
			}
			send = append(send, m)
		}
		p, serr := SerializeMessages(send)
		if serr != nil {
			return "", serr
		}
		if strings.TrimSpace(p) != "" {
			prompt = p
			slog.Info("conversa aderente: reutilizando conversa",
				"delta_msgs", len(send), "chars", len(prompt), "base_msgs", len(base), "proto_compact", compactProto)
		}
	}

	// delta que não sobrou nada a dizer (só assistant no meio) → a resposta
	// atual do modelo já cobre: devolve.
	if sticky && prompt == "" {
		if text, ok := devolveRespostaAtual(); ok {
			return text, nil
		}
	}

	if prompt == "" {
		// conversa nova: cada requisição independente quando não há
		// continuação — e conversas cumpridas reabrem para não desacelerar.
		if now.Responses > 0 {
			if err := w.provider.NewChat(runCtx); err != nil {
				slog.Warn("conversa nova falhou; seguindo na atual", "err", err)
			} else {
				slog.Info("fresh conversation")
			}
		}
		var serr error
		prompt, serr = SerializeMessages(messages)
		if serr != nil {
			return "", serr
		}
	}

	// pré-envio (modo + limpeza de editor + observer + digitação + envio)
	// como sequência RE-EXECUTÁVEL com teto próprio de 120s — essas etapas
	// não podem consumir os 3 minutos do request. Página engasgada no meio
	// (a digitação de prompt pesado é o ponto clássico: o probe leve de
	// sessão PASSA, a interação pesada pendura): página NOVA via /app e
	// recomeço limpo — editor vazio, estado relido. O sentinela
	// errEditorResetPage avisa que a página foi TROCADA dentro da sequência
	// (editor preso) — o delta aderente perdeu o contexto e o histórico
	// completo precisa ser redigitado pelo chamador.
	preSubmit := func() (genState, chan streamChunk, bool, error) {
		pctx, pcancel := context.WithTimeout(runCtx, 120*time.Second)
		defer pcancel()
		fail := func(err error) (genState, chan streamChunk, bool, error) {
			return genState{}, nil, pctx.Err() != nil, err // wedged = o TETO bateu
		}
		if err := w.provider.EnsureMode(pctx, model); err != nil {
			return fail(err)
		}
		// estado pré-observer: SOBRA de texto no editor = submit anterior
		// falhou (ou alguém digitou na UI) — digitar em cima duplicaria o
		// prompt (bug visto em produção: retry digitava delta em cima do
		// delta não enviado). Reload ANTES da injeção do observer (a
		// navegação destrói o contexto JS); texto não enviado NÃO sobrevive
		// à navegação e a conversa (URL) preserva — o delta segue válido.
		before, err := w.provider.GenState(pctx)
		if err != nil {
			return fail(fmt.Errorf("ler estado da conversa: %w", err))
		}
		if strings.TrimSpace(before.EditorText) != "" {
			slog.Warn("editor com texto não enviado; recarregando aba antes de digitar", "chars", len(before.EditorText))
			cleared := false
			if w.reloadTab() == nil {
				if st, ok := w.waitConversationState(pctx, 12*time.Second); ok && strings.TrimSpace(st.EditorText) == "" {
					before, cleared = st, true
				}
			}
			if !cleared {
				// editor preso nem com reload: página NOVA abandona a
				// conversa — o chamador redigita o histórico COMPLETO e
				// re-executa esta sequência (observer etc. inclusos)
				if w.freshPage(runCtx) {
					if _, ok := w.waitConversationState(pctx, 12*time.Second); ok {
						return fail(errEditorResetPage)
					}
				}
				return fail(fmt.Errorf("%w: editor ocupado com texto não enviado (%d chars); nem reload nem página nova limparam", ErrPromptNotFound, len(before.EditorText)))
			}
			slog.Info("editor limpo após reload; seguindo")
		}
		// observer: injetado ANTES de digitar e depois de TODA navegação
		// (a navegação destrói o contexto JS da página); se falhar,
		// streamResponse cai no caminho de polling (ch nil)
		var ch chan streamChunk
		if hooks.OnDelta != nil {
			ch = make(chan streamChunk, 64)
			w.setStreamCh(ch)
			var discard string
			if err := chromedp.Run(pctx, chromedp.Evaluate(w.provider.ObserverJS(), &discard)); err != nil {
				slog.Warn("observer de streaming indisponível; fallback para polling", "err", err)
				ch = nil
			}
		}
		if err := w.typePrompt(pctx, prompt); err != nil {
			return fail(err)
		}
		if err := w.submit(pctx, before.Responses); err != nil {
			return fail(err)
		}
		return before, ch, false, nil
	}
	defer w.setStreamCh(nil) // limpa o último canal que a sequência instalar

	before, streamCh, wedged, err := preSubmit()
	if err != nil && runCtx.Err() == nil && w.ctx.Err() == nil {
		needFullPrompt := false
		if wedged {
			slog.Warn("pré-envio pendurou (página engasgada); abrindo página nova e recomeçando", "err", err)
			if w.freshPage(runCtx) {
				needFullPrompt = true
			} else {
				err = fmt.Errorf("%w: página nova não abriu", ErrPageUnresponsive)
			}
		} else if errors.Is(err, errEditorResetPage) {
			// preSubmit trocou a página para destravar o editor preso — o
			// delta aderente perdeu o contexto: histórico COMPLETO
			slog.Warn("página trocada para limpar o editor; redigitando histórico completo")
			needFullPrompt = true
		}
		if needFullPrompt {
			// Determinístico: a 1ª serialização já passou pelo orçamento,
			// esta não pode falhar.
			prompt, _ = SerializeMessages(messages)
			if b2, ch2, _, err2 := preSubmit(); err2 == nil {
				before, streamCh, err = b2, ch2, nil
			} else {
				// nem página nova segurou: devolve o erro da tentativa —
				// browser segue na rotação (volume ≠ processo morto)
				slog.Error("pré-envio falhou mesmo com página nova", "err", err2)
				err = err2
			}
		}
	}
	if err != nil && !wedged && runCtx.Err() == nil && w.ctx.Err() == nil {
		// falha de DOM no pré-envio (texto não entra no editor / não
		// envia): o editor ficou em estado ruim e o RETRY do cliente
		// re-digitaria no mesmo editor quebrado — loop de 502 idênticos
		// (visto em produção: 6+ retries de 1,6s falhando igual). Reload
		// best-effort limpa o editor e destrava o renderer para a próxima
		// tentativa.
		if errors.Is(err, ErrPromptNotFound) || errors.Is(err, ErrResponseNotFound) {
			slog.Warn("falha de DOM no pré-envio; recarregando aba para a próxima tentativa", "err", err)
			_ = w.reloadTab()
		}
	}
	if err != nil {
		return "", err
	}
	slog.Info("prompt submitted", "chars", len(prompt), "sticky", sticky)
	if len(prompt) > 120000 {
		slog.Warn("prompt gigante digitado — página pode engasgar; ajuste BIFROST_TOOL_RESULT_MAX/BIFROST_MAX_PROMPT", "chars", len(prompt))
	}

	// commit no envio: a conversa agora contém `base`; retratativas (nudge)
	// enxergam esse estado e emendam a correção na MESMA conversa — o modelo
	// vê a própria resposta ruim + a correção, sem re-enviar o histórico.
	w.stickyOK = true
	w.lastBase = base
	w.lastModel = model
	w.dirty = false
	w.lastResponses = before.Responses + 1 // provisório; confirmado ao fim
	if proto != "" {
		w.lastProto = proto
	}

	if hooks.OnStart != nil {
		if err := hooks.OnStart(); err != nil {
			return "", err
		}
	}

	var text string
	if hooks.OnDelta != nil {
		slog.Info("generation started (stream)")
		text, err = w.streamResponse(runCtx, before.Responses, streamCh, hooks)
	} else {
		slog.Info("generation started")
		text, err = w.waitResponse(runCtx, before.Responses)
	}
	if err != nil {
		// Estado da conversa após falha: se a resposta deste turno jamais
		// apareceu, o conteúdo é incerto → conversa nova na próxima. Se
		// apareceu (abort de streaming — o callback recusou — ou timeout de
		// estabilidade), a conversa segue válida para o próximo turno.
		probeCtx, pcancel := context.WithTimeout(w.ctx, 2*time.Second)
		if st, serr := w.provider.GenState(probeCtx); serr != nil || st.Responses <= before.Responses {
			w.dirty = true
		} else {
			w.lastResponses = st.Responses
		}
		pcancel()
		return "", err
	}
	if st, serr := w.provider.GenState(runCtx); serr == nil {
		w.lastResponses = st.Responses
	}
	slog.Info("generation finished", "chars", len(text), "sticky", sticky)
	return text, nil
}

// commonPrefixLen devolve o tamanho do maior prefixo comum de a e b.
func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// reloadTab recarrega a aba para destravar um renderer engasgado:
// Page.reload é comando de BROWSER — não precisa do main thread da página
// (que está ocupado há minutos com a conversa gigante) e substitui o
// processo do renderer. A URL (conversa atual) persiste; o estado aderente
// sobrevive porque a contagem de respostas não muda no re-render.
func (w *WebWorker) reloadTab() error {
	rctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	defer cancel()
	if err := chromedp.Run(rctx, chromedp.Reload()); err != nil {
		return err
	}
	return nil
}

// probeUntilLoggedIn sonda a página em loop (probes de 5s) até responder
// logada ou o teto esgotar. Página viva responde em <1s; engasgada não
// responde nunca — o teto é que diferencia.
func (w *WebWorker) probeUntilLoggedIn(runCtx context.Context, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) && runCtx.Err() == nil && w.ctx.Err() == nil {
		pctx, cancel := context.WithTimeout(runCtx, 5*time.Second)
		st, _, serr := w.provider.State(pctx)
		cancel()
		if serr == nil && st == stateLoggedIn {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// waitConversationState sonda o estado da conversa até a página responder
// (pós-reload/navegação a página re-carrega; Evaluate antes disso falha)
// ou o teto esgotar.
func (w *WebWorker) waitConversationState(ctx context.Context, wait time.Duration) (genState, bool) {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) && ctx.Err() == nil && w.ctx.Err() == nil {
		if st, err := w.provider.GenState(ctx); err == nil {
			return st, true
		}
		time.Sleep(400 * time.Millisecond)
	}
	return genState{}, false
}

// freshPage navega a aba para /app (conversa NOVA, página leve) e reseta o
// estado aderente. O reload NÃO cura conversa gigante: re-renderiza a
// mesma conversa e a página volta a engasgar na primeira interação pesada
// — a página nova abandona a conversa pesada de vez. O custo é o próximo
// turno redigitar o histórico completo (orçado pelo serialize). Devolve
// false se nem a navegação respondeu (processo do Chromium morto) ou a
// página nova não veio logada no teto.
func (w *WebWorker) freshPage(runCtx context.Context) bool {
	nctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	err := chromedp.Run(nctx, chromedp.Navigate(w.provider.HomeURL()))
	cancel()
	if err != nil {
		slog.Warn("navegação a página nova falhou — processo do Chromium não responde", "err", err)
		return false
	}
	w.stickyOK = false // conversa abandonada: próximo turno = histórico completo
	w.dirty = false
	if w.probeUntilLoggedIn(runCtx, 20*time.Second) {
		slog.Info("página nova aberta; conversa aderente descartada")
		return true
	}
	return false
}

// unwedge destrava a aba em níveis crescentes: (1) reload — mantém a
// conversa e o estado aderente; (2) página nova via /app — descarta a
// conversa gigante (o reload a re-renderiza e ela re-enge na hora).
// Devolve (recuperou, páginaNova). false = processo do Chromium não
// executa comandos — só relançar o browser resolve (marca no gateway).
func (w *WebWorker) unwedge(runCtx context.Context) (recovered, freshPage bool) {
	if err := w.reloadTab(); err != nil {
		slog.Warn("reload da aba falhou; indo direto a página nova", "err", err)
	} else if w.probeUntilLoggedIn(runCtx, 25*time.Second) {
		slog.Info("aba destravada com reload; conversa preservada")
		return true, false
	}
	if w.freshPage(runCtx) {
		return true, true
	}
	return false, false
}

// waitForIdle espera a geração em andamento terminar (botão "parar" some).
func (w *WebWorker) waitForIdle(ctx context.Context, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil || w.ctx.Err() != nil {
			return false
		}
		st, err := w.provider.GenState(ctx)
		if err == nil && !st.StopButton {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// typePrompt digita via Input.insertText — dispara os eventos de input que
// o editor do Gemini espera e é instantâneo mesmo para prompts longos.
// Após inserir, aguarda até 1,5s o Quill processar o texto (InsertText é
// assíncrono em relação à renderização do Angular).
func (w *WebWorker) typePrompt(ctx context.Context, prompt string) error {
	if err := chromedp.Run(ctx,
		chromedp.Click(w.provider.PromptSelector(), chromedp.NodeVisible, chromedp.ByQuery),
		chromedp.ActionFunc(func(c context.Context) error {
			return input.InsertText(prompt).Do(c)
		}),
	); err != nil {
		return fmt.Errorf("%w: %v", ErrPromptNotFound, err)
	}
	// Poll: o Quill pode demorar alguns frames para refletir o texto no DOM.
	// 5s (era 1,5s): página com conversa de ~150KB renderiza devagar — o
	// texto CHEGOU mas o DOM ainda não refletia, e o timeout cedo abortava
	// a digitação que tinha funcionado (loop de 502 em produção).
	deadline := time.Now().Add(5000 * time.Millisecond)
	for time.Now().Before(deadline) {
		st, err := w.provider.GenState(ctx)
		if err == nil && st.EditorText != "" {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%w: texto não apareceu no editor após 5s", ErrPromptNotFound)
}

// submit envia o que está no editor. Enter é o caminho primário (o Gemini
// envia com Enter; Shift+Enter faria nova linha). Se o editor não esvaziar
// — sinal de que nada foi enviado — clica no botão de envio.
func (w *WebWorker) submit(ctx context.Context, responsesBefore int) error {
	if err := chromedp.Run(ctx,
		chromedp.Click(w.provider.PromptSelector(), chromedp.NodeVisible, chromedp.ByQuery),
		chromedp.SendKeys(w.provider.PromptSelector(), "\r", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("enviar prompt (Enter): %w", err)
	}
	if w.confirmSubmitted(ctx, responsesBefore, 8*time.Second) {
		return nil
	}

	slog.Warn("Enter não enviou; tentando botão de envio")
	if err := chromedp.Run(ctx, chromedp.Click(w.provider.SendSelector(), chromedp.NodeVisible, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("%w: nem Enter nem botão de envio funcionaram (%v)", ErrResponseNotFound, err)
	}
	if w.confirmSubmitted(ctx, responsesBefore, 8*time.Second) {
		return nil
	}
	return fmt.Errorf("%w: prompt não foi enviado", ErrResponseNotFound)
}

// confirmSubmitted espera o editor esvaziar (o editor reseta após enviar),
// o botão "parar" aparecer (sinal de geração em andamento) ou uma resposta
// nova aparecer. Timeout: 8s (aumentado de 4s para tolerar Gemini lento).
//
// Race condition crítica: o Gemini limpa o editor ANTES de criar o elemento
// model-response — há uma janela de ~200–500ms onde nenhuma das condições
// simples é verdadeira. Por isso, ao detectar editor vazio, aguardamos
// 500ms extras antes de confirmar (evita falso negativo que causaria
// duplo-envio via clique no botão de envio).
func (w *WebWorker) confirmSubmitted(ctx context.Context, responsesBefore int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	var editorClearedAt time.Time
	for time.Now().Before(deadline) {
		st, err := w.provider.GenState(ctx)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// Resposta nova ou geração iniciou (stop button): confirmado.
		if st.Responses > responsesBefore || st.StopButton {
			return true
		}
		// Editor vazio: pode ser a janela de race. Aguarda 500ms extra
		// para o elemento de resposta aparecer antes de confirmar.
		if st.EditorText == "" {
			if editorClearedAt.IsZero() {
				editorClearedAt = time.Now()
			} else if time.Since(editorClearedAt) >= 500*time.Millisecond {
				return true
			}
		} else {
			// Editor voltou a ter texto? Reset (raro, mas protege).
			editorClearedAt = time.Time{}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// waitResponse aguarda a nova resposta terminar: texto presente, estável
// por alguns polls seguidos e sem botão "parar" visível. Polling curto
// (300ms) sobre estado real do DOM — nada de sleep fixo longo.
// Aborta imediatamente se a UI do Gemini exibir um erro de geração
// (botão "Tentar novamente" ou elemento de erro), evitando esperar 3min.
func (w *WebWorker) waitResponse(ctx context.Context, responsesBefore int) (string, error) {
	const stableNeeded = 3
	last := ""
	stable := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("%w: %v", ErrGenerationTimeout, err)
		}
		if w.ctx.Err() != nil {
			return "", ErrBrowserClosed
		}
		st, err := w.provider.GenState(ctx)
		if err == nil {
			// Erro de UI detectado: aborta imediatamente.
			if st.GenerationError {
				slog.Warn("erro de geração detectado na UI do Gemini")
				return "", ErrGenerationFailed
			}
			if st.Responses > responsesBefore {
				if text := assembleText(st.Parts, false); text != "" {
					if text == last {
						stable++
					} else {
						last = text
						stable = 0
					}
					if stable >= stableNeeded && !st.StopButton {
						return text, nil
					}
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// streamChunk é um empurrão do MutationObserver injetado na página: N =
// total de respostas na tela quando o chunk nasceu (só interessam os da
// resposta NOVA: N > responsesBefore), T = texto montado da última
// resposta com as regras do assembleText (code blocks "{" retidos —
// chamada de ferramenta em potencial nunca vaza como conteúdo).
type streamChunk struct {
	N int    `json:"n"`
	T string `json:"t"`
}

// setStreamCh instala/desinstala o canal da geração corrente (o listener
// CDP roda em outra goroutine e precisa de acesso sincronizado).
func (w *WebWorker) setStreamCh(ch chan streamChunk) {
	w.streamMu.Lock()
	w.streamCh = ch
	w.streamMu.Unlock()
}

// listenChunks registra, UMA vez por aba, o listener de eventos CDP: os
// console.log com o marcador __BIFROST__ (emitidos pelo observerJS) são
// decodificados e encaminhados ao canal da geração corrente. Payload
// truncado/corrompido é ignorado — o polling de estado cobre o resto.
func (w *WebWorker) listenChunks() {
	chromedp.ListenTarget(w.ctx, func(ev any) {
		ce, ok := ev.(*runtime.EventConsoleAPICalled)
		if !ok || len(ce.Args) < 2 {
			return
		}
		var marker string
		if len(ce.Args[0].Value) == 0 || json.Unmarshal(ce.Args[0].Value, &marker) != nil || marker != "__BIFROST__" {
			return
		}
		var payload string
		if json.Unmarshal(ce.Args[1].Value, &payload) != nil {
			return
		}
		var ck streamChunk
		if json.Unmarshal([]byte(payload), &ck) != nil {
			return
		}
		w.streamMu.Lock()
		ch := w.streamCh
		w.streamMu.Unlock()
		if ch == nil {
			return
		}
		select {
		case ch <- ck:
		default: // cheio: o polling cobre o que faltar
		}
	})
}

// streamResponse emite a resposta ENQUANTO ela nasce: o MutationObserver
// injetado (observerJS) empurra o texto montado da resposta nova a cada
// ~100ms via console → listener CDP → canal; cada crescimento vira delta
// imediato, sem esperar estabilidade — o primeiro token chega ao cliente
// assim que o primeiro parágrafo começa, não quando o segundo existe.
//
// O polling de 300ms permanece para: detecção de erro de UI, TÉRMINO
// (texto completo estável + botão parar ausente) e emissão final do que o
// observer reteve (code blocks "{": chamadas de ferramenta em potencial —
// o handler os traduz para delta.tool_calls, nunca conteúdo).
//
// Se o observer não entregar nada (injeção falhou, CDP mudo), o caminho
// antigo assume após a carência: emissão por parts ESTÁVEIS (part só sai
// quando estável há 3 polls e já tem irmã — o Gemini continua escrevendo
// um parágrafo depois de criar o elemento seguinte).
// streamResponse acompanha a geração e emete progressivamente: conteúdo
// via observer (ou fallback por parts estáveis) e CHAMADAS DE FERRAMENTA
// via hooks.OnToolCall assim que o bloco fecha e estabiliza no meio da
// geração — o cliente pode começar a executar a primeira chamada enquanto
// o resto da resposta ancora. Lookalikes (forma de chamada, nome não
// declarado) continuam retidos: chegam como delta tardio no fim, porque
// emitir conteúdo JSON mid-stream quebraria a invariante sentText ⊆
// finalNoTool do emendo final. O texto completo (com fences de chamada)
// segue sendo o retorno — a tradução final do handler é a fonte da verdade
// e deduplica contra o que já foi emitido.
func (w *WebWorker) streamResponse(ctx context.Context, responsesBefore int, ch chan streamChunk, hooks StreamHooks) (string, error) {
	const stableNeeded = 3
	const observerGrace = 2500 * time.Millisecond

	sentText := "" // texto já enviado ao cliente (base dos deltas)
	obsSeen := false
	start := time.Now()

	// estabilidade por part — mantida SEMPRE (não só no fallback): a
	// emissão precoce de chamadas depende dela mesmo com observer vivo
	var prev []genPart
	var stab []int
	emitted := 0 // fallback de texto: parts já emitidas como conteúdo
	callPtr := 0 // emissão de chamadas: próxima part a examinar

	lastFull := ""
	stableFull := 0

	emit := func(delta string) error {
		if delta == "" {
			return nil
		}
		if err := hooks.OnDelta(delta); err != nil {
			return fmt.Errorf("emissão abortada pelo callback: %w", err)
		}
		sentText += delta
		return nil
	}

	// emitStableCall classifica um code block fechado com as tools
	// declaradas (hooks.Classify) e o emite precocemente. Retorna erro só
	// se o callback abortar; lookalike (não classificado) é no-op.
	emitStableCall := func(code string) error {
		if hooks.OnToolCall == nil || hooks.Classify == nil {
			return nil
		}
		c, ok := hooks.Classify(code)
		if !ok {
			return nil
		}
		if err := hooks.OnToolCall(c); err != nil {
			return fmt.Errorf("emissão de chamada abortada pelo callback: %w", err)
		}
		slog.Info("tool call emitida precocemente (bloco estável)", "tool", c.Function.Name)
		return nil
	}

	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w: %v", ErrGenerationTimeout, ctx.Err())
		case <-w.ctx.Done():
			return "", ErrBrowserClosed
		case c := <-ch:
			obsSeen = true
			// só a resposta NOVA interessa (chunks da resposta anterior
			// re-renderizando são ruído)
			if c.N > responsesBefore && strings.HasPrefix(c.T, sentText) && len(c.T) > len(sentText) {
				if err := emit(c.T[len(sentText):]); err != nil {
					return "", err
				}
			}
		case <-timer.C:
			st, err := w.provider.GenState(ctx)
			if err == nil {
				// Erro de UI detectado: aborta imediatamente.
				if st.GenerationError {
					slog.Warn("erro de geração detectado na UI do Gemini (stream)")
					return "", ErrGenerationFailed
				}
				if st.Responses > responsesBefore {
					// fim: texto completo estável + botão parar ausente
					if full := assembleText(st.Parts, false); full != "" {
						if full == lastFull {
							stableFull++
						} else {
							lastFull = full
							stableFull = 0
						}
						if stableFull >= stableNeeded && !st.StopButton {
							if obsSeen {
								// emissão final: o que o observer reteu além do
								// enviado (blocos "{", cauda não transmitida)
								finalNoTool := assembleText(st.Parts, true)
								switch {
								case strings.HasPrefix(finalNoTool, sentText):
									if err := emit(finalNoTool[len(sentText):]); err != nil {
										return "", err
									}
								case sentText == "":
									if err := emit(finalNoTool); err != nil {
										return "", err
									}
								default:
									// re-parse da UI mudou texto já enviado: emenda
									// a partir do maior prefixo comum — o cliente
									// pode ver uma ementa no ponto da mudança, mas
									// nunca perde o conteúdo que veio depois dela
									n := commonPrefixLen(sentText, finalNoTool)
									slog.Warn("stream: texto final divergiu do emitido; emendando a partir da divergência",
										"sent", len(sentText), "final", len(finalNoTool), "divergence", n)
									if err := emit(finalNoTool[n:]); err != nil {
										return "", err
									}
								}
							} else if len(st.Parts) > emitted {
								// fallback: emite as parts restantes
								delta := assembleText(st.Parts[emitted:], true)
								if sentText != "" && delta != "" {
									delta = "\n\n" + delta
								}
								if err := emit(delta); err != nil {
									return "", err
								}
							}
							return full, nil
						}
					}

					// estabilidade por part: sempre atualizada (a emissão
					// precoce de chamadas usa estes contadores)
					cur := st.Parts
					nextStab := make([]int, len(cur))
					for i := range cur {
						if i < len(prev) && prev[i] == cur[i] {
							nextStab[i] = stab[i] + 1
						}
					}
					prev, stab = cur, nextStab

					// emissão precoce de chamadas: part com forma de chamada,
					// estável há 3 polls E com irmã depois dela (o bloco
					// fechou de verdade — a part viva nunca tem irmã
					// confirmada). Avança em ordem de documento para as
					// chamadas emitidas serem prefixo da tradução final;
					// para no primeiro part não estável.
					for callPtr < len(cur) && stab[callPtr] >= stableNeeded && callPtr+1 < len(cur) {
						if p := cur[callPtr]; isToolCallPart(p) {
							if err := emitStableCall(p.Code); err != nil {
								return "", err
							}
						}
						callPtr++
					}

					// fallback (observer mudo, após a carência): emissão por
					// parts estáveis — part só sai estável há 3 polls E com
					// irmã depois dela
					if !obsSeen && time.Since(start) > observerGrace {
						emitCount := 0
						for i := 0; i+1 < len(cur) && stab[i] >= stableNeeded; i++ {
							emitCount = i + 1
						}
						if emitCount > emitted {
							delta := assembleText(cur[emitted:emitCount], true)
							if sentText != "" && delta != "" {
								delta = "\n\n" + delta
							}
							if err := emit(delta); err != nil {
								return "", err
							}
							emitted = emitCount
						}
					}
				}
			}
			timer.Reset(300 * time.Millisecond)
		}
	}
}
