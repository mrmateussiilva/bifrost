package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// GeminiSelectors centraliza TODOS os seletores da UI do Gemini Web.
// Mudou o layout? Muda aqui e em nenhum outro lugar.
//
// Estado da verificação (dumps de 2026-09-04):
//   - URL, Prompt, SignIn e Account: confirmados no DOM real (logado e
//     deslogado)
//   - Send, Response, StopButton: provisórios — a UI de resposta só existe
//     com sessão e mensagem enviada; confirmar com `inspect`/`test`
type GeminiSelectors struct {
	URL        string
	Prompt     string
	Send       string
	Response   string
	StopButton string
	// SignIn: botão "Fazer login" — presente só quando deslogado. O
	// Gemini deslogado renderiza a UI completa (com caixa de prompt!),
	// então a existência do prompt NÃO prova sessão. Só BOTÕES contam:
	// o link da conta de um usuário logado também aponta para
	// accounts.google.com e não pode ser confundido com login.
	SignIn string
	// Account: link do avatar/conta — presente só quando logado. Usa o
	// caminho de URL "SignOutOptions", que não é localizado.
	Account string
	// ModeSwitcher: botão que abre o seletor de modo; o aria-label contém
	// o modo atual ("... No momento: X"). Confirmado no DOM.
	ModeSwitcher string
	// ModeItem: item do menu de modos; a opção certa é achada pelo texto
	// do produto ("3.5 Flash Lite" etc.), que não é localizado.
	ModeItem string
	// NewChat: botão de nova conversa. Fallback comprovado: navegar ao
	// /app sempre abre conversa nova (a atual não é retomada).
	NewChat string
}

var geminiSelectors = GeminiSelectors{
	URL:          "https://gemini.google.com/app",
	Prompt:       `rich-textarea .ql-editor[role="textbox"]`,
	Send:         `button[aria-label*="Enviar" i], button[aria-label*="Send" i], button[aria-label*="Envio" i]`,
	Response:     `model-response, message-content, .response-container-content`,
	StopButton:   `button[aria-label*="parar" i], button[aria-label*="stop" i]`,
	SignIn:       `button[aria-label*="login" i], button[aria-label*="sign in" i]`,
	Account:      `a[href*="SignOutOptions"]`,
	ModeSwitcher: `bard-mode-switcher button`,
	ModeItem:     `gem-menu-item`,
	NewChat:      `side-nav-sparkle-button button, button[aria-label*="nova conversa" i], button[aria-label*="new chat" i]`,
}

// OpenGemini navega a aba até o Gemini. Sem sessão o Google redireciona para
// accounts.google.com — a detecção fica por conta de GeminiState.
func OpenGemini(ctx context.Context) error {
	if err := chromedp.Run(ctx, chromedp.Navigate(geminiSelectors.URL)); err != nil {
		return fmt.Errorf("abrir gemini: %w", err)
	}
	slog.Info("gemini opened", "url", geminiSelectors.URL)
	return nil
}

type pageState int

const (
	stateUnknown pageState = iota
	stateLoggedIn
	stateLoginNeeded
)

func (s pageState) String() string {
	switch s {
	case stateLoggedIn:
		return "logged-in"
	case stateLoginNeeded:
		return "login-needed"
	}
	return "unknown"
}

// geminiStateJS lê apenas sinais estáveis: URL corrente, o campo de prompt
// (seletor centralizado), o botão de login e o link da conta.
func geminiStateJS() string {
	return fmt.Sprintf(`JSON.stringify((() => {
		const url = location.href;
		const promptBox = document.querySelector(%q);
		const signInButton = document.querySelector(%q);
		const account = document.querySelector(%q);
		return {
			url: url,
			promptBox: !!promptBox,
			signInButton: !!signInButton,
			hasAccount: !!account
		};
	})())`, geminiSelectors.Prompt, geminiSelectors.SignIn, geminiSelectors.Account)
}

// GeminiState classifica a aba: sessão ok, precisa de login ou indefinido
// (consentimento, onboarding etc.).
func GeminiState(ctx context.Context) (pageState, string, error) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(geminiStateJS(), &raw)); err != nil {
		return stateUnknown, "", err
	}
	var st struct {
		URL          string `json:"url"`
		PromptBox    bool   `json:"promptBox"`
		SignInButton bool   `json:"signInButton"`
		HasAccount   bool   `json:"hasAccount"`
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return stateUnknown, "", fmt.Errorf("decodificar estado da página: %w", err)
	}
	switch {
	case strings.Contains(st.URL, "accounts.google.com") || st.SignInButton:
		return stateLoginNeeded, st.URL, nil
	case st.HasAccount:
		return stateLoggedIn, st.URL, nil
	case st.PromptBox && strings.Contains(st.URL, "gemini.google.com"):
		return stateLoggedIn, st.URL, nil
	}
	return stateUnknown, st.URL, nil
}

// inspectJS despeja candidatos a seletores: caixas de texto, botões com
// aria-label, custom elements e as últimas respostas do modelo.
const inspectJS = `JSON.stringify((() => {
	const norm = s => (s || '').replace(/\s+/g, ' ').trim().slice(0, 80);
	const textboxes = [];
	document.querySelectorAll('[role="textbox"]').forEach(el => {
		textboxes.push({tag: el.tagName.toLowerCase(), aria: el.getAttribute('aria-label'), cls: norm(el.className.toString())});
	});
	const buttons = [];
	document.querySelectorAll('button').forEach(el => {
		const aria = el.getAttribute('aria-label');
		if (aria) buttons.push({aria: norm(aria), disabled: el.disabled, cls: norm(el.className.toString())});
	});
	const customTags = [...new Set(
		[...document.querySelectorAll('*')]
			.map(el => el.tagName.toLowerCase())
			.filter(t => t.includes('-'))
	)];
	const responses = [];
	document.querySelectorAll('model-response, message-content, [class*="response-container"]').forEach(el => {
		responses.push({tag: el.tagName.toLowerCase(), cls: norm(el.className.toString()), text: norm(el.textContent)});
	});
	return {
		url: location.href,
		title: document.title,
		webdriver: navigator.webdriver,
		brands: (navigator.userAgentData && navigator.userAgentData.brands) || null,
		textboxes: textboxes,
		buttons: buttons.slice(0, 80),
		customTags: customTags.slice(0, 100),
		responses: responses.slice(-8),
	};
})())`

// InspectGemini devolve um retrato JSON do DOM atual do Gemini, para
// descobrir e confirmar seletores. Requer sessão logada para ver o chat.
func InspectGemini(ctx context.Context) (string, error) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(inspectJS, &raw)); err != nil {
		return "", fmt.Errorf("inspecionar dom: %w", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return raw, nil
	}
	return buf.String(), nil
}

// responseStructureJS desenha o esqueleto da última resposta do modelo:
// filhos diretos e descendentes (tag, classe, início do texto) — o mapa
// para desenhar a extração estruturada (code blocks, markdown).
func responseStructureJS() string {
	return fmt.Sprintf(`JSON.stringify((() => {
		const responses = document.querySelectorAll(%q);
		const last = responses.length ? responses[responses.length - 1] : null;
		if (!last) return {count: 0};
		const kid = (el, d) => ({
			tag: el.tagName.toLowerCase(),
			cls: (el.className || '').toString().replace(/\s+/g, ' ').trim().slice(0, 70),
			text: (el.innerText || '').replace(/\s+/g, ' ').trim().slice(0, 100),
			pre: el.tagName.toLowerCase() === 'code-block' ? (el.querySelector('pre') || {innerText: ''}).innerText.replace(/\s+/g, ' ').trim().slice(0, 60) : undefined,
			kids: d > 0 ? [...el.children].slice(0, 8).map(c => kid(c, d - 1)) : null
		});
		return {
			count: responses.length,
			last: {tag: last.tagName.toLowerCase(), cls: (last.className || '').toString().slice(0, 70)},
			children: [...last.children].slice(0, 20).map(c => kid(c, 6))
		};
	})())`, geminiSelectors.Response)
}

// ResponseStructure despeja o esqueleto da última resposta em JSON —
// diagnóstico para confirmar os seletores de extração (rodar com uma
// resposta ainda na tela, p.ex. logo após `bifrost test`).
func ResponseStructure(ctx context.Context) (string, error) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(responseStructureJS(), &raw)); err != nil {
		return "", fmt.Errorf("inspecionar resposta: %w", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return raw, nil
	}
	return buf.String(), nil
}

// ---------------------------------------------------------------------------
// Interação com o Gemini: uma requisição por vez, sempre na mesma aba.
// ---------------------------------------------------------------------------

// Erros claros para as situações que o Bifrost sabe identificar.
var (
	ErrGeminiNotLoggedIn = errors.New("gemini session is not authenticated")
	ErrPromptNotFound    = errors.New("gemini prompt box not found")
	ErrGenerationTimeout = errors.New("gemini generation timed out")
	ErrResponseNotFound  = errors.New("gemini response not found")
	ErrBrowserClosed     = errors.New("browser closed")
)

// Gemini adapta a interface web: ctx é o contexto chromedp da aba (dono: o
// Browser) e mu garante uma interação por vez — duas prompts simultâneas na
// mesma aba seria caos.
type Gemini struct {
	ctx context.Context
	mu  sync.Mutex
}

func NewGemini(ctx context.Context) *Gemini {
	return &Gemini{ctx: ctx}
}

// SerializeMessages transforma o histórico OpenAI em um prompt textual —
// a UI web do Gemini não aceita histórico estruturado.
func SerializeMessages(messages []Message) string {
	var b strings.Builder
	for _, m := range messages {
		if m.Content == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s]\n%s\n\n", strings.ToUpper(m.Role), m.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// genStateJS retrata a conversa: quantas respostas existem, as parts da
// última resposta (extraídas por ESTRUTURA — nunca o innerText bruto do
// code block, que inclui o rótulo da linguagem no header), texto atual no
// editor e presença do botão "parar" (sinal de geração em andamento).
func genStateJS() string {
	return fmt.Sprintf(`JSON.stringify((() => {
		const responses = document.querySelectorAll(%q);
		const last = responses.length ? responses[responses.length - 1] : null;
		const editor = document.querySelector(%q);
		const stop = document.querySelector(%q);
		const parts = [];
		const isCodeHost = el => {
			const tag = el.tagName.toLowerCase();
			return tag === 'code-block' || tag === 'response-element';
		};
		const pushCode = el => {
			const cb = el.matches('code-block') ? el : el.querySelector('code-block');
			if (!cb) return false;
			const lang = (cb.querySelector('.header-formatted') || {innerText: ''}).innerText.trim();
			const code = (cb.querySelector('pre') || {innerText: ''}).innerText;
			if (lang || code.trim()) parts.push({k: 'code', lang: lang, code: code});
			return true;
		};
		const blockTags = new Set(['p','h1','h2','h3','h4','h5','h6','ul','ol','table','blockquote','pre']);
		const pushText = el => {
			const tag = el.tagName.toLowerCase();
			// só blocos de markdown contam como resposta; o resto (chips de
			// follow-up, rodapés, containers) é enfeite da UI, não conteúdo
			if (!blockTags.has(tag)) return;
			let t = '';
			if (/^h[1-6]$/.test(tag)) t = '#'.repeat(+tag[1]) + ' ' + el.innerText.trim();
			else if (tag === 'ul' || tag === 'ol') {
				t = [...el.children].map((li, i) =>
					(tag === 'ol' ? (i + 1) + '. ' : '- ') + li.innerText.trim()).join('\n');
			} else t = el.innerText.trim();
			if (t) parts.push({k: 'text', text: t});
		};
		if (last) for (const child of last.children) {
			if (isCodeHost(child)) {
				if (!pushCode(child)) pushText(child);
				continue;
			}
			// o texto mora dentro dos contêineres .markdown; os demais
			// filhos diretos (rodapés, botões) não fazem parte da resposta
			if ((child.className || '').toString().includes('markdown')) {
				for (const block of child.children) {
					if (isCodeHost(block)) {
						if (!pushCode(block)) pushText(block);
					} else {
						pushText(block);
					}
				}
			}
		}
		return {
			responses: responses.length,
			parts: parts,
			editorText: editor ? editor.innerText.trim() : '',
			stopButton: !!stop
		};
	})())`, geminiSelectors.Response, geminiSelectors.Prompt, geminiSelectors.StopButton)
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
	Responses  int       `json:"responses"`
	Parts      []genPart `json:"parts"`
	EditorText string    `json:"editorText"`
	StopButton bool      `json:"stopButton"`
}

// assembleText monta o texto da resposta a partir das parts: parágrafos
// preservados, code blocks como fences de markdown (sem o rótulo da
// linguagem vazando). Todos os fences fecham — quem chama decide quais
// parts entram (no streaming, só as completas).
func assembleText(parts []genPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == "code" {
			if strings.TrimSpace(p.Lang) == "" && strings.TrimSpace(p.Code) == "" {
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

func (g *Gemini) generationState(ctx context.Context) (genState, error) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(genStateJS(), &raw)); err != nil {
		return genState{}, err
	}
	var st genState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return genState{}, fmt.Errorf("decodificar estado da conversa: %w", err)
	}
	return st, nil
}

// geminiMode descreve um modo da UI do Gemini. Os dois textos NÃO
// coincidem: o item do menu diz "3.5 Flash Lite", mas o rótulo do botão
// quando ativo diz "Gemini Flash-Lite" (tabela do comando `bifrost modes`).
// MenuItem serve para clicar; Label para confirmar — comparado com
// endsWith, porque "Gemini Flash" é prefixo (nunca sufixo) de
// "Gemini Flash-Lite".
type geminiMode struct {
	ID       string // id exposto na API
	MenuItem string // texto do item no menu (para clicar)
	Label    string // rótulo ativo do botão (para confirmar)
}

// Registro de modos — manter sincronizado com a saída de `bifrost modes`.
var geminiModes = []*geminiMode{
	{ID: "gemini-web-flash-lite", MenuItem: "3.5 Flash Lite", Label: "Gemini Flash-Lite"},
	{ID: "gemini-web-flash", MenuItem: "3.8 Flash", Label: "Gemini Flash"},
	{ID: "gemini-web-pro", MenuItem: "3.1 Pro", Label: "Gemini Pro"},
	{ID: "gemini-web-pro-extended", MenuItem: "Raciocínio complexo", Label: "Pro Estendido"},
}

func modeByID(id string) *geminiMode {
	for _, m := range geminiModes {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// Complete envia as mensagens ao Gemini e devolve o texto da última
// resposta do modelo. Uma chamada por vez (mutex). ctx controla o timeout
// de toda a operação — e, se morrer antes do fim (cliente desconectado),
// aborta a espera. mode seleciona o modo da UI; nil mantém o modo
// atual da conversa.
func (g *Gemini) Complete(ctx context.Context, messages []Message, mode *geminiMode) (string, error) {
	return g.complete(ctx, messages, mode, nil, nil)
}

// CompleteStream é o Complete com ganchos de streaming: onStart roda assim
// que o envio do prompt está confirmado — o momento certo de escrever os
// cabeçalhos SSE, porque erros anteriores (sessão, DOM, envio) ainda podem
// virar status HTTP de verdade; onDelta recebe cada acréscimo de texto
// enquanto a geração corre.
func (g *Gemini) CompleteStream(ctx context.Context, messages []Message, mode *geminiMode, onStart func() error, onDelta func(string)) (string, error) {
	return g.complete(ctx, messages, mode, onStart, onDelta)
}

func (g *Gemini) complete(ctx context.Context, messages []Message, mode *geminiMode, onStart func() error, onDelta func(string)) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.ctx.Err() != nil {
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
	runCtx, cancel := context.WithTimeout(g.ctx, time.Until(dl))
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-runCtx.Done():
		}
	}()

	state, _, err := GeminiState(runCtx)
	if err != nil {
		return "", fmt.Errorf("verificar sessão: %w", err)
	}
	if state != stateLoggedIn {
		return "", ErrGeminiNotLoggedIn
	}

	// Conversa nova quando a atual já tem respostas: cada requisição é
	// independente — o cliente OpenAI reenvia o histórico completo, e
	// acumular tudo na mesma conversa do Gemini duplicaria o contexto e
	// desaceleraria as gerações.
	if st, err := g.generationState(runCtx); err == nil && st.Responses > 0 {
		if err := g.newChat(runCtx); err != nil {
			slog.Warn("conversa nova falhou; seguindo na atual", "err", err)
		} else {
			slog.Info("fresh conversation")
		}
	}

	if err := g.ensureMode(runCtx, mode); err != nil {
		return "", err
	}

	before, err := g.generationState(runCtx)
	if err != nil {
		return "", fmt.Errorf("ler estado da conversa: %w", err)
	}

	prompt := SerializeMessages(messages)
	if err := g.typePrompt(runCtx, prompt); err != nil {
		return "", err
	}
	if err := g.submit(runCtx, before.Responses); err != nil {
		return "", err
	}
	slog.Info("prompt submitted", "chars", len(prompt))

	if onStart != nil {
		if err := onStart(); err != nil {
			return "", err
		}
	}

	if onDelta != nil {
		slog.Info("generation started (stream)")
		text, err := g.streamResponse(runCtx, before.Responses, onDelta)
		if err != nil {
			return "", err
		}
		slog.Info("generation finished (stream)", "chars", len(text))
		return text, nil
	}

	slog.Info("generation started")
	text, err := g.waitResponse(runCtx, before.Responses)
	if err != nil {
		return "", err
	}
	slog.Info("generation finished")

	slog.Info("response extracted", "chars", len(text))
	return text, nil
}

// typePrompt digita via Input.insertText — dispara os eventos de input que
// o editor do Gemini espera e é instantâneo mesmo para prompts longos.
func (g *Gemini) typePrompt(ctx context.Context, prompt string) error {
	if err := chromedp.Run(ctx,
		chromedp.Click(geminiSelectors.Prompt, chromedp.NodeVisible, chromedp.ByQuery),
		chromedp.ActionFunc(func(c context.Context) error {
			return input.InsertText(prompt).Do(c)
		}),
	); err != nil {
		return fmt.Errorf("%w: %v", ErrPromptNotFound, err)
	}
	st, err := g.generationState(ctx)
	if err != nil {
		return fmt.Errorf("conferir editor: %w", err)
	}
	if st.EditorText == "" {
		return fmt.Errorf("%w: texto não apareceu no editor", ErrPromptNotFound)
	}
	return nil
}

// submit envia o que está no editor. Enter é o caminho primário (o Gemini
// envia com Enter; Shift+Enter faria nova linha). Se o editor não esvaziar
// — sinal de que nada foi enviado — clica no botão de envio.
func (g *Gemini) submit(ctx context.Context, responsesBefore int) error {
	if err := chromedp.Run(ctx,
		chromedp.Click(geminiSelectors.Prompt, chromedp.NodeVisible, chromedp.ByQuery),
		chromedp.SendKeys(geminiSelectors.Prompt, "\r", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("enviar prompt (Enter): %w", err)
	}
	if g.confirmSubmitted(ctx, responsesBefore, 4*time.Second) {
		return nil
	}

	slog.Warn("Enter não enviou; tentando botão de envio")
	if err := chromedp.Run(ctx, chromedp.Click(geminiSelectors.Send, chromedp.NodeVisible, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("%w: nem Enter nem botão de envio funcionaram (%v)", ErrResponseNotFound, err)
	}
	if g.confirmSubmitted(ctx, responsesBefore, 4*time.Second) {
		return nil
	}
	return fmt.Errorf("%w: prompt não foi enviado", ErrResponseNotFound)
}

// confirmSubmitted espera o editor esvaziar (o editor reseta após enviar)
// ou uma resposta nova aparecer.
func (g *Gemini) confirmSubmitted(ctx context.Context, responsesBefore int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		st, err := g.generationState(ctx)
		if err == nil && (st.EditorText == "" || st.Responses > responsesBefore) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// waitResponse aguarda a nova resposta terminar: texto presente, estável
// por alguns polls seguidos e sem botão "parar" visível. Polling curto
// (300ms) sobre estado real do DOM — nada de sleep fixo longo.
func (g *Gemini) waitResponse(ctx context.Context, responsesBefore int) (string, error) {
	const stableNeeded = 3
	last := ""
	stable := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("%w: %v", ErrGenerationTimeout, err)
		}
		if g.ctx.Err() != nil {
			return "", ErrBrowserClosed
		}
		st, err := g.generationState(ctx)
		if err == nil && st.Responses > responsesBefore {
			if text := assembleText(st.Parts); text != "" {
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
		time.Sleep(300 * time.Millisecond)
	}
}

// streamResponse é o waitResponse do modo streaming, com emissão por
// parts ESTÁVEIS: uma part só vai ao cliente quando existe há
// stableNeeded polls sem mudar E já tem irmã depois dela — o Gemini
// continua escrevendo um parágrafo depois de já criar o elemento
// seguinte, então "ter irmã" sozinho não prova nada. A última part sai
// apenas no fim, com o texto final. Part já emitida que muda (re-parse
// raro) deixa o cliente com a versão anterior — o stream segue; truncar
// a resposta seria pior.
func (g *Gemini) streamResponse(ctx context.Context, responsesBefore int, onDelta func(string)) (string, error) {
	const stableNeeded = 3
	var prev []genPart  // parts do poll anterior
	var stab []int      // polls consecutivos sem mudar, por índice
	emitted := 0        // quantas parts já foram ao cliente
	sentText := ""      // texto acumulado enviado (com as junções \n\n)
	lastFull := ""      // texto completo do poll anterior
	stableFull := 0     // polls consecutivos com o texto completo igual
	morphWarned := false
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("%w: %v", ErrGenerationTimeout, err)
		}
		if g.ctx.Err() != nil {
			return "", ErrBrowserClosed
		}
		st, err := g.generationState(ctx)
		if err == nil && st.Responses > responsesBefore {
			// estabilidade por part: contador por índice que zera quando a
			// part muda (cresce, é re-parseada, some)
			cur := st.Parts
			nextStab := make([]int, len(cur))
			for i := range cur {
				if i < len(prev) && prev[i] == cur[i] {
					nextStab[i] = stab[i] + 1
				}
			}
			prev, stab = cur, nextStab

			// prefixo emitível: parts não-últimas estáveis há stableNeeded
			emitCount := 0
			for i := 0; i+1 < len(cur) && stab[i] >= stableNeeded; i++ {
				emitCount = i + 1
			}
			if emitCount > emitted {
				delta := assembleText(cur[emitted:emitCount])
				if sentText != "" && delta != "" {
					delta = "\n\n" + delta
				}
				if delta != "" {
					onDelta(delta)
					sentText += delta
				}
				emitted = emitCount
			} else {
				// part já emitida que voltou a mudar: cliente mantém a
				// versão anterior; emissão retoma quando estabilizar
				morphed := false
				for i := 0; i < emitted && i < len(stab); i++ {
					if stab[i] < stableNeeded {
						morphed = true
						break
					}
				}
				if morphed && !morphWarned {
					morphWarned = true
					slog.Warn("stream: part já emitida mudou; cliente mantém a versão anterior")
				}
			}

			// fim: texto completo estável + botão parar ausente
			if full := assembleText(st.Parts); full != "" {
				if full == lastFull {
					stableFull++
				} else {
					lastFull = full
					stableFull = 0
				}
				if stableFull >= stableNeeded && !st.StopButton {
					if len(st.Parts) > emitted {
						delta := assembleText(st.Parts[emitted:])
						if sentText != "" && delta != "" {
							delta = "\n\n" + delta
						}
						if delta != "" {
							onDelta(delta)
						}
					}
					return full, nil
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// ensureMode garante que o seletor de modo esteja no modo pedido (nil =
// não mexer). Troca só quando necessário: ler o aria-label do botão custa
// um evaluate.
func (g *Gemini) ensureMode(ctx context.Context, mode *geminiMode) error {
	if mode == nil {
		return nil
	}

	checkJS := fmt.Sprintf(`JSON.stringify((() => {
		const btn = document.querySelector(%q);
		return {has: !!(btn && (btn.getAttribute('aria-label') || '').endsWith(%q))};
	})())`, geminiSelectors.ModeSwitcher, mode.Label)

	hasMode := func() bool {
		var raw string
		if err := chromedp.Run(ctx, chromedp.Evaluate(checkJS, &raw)); err != nil {
			return false
		}
		var st struct {
			Has bool `json:"has"`
		}
		return json.Unmarshal([]byte(raw), &st) == nil && st.Has
	}

	if hasMode() {
		return nil // já está no modo pedido
	}

	slog.Info("switching mode", "to", mode.MenuItem)
	var jsErr string
	err := chromedp.Run(ctx,
		// abre o menu de modos
		chromedp.Evaluate(fmt.Sprintf(`(() => {
			const btn = document.querySelector(%q);
			if (!btn) return 'seletor de modo não encontrado';
			btn.click();
			return '';
		})()`, geminiSelectors.ModeSwitcher), &jsErr),
		chromedp.Sleep(400*time.Millisecond),
		// clica o item cujo texto contém o modo pedido
		chromedp.Evaluate(fmt.Sprintf(`(() => {
			const items = [...document.querySelectorAll(%q)];
			const el = items.find(el => el.textContent.includes(%q));
			if (!el) return 'modo não encontrado no menu: %s';
			el.click();
			return '';
		})()`, geminiSelectors.ModeItem, mode.MenuItem, mode.MenuItem), &jsErr),
	)
	if err != nil {
		return fmt.Errorf("trocar modo: %w", err)
	}
	if jsErr != "" {
		return fmt.Errorf("trocar modo: %s", jsErr)
	}

	// confirma: o aria-label do botão passa a terminar com o marcador do modo
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hasMode() {
			slog.Info("mode switched", "mode", mode.MenuItem)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	// diagnóstico: o que o rótulo de fato diz?
	var curLabel string
	_ = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => {
		const btn = document.querySelector(%q);
		return btn ? (btn.getAttribute('aria-label') || '') : '';
	})()`, geminiSelectors.ModeSwitcher), &curLabel))
	return fmt.Errorf("troca para o modo %q não confirmou (rótulo atual: %q)", mode.MenuItem, curLabel)
}

// newChat zera a conversa. Botão primeiro (sem reload); navegar ao /app é
// o fallback comprovado — sempre abre conversa nova.
func (g *Gemini) newChat(ctx context.Context) error {
	var jsErr string
	if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => {
		const btn = document.querySelector(%q);
		if (!btn) return 'botão de nova conversa não encontrado';
		btn.click();
		return '';
	})()`, geminiSelectors.NewChat), &jsErr)); err != nil || jsErr != "" {
		slog.Warn("botão de nova conversa indisponível; navegando", "err", err, "js", jsErr)
	} else if g.confirmFresh(ctx, 2*time.Second) {
		return nil
	} else {
		slog.Warn("botão de nova conversa não confirmou; navegando")
	}

	if err := chromedp.Run(ctx, chromedp.Navigate(geminiSelectors.URL)); err != nil {
		return fmt.Errorf("navegar para conversa nova: %w", err)
	}
	if g.confirmFresh(ctx, 5*time.Second) {
		return nil
	}
	return errors.New("conversa nova não confirmou")
}

// confirmFresh espera a conversa zerar (sem respostas).
func (g *Gemini) confirmFresh(ctx context.Context, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		st, err := g.generationState(ctx)
		if err == nil && st.Responses == 0 {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}
