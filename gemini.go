package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
	// ErrPageUnresponsive: o renderer da página engasgou (conversa/prompt
	// gigante) e nem o reload destravou — o Evaluate não volta.
	ErrPageUnresponsive = errors.New("gemini page unresponsive (renderer wedged)")
	// ErrPromptTooLarge: mesmo após TODA a elisão (resultados, argumentos
	// de chamadas, texto antigo, último recurso no working set) o prompt
	// serializado não cabe no orçamento — enviar wedgaria a página; o
	// cliente precisa compactar o histórico (ou subir BIFROST_MAX_PROMPT).
	ErrPromptTooLarge = errors.New("prompt excede o orçamento mesmo após elisão completa")
	// errEditorResetPage: a aba foi trocada por página nova para destravar
	// o editor preso com texto não enviado — o delta aderente perdeu o
	// contexto; o chamador redigita o histórico completo.
	errEditorResetPage = errors.New("página trocada para limpar o editor")
)

// Gemini é o adaptador WebProvider do Gemini Web: só conhece o DOM
// (seletores, JS de estado/extração, menu de modos, conversa nova). O
// ciclo do request (mutex, aderência, streaming, ladder anti-wedge) mora
// no WebWorker (webworker.go).
type Gemini struct {
	ctx context.Context
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
	return NewWebWorker(ctx, &Gemini{ctx: ctx})
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

// NewGemini devolve o adaptador do Gemini (o motor completa em
// NewWebWorker — ver geminiFactory.NewWorker).
func NewGemini(ctx context.Context) *Gemini {
	return &Gemini{ctx: ctx}
}

// ---------------------------------------------------------------------------
// Conversa aderente: casamento de histórico e construção do delta.
// ---------------------------------------------------------------------------

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

// GenState lê o estado da conversa (gancho do motor — mesma forma genState
// de todos os provedores).
func (g *Gemini) GenState(ctx context.Context) (genState, error) {
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
		st, err := g.GenState(ctx)
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

// ---------------------------------------------------------------------------
// Ganchos do WebProvider (motor em webworker.go).
// ---------------------------------------------------------------------------

// State inspeciona o DOM e devolve o estado de autenticação (gancho do
// motor e do gateway).
func (g *Gemini) State(ctx context.Context) (pageState, string, error) {
	return GeminiState(ctx)
}

// HomeURL: página nova do Gemini (conversa limpa) — o fallback comprovado
// do newChat.
func (g *Gemini) HomeURL() string { return geminiSelectors.URL }

// PromptSelector: a caixa de prompt (Quill do Gemini).
func (g *Gemini) PromptSelector() string { return geminiSelectors.Prompt }

// SendSelector: o botão de envio (fallback do Enter).
func (g *Gemini) SendSelector() string { return geminiSelectors.Send }

// ObserverJS: o MutationObserver de streaming do Gemini.
func (g *Gemini) ObserverJS() string { return observerJS() }

// NewChat abre conversa nova no Gemini (gancho do motor).
func (g *Gemini) NewChat(ctx context.Context) error { return g.newChat(ctx) }

// EnsureMode seleciona o modo da UI pelo id do request (gancho do motor).
// Degrada com warn em vez de matar o request (resposta do modo errado é
// melhor que 500 travando o loop do agente).
func (g *Gemini) EnsureMode(ctx context.Context, model string) error {
	return g.ensureMode(ctx, modeByID(model))
}
