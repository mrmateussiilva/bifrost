package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/go-chi/chi/v5"
)

const usageText = `Bifrost: gateway OpenAI-compatible sobre a interface web do Gemini.

Uso:
  bifrost login     abre o Chromium (profile persistente) no Gemini e aguarda
                    login manual na primeira execução; nas seguintes, confirma
                    que a sessão persistiu
  bifrost inspect   despeja o DOM do Gemini em JSON (com sessão logada), para
                    confirmar os seletores
  bifrost modes     troca item por item do menu de modos e lê o rótulo ativo
                    de cada um (mantém o registro de modelos em dia)
  bifrost test "prompt"  envia um prompt ao Gemini e imprime a resposta no
                    terminal (prova do caminho completo)
  bifrost serve     API HTTP: GET /health, GET /v1/models e
                     POST /v1/chat/completions — com e sem streaming (default
                     quando sem argumento)

Env:
  BIFROST_ADDR      endereço HTTP (default ":8080")
  BIFROST_PROFILE   diretório do profile do Chromium (default "./data/chrome-profile")
  BIFROST_HEADLESS  rodar Chromium sem janela (default "false")
  BIFROST_CHROME    caminho do binário do Chromium, se não estiver no PATH
  BIFROST_API_KEY   se definida, exige Authorization: Bearer (default: sem auth)
  BIFROST_LOG       nível de log: debug|info|warn|error (default "info")
  BIFROST_MODEL     modelo default quando o request omite "model" (default "gemini-web")
  BIFROST_PASSWORD_STORE  keystore do Chrome: "gnome-libsecret" (default, desktop)
                    ou "basic" (container, sem gnome-keyring) — tem de ser o
                    mesmo em todo acesso ao profile
  BIFROST_NO_SANDBOX  true adiciona --no-sandbox (necessário em container Docker)
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	flag.Parse()

	cfg := LoadConfig()
	lvl := logLevel(cfg.LogLevel)
	// as linhas de log também alimentam o painel (tee)
	slog.SetDefault(slog.New(thePanel.logTee(
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}), lvl)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, flag.Args()); err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("interrompido")
			return
		}
		slog.Error("falhou", "err", err)
		os.Exit(1)
	}
}

func logLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func run(ctx context.Context, cfg Config, args []string) error {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "login":
		return runLogin(ctx, cfg)
	case "inspect":
		return runInspect(ctx, cfg)
	case "modes":
		return runModes(ctx, cfg)
	case "probe-menu":
		return runProbeMenu(ctx, cfg)
	case "test":
		if len(args) < 2 {
			return errors.New(`uso: bifrost test "seu prompt"`)
		}
		return runTest(ctx, cfg, strings.Join(args[1:], " "))
	case "serve", "":
		return runServe(ctx, cfg)
	default:
		flag.Usage()
		return nil
	}
}

// runLogin abre o Gemini e espera existir sessão. Na primeira execução o
// login é manual, na janela do Chromium; nada é automatizado. Detectada a
// sessão, o processo segue vivo até Ctrl+C ou a janela fechar — assim o
// Chromium encerra com graça e grava os cookies no profile.
func runLogin(ctx context.Context, cfg Config) error {
	if cfg.Headless {
		slog.Warn("login manual exige janela visível; ignorando headless")
		cfg.Headless = false
	}
	browser, err := StartBrowser(ctx, cfg)
	if err != nil {
		return err
	}
	defer browser.Close()

	if err := OpenGemini(browser.BootCtx); err != nil {
		return err
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var last string
	toldUser := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-browser.BootCtx.Done():
			return fmt.Errorf("chromium fechou antes da sessão ser confirmada")
		case <-ticker.C:
			state, url, err := GeminiState(browser.BootCtx)
			if err != nil {
				continue // navegação em andamento; tenta de novo no próximo tick
			}
			if s := state.String(); s != last {
				slog.Info("page state", "state", s, "url", url)
				last = s
			}
			switch state {
			case stateLoggedIn:
				slog.Info("gemini session ok", "profile", cfg.Profile)
				slog.Info("pode fechar a janela do Chromium (ou Ctrl+C) para encerrar")
				select {
				case <-ctx.Done():
				case <-browser.BootCtx.Done():
				}
				return nil
			case stateLoginNeeded:
				if !toldUser {
					slog.Info("faça login com sua conta na janela do Chromium")
					toldUser = true
				}
			}
		}
	}
}

// runInspect abre o Gemini e despeja o DOM em JSON para confirmar seletores.
// Exige sessão logada: sem login o chat — e os elementos que interessam —
// não existem na página.
func runInspect(ctx context.Context, cfg Config) error {
	browser, err := StartBrowser(ctx, cfg)
	if err != nil {
		return err
	}
	defer browser.Close()

	if err := OpenGemini(browser.BootCtx); err != nil {
		return err
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		state, _, err := GeminiState(browser.BootCtx)
		if err == nil {
			if state == stateLoggedIn {
				break
			}
			if state == stateLoginNeeded {
				slog.Warn("sem sessão detectada — despejando a página deslogada para diagnóstico")
				break
			}
		}
		if time.Now().After(deadline) {
			slog.Warn("estado da página não confirmou em 10s; despejando o DOM atual mesmo assim")
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	dump, err := InspectGemini(browser.BootCtx)
	if err != nil {
		return err
	}
	fmt.Println(dump)

	var shot []byte
	if err := chromedp.Run(browser.BootCtx, chromedp.ActionFunc(func(c context.Context) error {
		buf, err := page.CaptureScreenshot().Do(c)
		if err != nil {
			return err
		}
		shot = buf
		return nil
	})); err != nil {
		slog.Warn("screenshot falhou", "err", err)
	} else if err := os.WriteFile("data/inspect.png", shot, 0o644); err != nil {
		slog.Warn("salvar screenshot falhou", "err", err)
	} else {
		slog.Info("screenshot salvo", "path", "data/inspect.png")
	}
	return nil
}

// runTest é o milestone obrigatório: provar o caminho completo
// prompt → envio → geração → resposta extraída → texto no terminal.
func runTest(ctx context.Context, cfg Config, prompt string) error {
	browser, err := StartBrowser(ctx, cfg)
	if err != nil {
		return err
	}
	defer browser.Close()

	if err := OpenGemini(browser.BootCtx); err != nil {
		return err
	}

	g := NewGemini(browser.BootCtx)

	// toda chamada ao Gemini tem timeout
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	slog.Info("request received", "prompt", prompt)
	resp, err := g.Complete(cctx, []Message{{Role: "user", Content: prompt}}, cfg.Model)
	if err != nil {
		// em caso de falha, despeja o DOM para diagnosticar seletores
		if dump, derr := InspectGemini(browser.BootCtx); derr == nil {
			slog.Error("falhou; despejo do DOM para diagnóstico:")
			fmt.Println(dump)
		}
		return err
	}
	fmt.Println(resp)

	// esqueleto da resposta em JSON — confirmação dos seletores de extração
	// (code blocks, markdown) com uma resposta ainda na tela. A espera de 5s
	// deixa a UI pós-resposta (chips de follow-up etc.) renderizar, para o
	// dump mostrar o que NÃO deve entrar na extração.
	time.Sleep(5 * time.Second)
	if dump, derr := ResponseStructure(browser.BootCtx); derr == nil {
		fmt.Println("--- estrutura da última resposta ---")
		fmt.Println(dump)
	} else {
		slog.Warn("dump da estrutura falhou", "err", derr)
	}
	return nil
}

// runServe sobe a API HTTP OpenAI-compatible sobre o Gemini Web.
func runServe(ctx context.Context, cfg Config) error {
	gw := NewGateway(ctx, cfg, GetProvider(cfg.Provider))
	defer gw.close()

	// warm-up: browser de pé antes de abrir a porta
	if _, release, err := gw.acquire(ctx); err != nil {
		return err
	} else {
		release()
	}

	router := chi.NewRouter()
	router.Use(apiMiddleware)
	router.Use(authMiddleware(cfg.APIKey))
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusNotFound, errInvalidRequest, "not_found", "rota inexistente: "+r.URL.Path)
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusMethodNotAllowed, errInvalidRequest, "method_not_allowed", r.Method+" não suportado em "+r.URL.Path)
	})
	router.Get("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	router.Get("/health", handleHealth(gw))
	router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/panel", http.StatusFound)
	})
	router.Get("/panel", handlePanel)
	router.Get("/panel/data", handlePanelData(gw))
	router.Post("/panel/login", handlePanelLogin(gw))
	router.Get("/panel/login/status", handlePanelLoginStatus(gw))
	router.Route("/v1", func(v1 chi.Router) {
		v1.Get("/models", handleModels(gw))
		v1.Get("/models/{model}", handleModel(gw))
		v1.Post("/chat/completions", handleChat(gw))
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("server started", "addr", cfg.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("encerrando")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// runProbeMenu diagnostica o seletor de modos: abre o menu e despeja a
// estrutura real dos itens (tags, roles, atributos, HTML truncado) +
// tenta um clique com sequência completa de eventos de ponteiro e lê o
// rótulo resultante. É a ferramenta para quando `modes` para de
// confirmar trocas — a UI muda, este comando mostra o que virou.
func runProbeMenu(ctx context.Context, cfg Config) error {
	browser, err := StartBrowser(ctx, cfg)
	if err != nil {
		return err
	}
	defer browser.Close()

	if err := OpenGemini(browser.BootCtx); err != nil {
		return err
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		state, _, err := GeminiState(browser.BootCtx)
		if err == nil && state == stateLoggedIn {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("sem sessão logada — rode `bifrost login` primeiro")
		}
		time.Sleep(300 * time.Millisecond)
	}

	// abre o menu
	var dummy string
	if err := chromedp.Run(browser.BootCtx,
		chromedp.Evaluate(`(() => {
			const btn = document.querySelector('bard-mode-switcher button');
			if (!btn) return 'seletor não encontrado';
			btn.click();
			return '';
		})()`, &dummy),
		chromedp.Sleep(800*time.Millisecond),
	); err != nil {
		return fmt.Errorf("abrir menu: %w", err)
	}

	// despeja a estrutura: cada gem-menu-item com tag/role/aria/HTML, os
	// containers de menu no top layer e os candidatos a item clicável
	dumpJS := `JSON.stringify((() => {
		const norm = s => (s || '').replace(/\s+/g, ' ').trim();
		const items = [...document.querySelectorAll('gem-menu-item')].map(el => ({
			tag: el.tagName.toLowerCase(),
			role: el.getAttribute('role'),
			aria: norm(el.getAttribute('aria-label')),
			disabled: el.hasAttribute('disabled') || el.getAttribute('aria-disabled') === 'true',
			text: norm(el.innerText).slice(0, 60),
			kids: [...el.children].map(c => c.tagName.toLowerCase() + (c.getAttribute('role') ? ':' + c.getAttribute('role') : '')).slice(0, 6),
			html: norm(el.outerHTML).slice(0, 260),
		}));
		const roles = [...document.querySelectorAll('[role=menuitem], [role=menu], [role=listbox], [role=option]')].map(el => ({
			tag: el.tagName.toLowerCase(),
			role: el.getAttribute('role'),
			text: norm(el.innerText).slice(0, 60),
		})).slice(0, 30);
		const btn = document.querySelector('bard-mode-switcher button');
		return {
			label: btn ? btn.getAttribute('aria-label') : '',
			items: items,
			roles: roles,
			popovers: [...document.querySelectorAll('[popover], .cdk-overlay-container, [role=dialog]')].map(el => ({
				tag: el.tagName.toLowerCase(),
				cls: norm(el.className.toString()).slice(0, 80),
				open: el.hasAttribute('open'),
			})).slice(0, 10),
		};
	})())`
	var raw string
	if err := chromedp.Run(browser.BootCtx, chromedp.Evaluate(dumpJS, &raw)); err != nil {
		return fmt.Errorf("despejar menu: %w", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		fmt.Println(raw)
	} else {
		fmt.Println(buf.String())
	}

	// tenta selecionar "3.1 Pro" com sequência completa de eventos de
	// ponteiro (algumas UIs ignoram click() sintético sem pointerdown/up)
	fmt.Println("--- tentativa de clique em '3.1 Pro' ---")
	var clickResult string
	if err := chromedp.Run(browser.BootCtx,
		chromedp.Evaluate(`(() => {
			const items = [...document.querySelectorAll('gem-menu-item')];
			const el = items.find(el => el.textContent.includes('3.1 Pro'));
			if (!el) return 'item não encontrado';
			const target = el.querySelector('button, [role=menuitem]') || el;
			const r = target.getBoundingClientRect();
			const opts = {bubbles: true, cancelable: true, clientX: r.x + r.width/2, clientY: r.y + r.height/2};
			target.dispatchEvent(new PointerEvent('pointerdown', opts));
			target.dispatchEvent(new MouseEvent('mousedown', opts));
			target.dispatchEvent(new PointerEvent('pointerup', opts));
			target.dispatchEvent(new MouseEvent('mouseup', opts));
			target.click();
			return 'clique em ' + target.tagName.toLowerCase();
		})()`, &clickResult),
		chromedp.Sleep(3000*time.Millisecond),
		chromedp.Evaluate(`(() => {
			const btn = document.querySelector('bard-mode-switcher button');
			return (btn ? btn.getAttribute('aria-label') : '') + ' | itens: ' + document.querySelectorAll('gem-menu-item').length;
		})()`, &clickResult),
	); err != nil {
		return fmt.Errorf("clique de teste: %w", err)
	}
	fmt.Println("clique:", clickResult)
	return nil
}

// runModes abre o menu de modos do Gemini e, item por item, troca e lê o
// rótulo ativo do botão — a tabela item ↔ rótulo que alimenta o registro
// de modelos. Rodar com sessão logada e sem o serve no ar.
func runModes(ctx context.Context, cfg Config) error {
	browser, err := StartBrowser(ctx, cfg)
	if err != nil {
		return err
	}
	defer browser.Close()

	if err := OpenGemini(browser.BootCtx); err != nil {
		return err
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		state, _, err := GeminiState(browser.BootCtx)
		if err == nil && state == stateLoggedIn {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("sem sessão logada — rode `bifrost login` primeiro")
		}
		time.Sleep(300 * time.Millisecond)
	}

	// o seletor de modo pode renderizar depois do estado logado — esperamos
	// por ele (sinal real do DOM), não por tempo fixo
	deadline = time.Now().Add(10 * time.Second)
	for {
		var has string
		if err := chromedp.Run(browser.BootCtx, chromedp.Evaluate(fmt.Sprintf(`(() => {
			return document.querySelector(%q) ? '1' : '';
		})()`, geminiSelectors.ModeSwitcher), &has)); err == nil && has == "1" {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("seletor de modo não apareceu — layout mudou? rode `bifrost inspect`")
		}
		time.Sleep(300 * time.Millisecond)
	}

	const openMenu = `(() => {
		const btn = document.querySelector('bard-mode-switcher button');
		if (!btn) return 'seletor não encontrado';
		btn.click();
		return '';
	})()`
	const readLabel = `(() => {
		const btn = document.querySelector('bard-mode-switcher button');
		return btn ? (btn.getAttribute('aria-label') || '') : '';
	})()`
	const listItems = `JSON.stringify([...document.querySelectorAll('gem-menu-item')]
		.map(el => (el.innerText || '').split('\n')[0].trim()))`
	const postClick = `JSON.stringify((() => {
		const btn = document.querySelector('bard-mode-switcher button');
		return {
			label: btn ? (btn.getAttribute('aria-label') || '') : '',
			overlay: [...document.querySelectorAll('gem-menu-item')]
				.map(el => (el.innerText || '').split('\n')[0].trim())
		};
	})())`

	var dummy, initialLabel, rawItems string
	if err := chromedp.Run(browser.BootCtx,
		chromedp.Evaluate(readLabel, &initialLabel),
		chromedp.Evaluate(openMenu, &dummy),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.Evaluate(listItems, &rawItems),
		chromedp.Evaluate(openMenu, &dummy), // fecha o menu de novo
	); err != nil {
		return fmt.Errorf("listar modos: %w", err)
	}

	var items []string
	if err := json.Unmarshal([]byte(rawItems), &items); err != nil {
		return fmt.Errorf("decodificar itens: %w", err)
	}

	fmt.Println("modo atual:", initialLabel)
	fmt.Printf("%d itens no menu:\n", len(items))

	for i, item := range items {
		var post string
		err := chromedp.Run(browser.BootCtx,
			chromedp.Evaluate(openMenu, &dummy),
			chromedp.Sleep(600*time.Millisecond),
			chromedp.Evaluate(fmt.Sprintf(`(() => {
				const items = [...document.querySelectorAll('gem-menu-item')];
				if (!items[%d]) return 'item não existe';
				items[%d].click();
				return '';
			})()`, i, i), &dummy),
			// 2.5s: o rótulo ativo demora a atualizar (e submenus a
			// renderizar) — 900ms dava leitura falsa do modo anterior
			chromedp.Sleep(2500*time.Millisecond),
			chromedp.Evaluate(postClick, &post),
			chromedp.Evaluate(openMenu, &dummy), // fecha o menu de novo
		)
		if err != nil {
			fmt.Printf("  %-24s ERRO: %v\n", item, err)
			continue
		}
		var st struct {
			Label   string   `json:"label"`
			Overlay []string `json:"overlay"`
		}
		if err := json.Unmarshal([]byte(post), &st); err != nil {
			fmt.Printf("  %-24s ERRO decode: %v\n", item, err)
			continue
		}
		extra := ""
		if len(st.Overlay) > 0 {
			extra = "  | submenu: " + strings.Join(st.Overlay, ", ")
		}
		fmt.Printf("  %-24s → %s%s\n", item, st.Label, extra)
	}
	return nil
}
