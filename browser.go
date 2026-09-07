package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

// Browser controla uma única instância do Chromium com profile persistente,
// uma aba e um contexto chromedp para todas as ações.
type Browser struct {
	Ctx    context.Context
	cancel context.CancelFunc
}

// headlessFlag devolve o valor para a flag --headless: "new" (o modo atual,
// único desde o Chrome 132) ou false para remover a flag em modo headed.
func headlessFlag(headless bool) any {
	if headless {
		return "new"
	}
	return false
}

// StartBrowser lança o Chromium com --user-data-dir no profile da config. O
// profile persiste entre execuções — é ele que guarda o login do Gemini.
func StartBrowser(ctx context.Context, cfg Config) (*Browser, error) {
	profileDir := cfg.Profile
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return nil, fmt.Errorf("criar profile %s: %w", profileDir, err)
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(profileDir),
		// O chromedp passaria --headless no formato antigo, removido no
		// Chrome 132+; o modo atual é o "new". Em headed, a flag não vai.
		chromedp.Flag("headless", headlessFlag(cfg.Headless)),
		// O chromedp adiciona --enable-automation por padrão (herança de um
		// Puppeteer antigo). Essa flag seta navigator.webdriver=true e faz o
		// login do Google bloquear com "navegador não seguro". Ela não é
		// necessária para nada: o CDP funciona sem ela.
		chromedp.Flag("enable-automation", false),
		// Keystore: o Chrome do desktop cifra cookies com a chave do
		// gnome-keyring (libsecret); o "basic" cifra com chave interna do
		// próprio profile. O keystore TEM de ser o mesmo em todo acesso ao
		// profile — quem abre com store errado não decifra (e às vezes
		// descarta) os cookies do login. Desktop: gnome-libsecret;
		// container (sem keyring): basic.
		chromedp.Flag("password-store", cfg.PasswordStore),
		chromedp.Flag("use-mock-keychain", false),
	)
	if cfg.NoSandbox {
		// O sandbox do Chrome (namespaces) não sobrevive ao seccomp default
		// do Docker; sem essas flags o processo morre no boot. O isolamento
		// fica por conta do container (usuário não-root, sem privilégios).
		opts = append(opts,
			chromedp.Flag("no-sandbox", true),
			// /dev/shm default de container (64MB) é pouco para o Chrome;
			// com esta flag ele usa /tmp.
			chromedp.Flag("disable-dev-shm-usage", true),
		)
	}
	if cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(cfg.ChromePath))
	}

	tryStart := func() (*Browser, error) {
		allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
		browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)

		// O chromedp só lança o processo de fato na primeira ação.
		if err := chromedp.Run(browserCtx, chromedp.Navigate("about:blank")); err != nil {
			cancelBrowser()
			cancelAlloc()
			return nil, err
		}
		return &Browser{Ctx: browserCtx, cancel: func() {
			cancelBrowser()
			cancelAlloc()
		}}, nil
	}

	browser, err := tryStart()
	// Chrome morto sem graceful close (SIGKILL de container, crash) deixa o
	// lock singleton órfão — e cada container tem hostname diferente, então
	// o Chrome acha que "outra máquina" o prende. Socket morto = órfão:
	// limpa e tenta uma vez mais. Socket vivo é outra instância de verdade.
	if err != nil && isSingletonError(err) && clearStaleSingletonLock(profileDir) {
		slog.Warn("lock singleton órfão no profile; removido e relançando", "profile", profileDir)
		browser, err = tryStart()
	}
	if err != nil {
		if isSingletonError(err) {
			// 1 Chromium, 1 profile: segunda instância aborta por design.
			return nil, fmt.Errorf("profile %s já está em uso por outra instância do bifrost (ou um Chrome com o mesmo user-data-dir). "+
				"Encerre-a antes (pkill -x bifrost) — com o servidor no ar, teste via HTTP: "+
				"curl http://localhost:8081/v1/chat/completions", profileDir)
		}
		return nil, fmt.Errorf("iniciar chromium: %w", err)
	}

	slog.Info("browser started",
		"profile", profileDir,
		"headless", cfg.Headless,
		"chrome", cfg.ChromePath,
		"password-store", cfg.PasswordStore,
		"no-sandbox", cfg.NoSandbox,
	)
	return browser, nil
}

// isSingletonError: falha de lançamento causada pelo lock do profile. O
// Chrome escreve "process_singleton_posix.cc" / "The profile appears to be
// in use" (a mensagem literal "SingletonLock" nem sempre aparece).
func isSingletonError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "singletonlock") ||
		strings.Contains(msg, "processsingleton") ||
		strings.Contains(msg, "process_singleton") ||
		strings.Contains(msg, "the profile appears to be in use")
}

// clearStaleSingletonLock devolve true se removeu locks órfãos (nenhum
// Chrome vivo os segura — o socket singleton não responde). Se o socket
// atende, há uma instância viva e NADA é removido.
func clearStaleSingletonLock(profileDir string) bool {
	sock := filepath.Join(profileDir, "SingletonSocket")
	if conn, err := net.Dial("unix", sock); err == nil {
		_ = conn.Close()
		return false // um Chrome vivo possui o profile
	}
	removed := false
	for _, f := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		if err := os.Remove(filepath.Join(profileDir, f)); err == nil {
			removed = true
		}
	}
	return removed
}

// Alive reporta se o Chromium ainda está de pé. O contexto do chromedp é
// cancelado automaticamente quando o browser morre.
func (b *Browser) Alive() bool {
	return b.Ctx.Err() == nil
}

// Close encerra o Chromium com Browser.close (graceful, grava o profile);
// em qualquer falha cai no cancelamento dos contextos, que mata o processo.
func (b *Browser) Close() {
	if b.Ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(b.Ctx, 3*time.Second)
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
			return browser.Close().Do(c)
		})); err != nil {
			slog.Debug("graceful close indisponível", "err", err)
		}
		cancel()
	}
	b.cancel()
}
