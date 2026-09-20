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
	"github.com/chromedp/cdproto/runtime"
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
			children: [...last.children].slice(0, 20).map(c => kid(c, 10))
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
	ErrGenerationFailed  = errors.New("gemini generation failed (error shown in UI)")
)

// maxStickyResponses: acima disso a conversa aderente reabre — conversas
// muito longas desaceleram a UI do Gemini e acumulam desvio de contexto;
// o histórico completo é reenviado numa conversa nova.
const maxStickyResponses = 30

// stickyProtoReminder substitui o protocolo completo de tools em turnos
// aderentes com o MESMO toolset: o protocolo inteiro já está no início da
// conversa; re-digitar 28 schemas (~60KB) por turno só desperdiça tempo.
const stickyProtoReminder = `LEMBRETE DE PROTOCOLO: as ferramentas declaradas no início desta conversa seguem valendo, com as mesmas regras e schemas. Chamada = um code block contendo APENAS o JSON {"name": ..., "arguments": {...}}; uma chamada por bloco. Resultados chegam como blocos [TOOL nome_da_ferramenta]. Nunca repita chamada cujo resultado já chegou; nunca exiba conteúdo de arquivo como texto — chame a ferramenta. Se nenhuma ferramenta cobre a tarefa, responda em markdown normal.`

// Gemini adapta a interface web: ctx é o contexto chromedp da aba (dono: o
// Browser) e mu garante uma interação por vez — duas prompts simultâneas na
// mesma aba seria caos.
type Gemini struct {
	ctx context.Context
	mu  sync.Mutex

	// Conversa aderente: o cliente OpenAI (agente) reenvia o histórico
	// completo a cada turno; quando ele é continuação do que já está na
	// conversa do Gemini (prefixo casa, mesmo modelo, mesma contagem de
	// respostas), só o delta é digitado — o histórico permanece na
	// conversa, cached do lado do Google. Retratativas (Nudge) emendam a
	// correção na MESMA conversa, enxergando a resposta ruim do modelo.
	stickyOK      bool
	lastBase      []Message // histórico (sem blocos de controle) presente na conversa
	lastModel     string    // modelo do último turno (mudou → conversa nova)
	lastResponses int       // contagem de respostas após o último turno
	lastProto     string    // protocolo de tools do turno que abriu a conversa (normalizado)
	dirty         bool      // último turno falhou com estado incerto: força conversa nova

	// Streaming: canal do MutationObserver (listener CDP → streamResponse).
	streamMu sync.Mutex
	streamCh chan streamChunk
}

type geminiFactory struct{}

func (f *geminiFactory) Name() string {
	return "gemini"
}

func (f *geminiFactory) Open(ctx context.Context) error {
	return OpenGemini(ctx)
}

func (f *geminiFactory) State(ctx context.Context) (pageState, string, error) {
	return GeminiState(ctx)
}

func (f *geminiFactory) NewWorker(ctx context.Context) LLMWorker {
	return NewGemini(ctx)
}

func (f *geminiFactory) Models() []modelObject {
	return []modelObject{
		{ID: "gemini-web", Object: "model", ContextLength: 1000000},
		{ID: "gemini-web-flash-lite", Object: "model", ContextLength: 1000000},
		{ID: "gemini-web-flash", Object: "model", ContextLength: 1000000},
		{ID: "gemini-web-pro", Object: "model", ContextLength: 1000000},
		{ID: "gemini-web-pro-extended", Object: "model", ContextLength: 1000000},
	}
}

func (f *geminiFactory) DefaultModel() string {
	return "gemini-web"
}

func NewGemini(ctx context.Context) *Gemini {
	g := &Gemini{ctx: ctx}
	g.listenChunks()
	return g
}

// ---------------------------------------------------------------------------
// Conversa aderente: casamento de histórico e construção do delta.
// ---------------------------------------------------------------------------

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

// SerializeMessages transforma o histórico OpenAI em um prompt textual — a
// UI web do Gemini não aceita histórico estruturado. Chamadas de ferramenta
// do assistente viram fences tool_call (o formato que o próprio modelo foi
// instruído a emitir) e resultados chegam como mensagens [TOOL nome].
func SerializeMessages(messages []Message) string {
	var b strings.Builder
	toolNames := map[string]string{} // tool_call_id → nome da função
	for _, m := range messages {
		switch {
		case m.Role == "tool":
			label := "TOOL"
			if name := toolNames[m.ToolCallID]; name != "" {
				label = "TOOL " + name
			}
			fmt.Fprintf(&b, "[%s]\n%s\n\n", label, m.Content)
		case m.Content == "" && len(m.ToolCalls) == 0:
			// mensagem vazia: nada a serializar
		default:
			fmt.Fprintf(&b, "[%s]\n", strings.ToUpper(m.Role))
			if m.Content != "" {
				fmt.Fprintf(&b, "%s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				toolNames[tc.ID] = tc.Function.Name
				args := tc.Function.Arguments
				if !json.Valid([]byte(args)) {
					args = "{}"
				}
				fmt.Fprintf(&b, "%stool_call\n{\"name\": %q, \"arguments\": %s}\n%s\n", fence, tc.Function.Name, args, fence)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// jsPartsFn é a fonte de uma função JS (sel) => parts[]: a extração
// ESTRUTURAL da última resposta (code blocks com rótulo/código puros do
// DOM, blocos de markdown como texto — nunca o innerText bruto do code
// block, cujo header inclui o rótulo da linguagem). Compartilhada pelo
// polling de estado (genStateJS) e pelo observer de streaming (observerJS).
//
// Fidelidade de escrita de código (bugs corrigidos contra o DOM real):
//   - table-block (a UI renderiza tabelas assim) virava nada — agora vira
//     tabela markdown com pipes, células escapadas
//   - listas aninhadas achatavam num nível — agora indentam 2 espaços/nível
//   - formatação inline sumia (code sem backticks, bold sem **): inlineMD
//     converte numa cópia do nó, sem tocar no DOM real
const jsPartsFn = `function(sel) {
	const responses = document.querySelectorAll(sel);
	const last = responses.length ? responses[responses.length - 1] : null;
	const parts = [];
	const BT = String.fromCharCode(96);
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
	// inlineMD: formatação inline vira marcador markdown numa CLONE do nó
	// (o DOM real não é tocado) — code → backticks, b/strong → **, em/i → *
	const inlineMD = el => {
		const clone = el.cloneNode(true);
		clone.querySelectorAll('code').forEach(c => {
			if (c.closest('code-block')) return;
			c.replaceWith(BT + (c.innerText || '') + BT);
		});
		clone.querySelectorAll('b, strong').forEach(c => c.replaceWith('**' + (c.innerText || '') + '**'));
		clone.querySelectorAll('em, i').forEach(c => c.replaceWith('*' + (c.innerText || '') + '*'));
		return (clone.innerText || '').trim();
	};
	// pushTable: table-block (ou table crua) vira tabela markdown — células
	// em linha única com pipes escapados, separador após o cabeçalho
	const pushTable = el => {
		const t = el.matches('table') ? el : el.querySelector('table');
		if (!t) return false;
		const cell = c => (c.innerText || '').replace(/\|/g, '\\|').replace(/\s+/g, ' ').trim();
		const rows = [...t.querySelectorAll('tr')].map(tr => [...tr.children].map(cell)).filter(r => r.length);
		if (!rows.length) return false;
		const nCols = Math.max(...rows.map(r => r.length));
		const norm = rows.map(r => { const out = r.slice(); while (out.length < nCols) out.push(''); return out; });
		const md = ['| ' + norm[0].join(' | ') + ' |', '| ' + Array(nCols).fill('---').join(' | ') + ' |']
			.concat(norm.slice(1).map(r => '| ' + r.join(' | ') + ' |')).join('\n');
		parts.push({k: 'text', text: md});
		return true;
	};
	// listMD: listas aninhadas com indentação de 2 espaços por nível
	const listMD = (el, depth) => {
		const lines = [];
		const ordered = el.tagName.toLowerCase() === 'ol';
		[...el.children].forEach((li, i) => {
			if (li.tagName.toLowerCase() !== 'li') return;
			const clone = li.cloneNode(true);
			clone.querySelectorAll('ul, ol').forEach(sub => sub.remove());
			const own = inlineMD(clone);
			if (own) lines.push('  '.repeat(depth) + (ordered ? (i + 1) + '. ' : '- ') + own);
			[...li.children].forEach(sub => {
				const st = sub.tagName.toLowerCase();
				if (st === 'ul' || st === 'ol') lines.push(...listMD(sub, depth + 1));
			});
		});
		return lines;
	};
	const blockTags = new Set(['p','h1','h2','h3','h4','h5','h6','ul','ol','blockquote','pre','hr']);
	const pushText = el => {
		const tag = el.tagName.toLowerCase();
		// só blocos de markdown contam como resposta; o resto (chips de
		// follow-up, rodapés, containers) é enfeite da UI, não conteúdo
		if (!blockTags.has(tag)) return;
		let t = '';
		if (tag === 'hr') t = '---';
		else if (/^h[1-6]$/.test(tag)) t = '#'.repeat(+tag[1]) + ' ' + inlineMD(el);
		else if (tag === 'ul' || tag === 'ol') t = listMD(el, 0).join('\n');
		else t = inlineMD(el);
		if (t) parts.push({k: 'text', text: t});
	};
	const isTableBlock = el => {
		const tag = el.tagName.toLowerCase();
		return tag === 'table-block' || tag === 'table';
	};
	// SKIP: enfeites da UI — botões, rodapés de resposta/ações, chips de
	// follow-up, barras de export. O texto deles não é conteúdo.
	const SKIP = el => {
		const tag = el.tagName.toLowerCase();
		if (tag === 'button' || tag === 'svg' || tag === 'mat-icon') return true;
		return /(footer|message-actions|feedback|follow-up|carousel|hide-on-print)/i
			.test((el.className || '').toString());
	};
	// walk: desce RECURSIVAMENTE por containers neutros (divs) até os blocos
	// reais — a UI embrulha tabelas em horizontal-scroll-wrapper e
	// table-block-component, e um walk de um nível só as perdia inteiras.
	const walk = el => {
		for (const block of el.children) {
			if (SKIP(block)) continue;
			const tag = block.tagName.toLowerCase();
			if (isCodeHost(block)) {
				if (!pushCode(block) && !pushTable(block)) pushText(block);
			} else if (isTableBlock(block)) {
				pushTable(block);
			} else if (blockTags.has(tag)) {
				pushText(block);
			} else if (tag === 'div') {
				walk(block);
			}
			// demais tags (span, custom de enfeite): fora da resposta
		}
	};
	if (last) walk(last);
	return parts;
}`

// genStateJS retrata a conversa: URL corrente, quantas respostas existem,
// as parts da última resposta (extraídas por ESTRUTURA), texto atual no
// editor, presença do botão "parar" (sinal de geração em andamento) e
// presença de erro de geração na UI (botão de retry visível ou mensagem
// de erro — permite abortar o loop de espera imediatamente em vez de
// aguardar o timeout de 3min).
func genStateJS() string {
	return fmt.Sprintf(`JSON.stringify((() => {
		const partsFn = %s;
		const parts = partsFn(%q);
		const editor = document.querySelector(%q);
		const stop = document.querySelector(%q);
		// Detecção de erro de geração: botão de retry ("Tentar novamente",
		// "Retry") ou elementos de mensagem de erro visíveis na UI.
		const errorEl = document.querySelector(
			'button[aria-label*="Tentar novamente" i], button[aria-label*="Retry" i], ' +
			'button[aria-label*="regenerate" i], ' +
			'[class*="error-message"], [class*="generation-error"], ' +
			'error-response, [data-test-id*="error"]'
		);
		return {
			url: location.href,
			responses: document.querySelectorAll(%q).length,
			parts: parts,
			editorText: editor ? editor.innerText.trim() : '',
			stopButton: !!stop,
			generationError: !!(errorEl && !stop),
		};
	})())`, jsPartsFn, geminiSelectors.Response, geminiSelectors.Prompt, geminiSelectors.StopButton, geminiSelectors.Response)
}

// observerJS injeta o MutationObserver de streaming: a cada mutação (com
// throttle de 100ms), monta o texto da última resposta com as MESMAS
// regras do assembleText Go (fences de markdown, \n\n entre parts) — exceto
// code blocks que começam com "{": podem ser chamadas de ferramenta em
// formação, e essas NUNCA vazam como conteúdo (valem delta.tool_calls no
// fim, não texto). O texto viaja por console.log com o marcador
// __BIFROST__; o listener CDP (listenChunks) o encaminha ao canal da
// geração corrente.
func observerJS() string {
	return fmt.Sprintf(`(() => {
		if (window.__bifrostObs) { try { window.__bifrostObs.disconnect(); } catch (e) {} }
		const partsFn = %s;
		const F = String.fromCharCode(96).repeat(3);
		const assemble = () => {
			const parts = partsFn(%q);
			let out = '';
			for (const p of parts) {
				if (p.k === 'code') {
					const lang = (p.lang || '').trim();
					const code = (p.code || '').replace(/^[\n]+|[\n]+$/g, '');
					if (!lang && !code.trim()) continue;
					if (code.trim().startsWith('{')) continue;
					if (out) out += '\n\n';
					out += F + lang + '\n' + code + '\n' + F;
				} else {
					const t = (p.text || '').trim();
					if (!t) continue;
					if (out) out += '\n\n';
					out += t;
				}
			}
			return out;
		};
		let last = 0, timer = null;
		const fire = () => {
			last = Date.now();
			timer = null;
			try {
				console.log('__BIFROST__', JSON.stringify({
					n: document.querySelectorAll(%q).length,
					t: assemble(),
				}));
			} catch (e) {}
		};
		window.__bifrostObs = new MutationObserver(() => {
			const now = Date.now();
			if (now - last >= 100) { fire(); return; }
			if (timer === null) timer = setTimeout(fire, 100 - (now - last));
		});
		window.__bifrostObs.observe(document.body, {childList: true, subtree: true, characterData: true});
		return '';
	})()`, jsPartsFn, geminiSelectors.Response, geminiSelectors.Response)
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
// aborta a espera. model seleciona o modo da UI.
func (g *Gemini) Complete(ctx context.Context, messages []Message, model string) (string, error) {
	return g.complete(ctx, messages, model, StreamHooks{})
}

// CompleteStream é o Complete com ganchos de streaming (StreamHooks): o
// worker emite conteúdo via OnDelta, chamadas de ferramenta fechadas no
// meio da geração via OnToolCall (classificadas por hooks.Classify) e
// dispara OnStart assim que o envio do prompt está confirmado.
func (g *Gemini) CompleteStream(ctx context.Context, messages []Message, model string, hooks StreamHooks) (string, error) {
	return g.complete(ctx, messages, model, hooks)
}

func (g *Gemini) complete(ctx context.Context, messages []Message, model string, hooks StreamHooks) (string, error) {
	mode := modeByID(model)
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

	now, err := g.generationState(runCtx)
	if err != nil {
		return "", fmt.Errorf("ler estado da conversa: %w", err)
	}

	sticky := g.stickyOK && !g.dirty && g.lastModel == model &&
		now.Responses == g.lastResponses && now.Responses > 0 &&
		now.Responses <= maxStickyResponses &&
		prefixMatch(g.lastBase, base)

	// geração do turno anterior ainda correndo (cliente abortou o stream,
	// a UI continuou): espera assentar antes de digitar — enviar durante
	// a geração pode enfileirar ou perder a mensagem.
	if sticky && now.StopButton {
		if !g.waitForIdle(runCtx, 90*time.Second) {
			slog.Warn("conversa aderente: geração anterior não assentou; conversa nova")
			g.dirty = true
			sticky = false
		} else if st, serr := g.generationState(runCtx); serr == nil {
			now = st
			sticky = sticky && now.Responses == g.lastResponses
		}
	}

	// devolveRespostaAtual: caminho de cache — a resposta pedida já está
	// na conversa (reenvio idêntico do cliente, ou delta que não sobrou
	// nada a dizer). Extrai a última resposta (esperando assentar se ainda
	// gera) e devolve sem enviar nada.
	devolveRespostaAtual := func() (string, bool) {
		cacheCtx, ccancel := context.WithTimeout(runCtx, 90*time.Second)
		defer ccancel()
		text, werr := g.waitResponse(cacheCtx, now.Responses-1)
		if werr != nil {
			slog.Warn("conversa aderente: extração da resposta atual falhou; conversa nova", "err", werr)
			g.dirty = true
			sticky = false
			if st, serr := g.generationState(runCtx); serr == nil {
				now = st
			}
			return "", false
		}
		g.lastResponses = now.Responses
		slog.Info("conversa aderente: resposta já na conversa devolvida", "chars", len(text))
		return text, true
	}

	// reenvio idêntico sem correção → idempotência: a resposta já está lá.
	if sticky && len(base) == len(g.lastBase) && !hasNudge(controls) {
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
		delta := elideTextAssistants(base[len(g.lastBase):])
		send := make([]Message, 0, len(delta)+len(controls))
		send = append(send, delta...)
		compactProto := false
		for _, m := range controls {
			if m.Control && proto != "" && proto == g.lastProto {
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
		if p := SerializeMessages(send); strings.TrimSpace(p) != "" {
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
			if err := g.newChat(runCtx); err != nil {
				slog.Warn("conversa nova falhou; seguindo na atual", "err", err)
			} else {
				slog.Info("fresh conversation")
			}
		}
		prompt = SerializeMessages(messages)
	}

	if err := g.ensureMode(runCtx, mode); err != nil {
		return "", err
	}

	// streaming: registra o canal do observer e o injeta ANTES de digitar —
	// a navegação (newChat) destrói o contexto JS da página, então a injeção
	// vem depois de todas as navegações. Se falhar, streamResponse cai no
	// caminho de polling (ch nil).
	var streamCh chan streamChunk
	if hooks.OnDelta != nil {
		streamCh = make(chan streamChunk, 64)
		g.setStreamCh(streamCh)
		defer g.setStreamCh(nil)
		var discard string
		if err := chromedp.Run(runCtx, chromedp.Evaluate(observerJS(), &discard)); err != nil {
			slog.Warn("observer de streaming indisponível; fallback para polling", "err", err)
			streamCh = nil
		}
	}

	before, err := g.generationState(runCtx)
	if err != nil {
		return "", fmt.Errorf("ler estado da conversa: %w", err)
	}

	if err := g.typePrompt(runCtx, prompt); err != nil {
		return "", err
	}
	if err := g.submit(runCtx, before.Responses); err != nil {
		return "", err
	}
	slog.Info("prompt submitted", "chars", len(prompt), "sticky", sticky)

	// commit no envio: a conversa agora contém `base`; retratativas (nudge)
	// enxergam esse estado e emendam a correção na MESMA conversa — o modelo
	// vê a própria resposta ruim + a correção, sem re-enviar o histórico.
	g.stickyOK = true
	g.lastBase = base
	g.lastModel = model
	g.dirty = false
	g.lastResponses = before.Responses + 1 // provisório; confirmado ao fim
	if proto != "" {
		g.lastProto = proto
	}

	if hooks.OnStart != nil {
		if err := hooks.OnStart(); err != nil {
			return "", err
		}
	}

	var text string
	if hooks.OnDelta != nil {
		slog.Info("generation started (stream)")
		text, err = g.streamResponse(runCtx, before.Responses, streamCh, hooks)
	} else {
		slog.Info("generation started")
		text, err = g.waitResponse(runCtx, before.Responses)
	}
	if err != nil {
		// Estado da conversa após falha: se a resposta deste turno jamais
		// apareceu, o conteúdo é incerto → conversa nova na próxima. Se
		// apareceu (abort de streaming — o callback recusou — ou timeout de
		// estabilidade), a conversa segue válida para o próximo turno.
		probeCtx, pcancel := context.WithTimeout(g.ctx, 2*time.Second)
		if st, serr := g.generationState(probeCtx); serr != nil || st.Responses <= before.Responses {
			g.dirty = true
		} else {
			g.lastResponses = st.Responses
		}
		pcancel()
		return "", err
	}
	if st, serr := g.generationState(runCtx); serr == nil {
		g.lastResponses = st.Responses
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

// waitForIdle espera a geração em andamento terminar (botão "parar" some).
func (g *Gemini) waitForIdle(ctx context.Context, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil || g.ctx.Err() != nil {
			return false
		}
		st, err := g.generationState(ctx)
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
func (g *Gemini) typePrompt(ctx context.Context, prompt string) error {
	if err := chromedp.Run(ctx,
		chromedp.Click(geminiSelectors.Prompt, chromedp.NodeVisible, chromedp.ByQuery),
		chromedp.ActionFunc(func(c context.Context) error {
			return input.InsertText(prompt).Do(c)
		}),
	); err != nil {
		return fmt.Errorf("%w: %v", ErrPromptNotFound, err)
	}
	// Poll: o Quill pode demorar alguns frames para refletir o texto no DOM.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		st, err := g.generationState(ctx)
		if err == nil && st.EditorText != "" {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%w: texto não apareceu no editor após 1,5s", ErrPromptNotFound)
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
	if g.confirmSubmitted(ctx, responsesBefore, 8*time.Second) {
		return nil
	}

	slog.Warn("Enter não enviou; tentando botão de envio")
	if err := chromedp.Run(ctx, chromedp.Click(geminiSelectors.Send, chromedp.NodeVisible, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("%w: nem Enter nem botão de envio funcionaram (%v)", ErrResponseNotFound, err)
	}
	if g.confirmSubmitted(ctx, responsesBefore, 8*time.Second) {
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
func (g *Gemini) confirmSubmitted(ctx context.Context, responsesBefore int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	var editorClearedAt time.Time
	for time.Now().Before(deadline) {
		st, err := g.generationState(ctx)
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
func (g *Gemini) setStreamCh(ch chan streamChunk) {
	g.streamMu.Lock()
	g.streamCh = ch
	g.streamMu.Unlock()
}

// listenChunks registra, UMA vez por aba, o listener de eventos CDP: os
// console.log com o marcador __BIFROST__ (emitidos pelo observerJS) são
// decodificados e encaminhados ao canal da geração corrente. Payload
// truncado/corrompido é ignorado — o polling de estado cobre o resto.
func (g *Gemini) listenChunks() {
	chromedp.ListenTarget(g.ctx, func(ev any) {
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
		g.streamMu.Lock()
		ch := g.streamCh
		g.streamMu.Unlock()
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
func (g *Gemini) streamResponse(ctx context.Context, responsesBefore int, ch chan streamChunk, hooks StreamHooks) (string, error) {
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
		case <-g.ctx.Done():
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
			st, err := g.generationState(ctx)
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

// ensureMode garante que o seletor de modo esteja no modo pedido (nil =
// não mexer). Troca só quando necessário: ler o aria-label do botão custa
// um evaluate. Tenta a sequência (abrir menu → aguardar itens → clicar)
// até 3 vezes. Em páginas de conversa longa o popover pode levar vários
// segundos para renderizar em headless — os polls são longos o bastante
// para isso. Ao falhar, DEGRADA em vez de matar o request: a resposta
// vem do modo atual (um erro 500 travaria o loop do agente inteiro; uma
// resposta do modelo errado, não).
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

	// menuItemsVisible: poll real até gem-menu-item estar no DOM. 6s: em
	// conversas longas a renderização do popover em headless é lenta — 2s
	// dava falso negativo ("menu não abriu") e matava o request.
	menuItemsVisible := func() bool {
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			var has string
			if err := chromedp.Run(ctx, chromedp.Evaluate(
				fmt.Sprintf(`document.querySelector(%q) ? '1' : ''`, geminiSelectors.ModeItem), &has,
			)); err == nil && has == "1" {
				return true
			}
			time.Sleep(150 * time.Millisecond)
		}
		return false
	}

	// trySwitch: uma tentativa completa (abrir menu → clicar item). O item
	// pode ser pai de submenu na UI nova — clica o filho homônimo aninhado
	// quando existir.
	trySwitch := func() (string, error) {
		var jsErr string
		if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => {
			const btn = document.querySelector(%q);
			if (!btn) return 'seletor de modo não encontrado';
			btn.click();
			return '';
		})()`, geminiSelectors.ModeSwitcher), &jsErr)); err != nil {
			return "", err
		}
		if jsErr != "" {
			return jsErr, nil
		}
		if !menuItemsVisible() {
			// menu não abriu — fecha qualquer overlay e tenta na próxima iteração
			_ = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => {
				const btn = document.querySelector(%q); if (btn) btn.click();
			})()`, geminiSelectors.ModeSwitcher), &jsErr))
			return "menu não abriu", nil
		}
		if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => {
			const want = %q;
			const items = [...document.querySelectorAll(%q)];
			const exact = items.find(el => (el.innerText || '').trim().startsWith(want));
			const el = exact || items.find(el => el.textContent.includes(want));
			if (!el) return 'modo não encontrado no menu: ' + want;
			// item desabilitado (limite de uso da conta, p.ex.): é
			// determinístico — inútil retentar; o motivo (com o reset do
			// limite) vai na mensagem
			if (el.hasAttribute('disabled') || el.getAttribute('aria-disabled') === 'true') {
				const detail = (el.innerText || '').split('\n').slice(1).join(' ').trim();
				return 'modo ' + want + ' indisponível' + (detail ? ': ' + detail : '');
			}
			const nested = [...el.querySelectorAll(%q)].find(c => c.textContent.includes(want));
			(nested || el).click();
			return '';
		})()`, mode.MenuItem, geminiSelectors.ModeItem, geminiSelectors.ModeItem), &jsErr)); err != nil {
			return "", err
		}
		return jsErr, nil
	}

	const maxAttempts = 3
	var lastJSErr string
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			slog.Warn("mode switch retry", "attempt", attempt+1, "reason", lastJSErr)
			time.Sleep(500 * time.Millisecond)
		}
		var err error
		lastJSErr, err = trySwitch()
		if err != nil {
			return fmt.Errorf("trocar modo: %w", err)
		}
		// desabilitado é determinístico (limite de uso): sem retentativa
		if strings.Contains(lastJSErr, "indisponível") {
			break
		}
		// confirma: o aria-label passa a terminar com o marcador do modo
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			if hasMode() {
				slog.Info("mode switched", "mode", mode.MenuItem, "attempt", attempt+1)
				return nil
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	// diagnóstico: o que o rótulo de fato diz?
	var curLabel string
	_ = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(() => {
		const btn = document.querySelector(%q);
		return btn ? (btn.getAttribute('aria-label') || '') : '';
	})()`, geminiSelectors.ModeSwitcher), &curLabel))
	// degradação graciosa: segue no modo atual — o request responde em vez
	// de falhar. O rótulo vai ao log (e ao painel via tee) para diagnóstico.
	slog.Warn("mode switch falhou; seguindo no modo atual",
		"want", mode.MenuItem, "attempts", maxAttempts, "label", curLabel, "last_err", lastJSErr)
	return nil
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
	} else if g.confirmFresh(ctx, 3*time.Second) {
		return nil
	} else {
		slog.Warn("botão de nova conversa não confirmou; navegando")
	}

	if err := chromedp.Run(ctx, chromedp.Navigate(geminiSelectors.URL)); err != nil {
		return fmt.Errorf("navegar para conversa nova: %w", err)
	}
	if g.confirmFresh(ctx, 8*time.Second) {
		return nil
	}
	return errors.New("conversa nova não confirmou")
}

// confirmFresh espera a conversa zerar (sem respostas) E o prompt box
// estar presente no DOM — o Quill pode demorar até 2s após a navegação
// para aparecer; typePrompt não pode rodar antes disso.
func (g *Gemini) confirmFresh(ctx context.Context, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		st, err := g.generationState(ctx)
		if err != nil {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if st.Responses > 0 {
			// ainda mostrando conversa anterior
			time.Sleep(250 * time.Millisecond)
			continue
		}
		// Responses == 0: confirma que o prompt box existe (Quill pronto).
		var hasPrompt string
		if err := chromedp.Run(ctx, chromedp.Evaluate(
			fmt.Sprintf(`document.querySelector(%q) ? '1' : ''`, geminiSelectors.Prompt), &hasPrompt,
		)); err == nil && hasPrompt == "1" {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}
