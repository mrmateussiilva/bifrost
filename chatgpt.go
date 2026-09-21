package main

// Driver do ChatGPT Web (chatgpt.com) — adaptador WebProvider do motor
// genérico (webworker.go). O ciclo inteiro do request (sessão, aderência,
// pré-envio, streaming, ladder anti-wedge) vem do motor; aqui só vive o
// que é DOM do ChatGPT.
//
// ESTADO DOS SELETORES (2026-09-20): PROVISÓRIOS — mapeados contra o DOM
// conhecido do chatgpt.com, NÃO ainda validados contra a página real
// (login necessário). O caminho de calibração é o mesmo do Gemini:
// 1. `bifrost login` com BIFROST_PROVIDER=chatgpt (Chrome headed, login
//    manual FORA do CDP — mesmas regras do Gemini; o Cloudflare challenge,
//    se aparecer, se resolve nessa janela);
// 2. `bifrost test "olá"` valida ponta a ponta (state → digitação →
//    envio → extração);
// 3. conferir genState/partsFn contra o DOM real e ajustar
//    chatgptSelectors/chatgptPartsFn.
// Rótulos de idioma de code block ainda não são extraídos (lang="") — a
// detecção de tool call é por CONTEÚDO, não por rótulo (regra do Gemini:
// rótulo não sobrevive ao DOM), então function calling funciona mesmo assim.

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

// ChatGPTSelectors centraliza os seletores da UI do ChatGPT Web. Mudou o
// layout? Muda aqui e em nenhum outro lugar.
type ChatGPTSelectors struct {
	URL        string
	Prompt     string // caixa de prompt (ProseMirror contenteditable)
	Send       string // botão de envio (fallback do Enter)
	Stop       string // botão parar (geração em andamento)
	Response   string // mensagens do assistente (a ÚLTIMA é a corrente)
	NewChat    string // botão de conversa nova
	Account    string // botão de conta — presente só quando logado
	Login      string // botão/link de login — presente só quando deslogado
	ModelBtn   string // botão do seletor de modelo
	ModeItem   string // itens do menu de modelos
	ModelLabel string // contêiner do rótulo do modelo ativo no botão
}

var chatgptSelectors = ChatGPTSelectors{
	URL:      "https://chatgpt.com/",
	Prompt:   `#prompt-textarea`,
	Send:     `button[data-testid="send-button"]`,
	Stop:     `button[data-testid="stop-button"]`,
	Response: `[data-message-author-role="assistant"]`,
	NewChat:  `button[data-testid="create-new-chat-button"]`,
	// Account: botão de conta (logado). CONFIRMADO contra o DOM real
	// (inspect 2026-09-21): `accounts-profile-button` — o histórico
	// "accounts-control-button" das versões antigas fica como fallback.
	Account: `[data-testid="accounts-profile-button"], [data-testid="accounts-control-button"]`,
	// Login: botão/link de login (deslogado).
	Login:      `[data-testid="login-button"], a[href*="/auth/login"], button[data-testid*="ogin"], button[data-testid*="Log in"]`,
	ModelBtn:   `button[data-testid="model-switcher-dropdown-button"]`,
	ModeItem:   `[role="menuitem"]`,
	ModelLabel: `button[data-testid="model-switcher-dropdown-button"]`,
}

func OpenChatGPT(ctx context.Context) error {
	// ERR_ABORTED: o chatgpt.com REDIRECIONA (auth/login ou desafio
	// Cloudflare) e o redirect aborta a navegação original — o chromedp
	// reporta erro com a página aterrissando noutro lugar. Tolerante: warn
	// e segue — o State lê a página que estiver na tela.
	if err := chromedp.Run(ctx, chromedp.Navigate(chatgptSelectors.URL)); err != nil {
		if strings.Contains(err.Error(), "net::ERR_ABORTED") {
			slog.Warn("chatgpt: navegação abortada por redirect (auth/challenge); seguindo com a página que aterrissou", "err", err)
			return nil
		}
		return err
	}
	return nil
}

// chatgptStateJS: logado = botão de conta (avatar, canto inferior); o
// ChatGPT deslogado mostra botão/link de login (a caixa de prompt existe
// deslogado — não prova nada, mesma regra do Gemini). Nem conta nem login
// = DESCONHECIDO (desafio Cloudflare, página carregando, seletor errado) —
// distinto de login_needed para o fluxo de login continuar sondando e o
// `inspect` revelar o que há na tela.
func chatgptStateJS() string {
	return fmt.Sprintf(`(() => {
		const url = location.href;
		const account = document.querySelector(%q);
		const login = document.querySelector(%q);
		const prompt = document.querySelector(%q);
		let state = 'unknown';
		if (account) state = 'logged_in';
		else if (login || url.indexOf('/auth') !== -1) state = 'login_needed';
		return JSON.stringify({state, url, prompt: !!prompt});
	})()`, chatgptSelectors.Account, chatgptSelectors.Login, chatgptSelectors.Prompt)
}

func ChatGPTState(ctx context.Context) (pageState, string, error) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(chatgptStateJS(), &raw)); err != nil {
		return stateUnknown, "", err
	}
	var st struct {
		State  string `json:"state"`
		URL    string `json:"url"`
		Prompt bool   `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return stateUnknown, "", fmt.Errorf("decodificar estado do chatgpt: %w", err)
	}
	switch st.State {
	case "logged_in":
		return stateLoggedIn, st.URL, nil
	case "login_needed":
		return stateLoginNeeded, st.URL, nil
	default:
		return stateUnknown, st.URL, nil
	}
}

// chatgptPartsFn é a fonte da função JS (sel) => parts[]: a extração
// ESTRUTURAL da última mensagem do assistente — code blocks (código do
// <code>, sem o header de copiar), blocos de markdown como texto.
// Compartilhada pelo polling de estado e pelo observer de streaming.
// PROVISÓRIO: rótulo de linguagem não extraído (lang vazio); tabelas viram
// innerText (sem pipes markdown) — calibrar contra o DOM real.
const chatgptPartsFn = `function(sel) {
	const msgs = document.querySelectorAll(sel);
	const last = msgs.length ? msgs[msgs.length - 1] : null;
	const parts = [];
	const pushText = el => { const t = (el.innerText || '').trim(); if (t) parts.push({k: 'text', text: t}); };
	const pushCode = el => {
		const pre = el.matches('pre') ? el : el.querySelector('pre');
		if (!pre) return false;
		const code = (pre.querySelector('code') || pre).innerText;
		if (code.trim()) parts.push({k: 'code', lang: '', code: code});
		return true;
	};
	const walk = el => {
		for (const block of el.children) {
			const tag = block.tagName.toLowerCase();
			if (tag === 'pre' || block.querySelector('pre')) {
				if (pushCode(block)) continue;
			}
			if (['p','h1','h2','h3','h4','h5','h6','ul','ol','table','blockquote'].includes(tag)) {
				pushText(block);
				continue;
			}
			if (block.children.length) walk(block);
			else pushText(block);
		}
	};
	if (last) walk(last);
	return parts;
}`

// chatgptGenStateJS lê o estado da conversa na forma genState — o contrato
// do motor. Erro de UI do ChatGPT (banner de erro) não é detectado ainda
// (generationError sempre false) — calibrar depois do login real.
func chatgptGenStateJS() string {
	return fmt.Sprintf(`(() => {
		const url = location.href;
		const editor = document.querySelector(%q);
		const editorText = editor ? (editor.innerText || '') : '';
		const responses = document.querySelectorAll(%q).length;
		const stop = !!document.querySelector(%q);
		const partsFn = %s;
		const parts = partsFn(%q);
		return JSON.stringify({
			url: url,
			responses: responses,
			parts: parts,
			editorText: editorText,
			stopButton: stop,
			generationError: false
		});
	})()`, chatgptSelectors.Prompt, chatgptSelectors.Response, chatgptSelectors.Stop, chatgptPartsFn, chatgptSelectors.Response)
}

// chatgptObserverJS: MutationObserver com o protocolo __BIFROST__ (mesma
// assinatura do Gemini: {n: total de respostas, t: texto montado}). Code
// blocks começando com "{" são retidos — chamada de ferramenta em potencial
// nunca vaza como conteúdo no stream.
func chatgptObserverJS() string {
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
	})()`, chatgptPartsFn, chatgptSelectors.Response, chatgptSelectors.Response)
}

// chatgptMode descreve um modelo do seletor do ChatGPT. MenuItem é o texto
// do item no menu (para clicar/confirmar). PROVISÓRIO: rótulos reais do
// menu precisam de calibração contra a UI logada (equivalente do
// `bifrost modes` do Gemini).
type chatgptMode struct {
	ID       string
	MenuItem string
	Deadline time.Duration // teto de geração próprio (modelos "thinking" são lentos)
}

var chatgptModes = []*chatgptMode{
	{ID: "chatgpt-web", MenuItem: ""}, // não mexe no modelo atual
	{ID: "chatgpt-web-4o", MenuItem: "GPT-4o"},
	{ID: "chatgpt-web-4o-mini", MenuItem: "4o mini"},
}

func chatgptModeByID(id string) *chatgptMode {
	for _, m := range chatgptModes {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// ChatGPT é o adaptador WebProvider do ChatGPT Web.
type ChatGPT struct {
	ctx context.Context
}

type chatGPTFactory struct{}

func (f *chatGPTFactory) Name() string {
	return "chatgpt"
}

func (f *chatGPTFactory) Open(ctx context.Context) error {
	return OpenChatGPT(ctx)
}

func (f *chatGPTFactory) State(ctx context.Context) (pageState, string, error) {
	return ChatGPTState(ctx)
}

func (f *chatGPTFactory) NewWorker(ctx context.Context) LLMWorker {
	return NewWebWorker(ctx, &ChatGPT{ctx: ctx})
}

func (f *chatGPTFactory) Models() []modelObject {
	out := make([]modelObject, 0, len(chatgptModes))
	for _, m := range chatgptModes {
		out = append(out, modelObject{ID: m.ID, Object: "model", ContextLength: 128000})
	}
	return out
}

func (f *chatGPTFactory) DefaultModel() string {
	return "chatgpt-web"
}

// ---------------------------------------------------------------------------
// Ganchos do WebProvider (motor em webworker.go).
// ---------------------------------------------------------------------------

func (c *ChatGPT) State(ctx context.Context) (pageState, string, error) {
	return ChatGPTState(ctx)
}

func (c *ChatGPT) HomeURL() string        { return chatgptSelectors.URL }
func (c *ChatGPT) PromptSelector() string { return chatgptSelectors.Prompt }
func (c *ChatGPT) SendSelector() string   { return chatgptSelectors.Send }
func (c *ChatGPT) ObserverJS() string     { return chatgptObserverJS() }

func (c *ChatGPT) GenState(ctx context.Context) (genState, error) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(chatgptGenStateJS(), &raw)); err != nil {
		return genState{}, err
	}
	var st genState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return genState{}, fmt.Errorf("decodificar estado da conversa: %w", err)
	}
	return st, nil
}

func (c *ChatGPT) NewChat(ctx context.Context) error {
	// botão de conversa nova (sidebar); fallback comprovado: navegar à home
	// (mesma estratégia do newChat do Gemini — /app sempre abre conversa nova)
	clickCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := chromedp.Run(clickCtx, chromedp.Click(chatgptSelectors.NewChat, chromedp.NodeVisible, chromedp.ByQuery))
	if err == nil && c.confirmFresh(ctx, 8*time.Second) {
		slog.Info("chatgpt: fresh conversation (botão)")
		return nil
	}
	slog.Warn("chatgpt: botão de conversa nova falhou; navegando à home", "err", err)
	if err := chromedp.Run(ctx, chromedp.Navigate(chatgptSelectors.URL)); err != nil {
		return err
	}
	if c.confirmFresh(ctx, 8*time.Second) {
		slog.Info("chatgpt: fresh conversation (home)")
		return nil
	}
	return errors.New("chatgpt: conversa nova não confirmou")
}

// confirmFresh: prompt presente e vazio, nenhuma mensagem do assistente.
func (c *ChatGPT) confirmFresh(ctx context.Context, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil || c.ctx.Err() != nil {
			return false
		}
		st, err := c.GenState(ctx)
		if err == nil && st.Responses == 0 && strings.TrimSpace(st.EditorText) == "" {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// EnsureMode seleciona o modelo no dropdown do ChatGPT. Degrada com warn
// em vez de matar o request (mesma política do Gemini): resposta do modelo
// errado é melhor que 500 travando o loop do agente — e os rótulos são
// provisórios até a calibração.
func (c *ChatGPT) EnsureMode(ctx context.Context, model string) error {
	mode := chatgptModeByID(model)
	if mode == nil || mode.MenuItem == "" {
		return nil // chatgpt-web / desconhecido: não mexe
	}

	const attempts = 3
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if ctx.Err() != nil || c.ctx.Err() != nil {
			return lastErr
		}
		// já está no modelo pedido? (rótulo do botão contém o item)
		var cur string
		if err := chromedp.Run(ctx, chromedp.Evaluate(
			fmt.Sprintf(`document.querySelector(%q) ? document.querySelector(%q).innerText : ''`,
				chatgptSelectors.ModelLabel, chatgptSelectors.ModelLabel), &cur)); err == nil {
			if strings.Contains(strings.TrimSpace(cur), mode.MenuItem) {
				return nil
			}
		}
		if err := c.clickModelItem(ctx, mode.MenuItem); err != nil {
			lastErr = err
			slog.Warn("chatgpt: troca de modelo falhou (tentativa)", "attempt", attempt, "model", mode.MenuItem, "err", err)
			continue
		}
		// confirmação: rótulo do botão contém o item
		var after string
		if err := chromedp.Run(ctx, chromedp.Evaluate(
			fmt.Sprintf(`document.querySelector(%q) ? document.querySelector(%q).innerText : ''`,
				chatgptSelectors.ModelLabel, chatgptSelectors.ModelLabel), &after)); err == nil &&
			strings.Contains(strings.TrimSpace(after), mode.MenuItem) {
			slog.Info("chatgpt: mode switched", "mode", mode.MenuItem, "attempt", attempt)
			return nil
		}
		lastErr = fmt.Errorf("rótulo do seletor não confirmou %q", mode.MenuItem)
	}
	slog.Warn("chatgpt: DEGRADAÇÃO — troca de modelo não confirmou; seguindo no modelo atual",
		"model", mode.MenuItem, "err", lastErr)
	return nil
}

// clickModelItem abre o dropdown do seletor e clica o item cujo texto
// contém o rótulo pedido.
func (c *ChatGPT) clickModelItem(ctx context.Context, item string) error {
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := chromedp.Run(openCtx,
		chromedp.Click(chatgptSelectors.ModelBtn, chromedp.NodeVisible, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("abrir seletor de modelo: %w", err)
	}
	// aguarda o menu renderizar (dropdown do ChatGPT é rápido; poll curto)
	menuCtx, mcancel := context.WithTimeout(ctx, 5*time.Second)
	defer mcancel()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if menuCtx.Err() != nil {
			break
		}
		var found string
		js := fmt.Sprintf(`(() => {
			const items = [...document.querySelectorAll(%q)];
			const el = items.find(i => (i.innerText || '').includes(%q));
			if (el) { el.click(); return el.innerText; }
			return '';
		})()`, chatgptSelectors.ModeItem, item)
		if err := chromedp.Run(menuCtx, chromedp.Evaluate(js, &found)); err != nil {
			return err
		}
		if strings.TrimSpace(found) != "" {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("item de modelo não encontrado no menu: %q", item)
}

// chatgptInspectJS despeja o essencial do DOM para calibração de
// seletores: URL/título, presença dos seletores candidatos, inventário de
// data-testid da página, botões com rótulo e o começo do corpo — o que há
// na tela (app logado, página de login, desafio Cloudflare) fica óbvio.
const chatgptInspectJS = `JSON.stringify((() => {
	const norm = s => (s || '').replace(/\s+/g, ' ').trim().slice(0, 80);
	const testids = [...new Set(
		[...document.querySelectorAll('[data-testid]')].map(e => e.getAttribute('data-testid'))
	)].slice(0, 150);
	const buttons = [...document.querySelectorAll('button')].slice(0, 50).map(b => ({
		testid: b.getAttribute('data-testid'),
		aria: b.getAttribute('aria-label'),
		text: norm(b.innerText),
	})).filter(b => b.testid || b.aria || b.text);
	const sels = {
		prompt: !!document.querySelector('#prompt-textarea'),
		send: !!document.querySelector('button[data-testid="send-button"]'),
		stop: !!document.querySelector('button[data-testid="stop-button"]'),
		assistants: document.querySelectorAll('[data-message-author-role="assistant"]').length,
		account: !!document.querySelector('[data-testid="accounts-profile-button"], [data-testid="accounts-control-button"]'),
		login: !!document.querySelector('[data-testid="login-button"], a[href*="/auth/login"], button[data-testid*="ogin"]'),
		newchat: !!document.querySelector('button[data-testid="create-new-chat-button"]'),
		modelbtn: !!document.querySelector('button[data-testid="model-switcher-dropdown-button"]'),
		menuitems: document.querySelectorAll('[role="menuitem"]').length,
	};
	return {
		url: location.href,
		title: document.title,
		selectors: sels,
		testids: testids,
		buttons: buttons,
		bodyHead: norm(document.body.innerText).slice(0, 600),
	};
})())`

// InspectChatGPT despeja o DOM do chatgpt.com em JSON (calibração de
// seletores / diagnóstico de página: app, login ou desafio Cloudflare).
func InspectChatGPT(ctx context.Context) (string, error) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(chatgptInspectJS, &raw)); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return "", err
	}
	return buf.String(), nil
}
