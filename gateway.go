package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"
)

// queuedRequests: quantas requisições podem esperar além da que executa.
// Acima disso, 429 em vez de cliente preso por minutos.
const queuedRequests = 4

// LoginStatus descreve o estado atual do fluxo de login iniciado pelo painel.
type LoginStatus struct {
	Active  bool   `json:"active"`
	Done    bool   `json:"done"`
	Ok      bool   `json:"ok"`
	Message string `json:"message"`
}

// Gateway supervisiona o Chromium: relança se morrer (auto-recuperação),
// gerencia um pool de abas para concorrência e limita a fila de espera.
type Gateway struct {
	cfg     Config
	ctx     context.Context // ciclo de vida do servidor (browser não morre com um request)
	sem     chan int        // tokens de índice (0 a PoolSize-1) para as abas
	swapMu  sync.Mutex      // protege browser/workers em seções críticas curtas
	browser *Browser
	factory ProviderFactory
	workers []LLMWorker
	queue   chan struct{} // cap PoolSize+queuedRequests: admissão total

	loginActive atomic.Bool
	loginMu     sync.RWMutex
	loginStatus LoginStatus
}

func NewGateway(ctx context.Context, cfg Config, factory ProviderFactory) *Gateway {
	poolSize := cfg.PoolSize
	if poolSize < 1 {
		poolSize = 1
	}
	gw := &Gateway{
		cfg:     cfg,
		ctx:     ctx,
		sem:     make(chan int, poolSize),
		factory: factory,
		workers: make([]LLMWorker, poolSize),
		queue:   make(chan struct{}, poolSize+queuedRequests),
	}
	for i := 0; i < poolSize; i++ {
		gw.sem <- i
	}
	return gw
}

// tryAdmit devolve false (429) quando a fila está cheia.
func (gw *Gateway) tryAdmit() (leave func(), ok bool) {
	select {
	case gw.queue <- struct{}{}:
		return func() { <-gw.queue }, true
	default:
		return nil, false
	}
}

// acquire devolve um worker do pool vivo, relançando o Chromium se tiver morrido.
func (gw *Gateway) acquire(ctx context.Context) (LLMWorker, func(), error) {
	var idx int
	select {
	case idx = <-gw.sem:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	release := func() { gw.sem <- idx }

	gw.swapMu.Lock()
	browser, worker := gw.browser, gw.workers[idx]
	dead := browser == nil || !browser.Alive()

	if dead {
		if gw.loginActive.Load() {
			// login pelo painel em andamento: o browser de login detém o
			// profile — relançar aqui falharia com erro de singleton
			// confuso. Erro claro em vez disso.
			gw.swapMu.Unlock()
			release()
			return nil, nil, errors.New("login em andamento pelo painel; aguarde a conclusão e tente de novo")
		}
		if err := gw.recoverLocked(); err != nil {
			gw.swapMu.Unlock()
			release()
			return nil, nil, err
		}
		worker = gw.workers[idx]
	}
	gw.swapMu.Unlock()

	return worker, release, nil
}

// recoverLocked relança o browser e recria todas as abas. Deve ser chamado com swapMu bloqueado.
func (gw *Gateway) recoverLocked() error {
	if gw.browser != nil {
		slog.Warn("browser morto; relançando")
		gw.browser.Close()
	} else {
		slog.Info("browser starting")
	}

	b, err := StartBrowser(gw.ctx, gw.cfg)
	if err != nil {
		return fmt.Errorf("relançar chromium: %w", err)
	}
	gw.browser = b

	// Recria o pool de abas
	for i := 0; i < gw.cfg.PoolSize; i++ {
		var tabCtx context.Context
		if i == 0 {
			// Reaproveita a aba de boot (about:blank inicial)
			tabCtx = b.BootCtx
		} else {
			var cancelTab context.CancelFunc
			tabCtx, cancelTab = chromedp.NewContext(b.AllocCtx)
			_ = cancelTab // o chromedp cancela as abas quando allocCtx morre
		}
		if err := gw.factory.Open(tabCtx); err != nil {
			b.Close()
			return fmt.Errorf("falha ao navegar aba %d: %w", i, err)
		}
		gw.workers[i] = gw.factory.NewWorker(tabCtx)
	}

	slog.Info("browser recovered", "pool_size", gw.cfg.PoolSize)
	return nil
}

// queueDepth reporta quantos requests esperam (para o painel).
func (gw *Gateway) queueDepth() int {
	return len(gw.queue)
}

// status reporta saúde sem bloquear atrás de uma geração em andamento.
func (gw *Gateway) status() map[string]string {
	if gw.loginActive.Load() {
		// login pelo painel em andamento: o browser de login está de pé,
		// mas o pool de produção só volta quando a sessão confirmar
		return map[string]string{"status": "login", "browser": "up"}
	}
	select {
	case idx := <-gw.sem:
		gw.sem <- idx
	default:
		return map[string]string{"status": "ok", "busy": "true"}
	}

	gw.swapMu.Lock()
	defer gw.swapMu.Unlock()
	if gw.browser == nil || !gw.browser.Alive() {
		return map[string]string{"status": "degraded", "browser": "down"}
	}
	state, _, err := gw.factory.State(gw.browser.BootCtx)
	if err != nil || state != stateLoggedIn {
		return map[string]string{"status": "degraded", "session": "missing"}
	}
	return map[string]string{"status": "ok"}
}

// close encerra o browser supervisionado.
func (gw *Gateway) close() {
	gw.swapMu.Lock()
	defer gw.swapMu.Unlock()
	if gw.browser != nil {
		gw.browser.Close()
		gw.browser = nil
	}
}

// GetLoginStatus devolve o estado atual do fluxo de login do painel.
func (gw *Gateway) GetLoginStatus() LoginStatus {
	gw.loginMu.RLock()
	defer gw.loginMu.RUnlock()
	return gw.loginStatus
}

func (gw *Gateway) setLoginStatus(s LoginStatus) {
	gw.loginMu.Lock()
	gw.loginStatus = s
	gw.loginMu.Unlock()
}

// TriggerLogin inicia o fluxo de login interativo a partir do painel.
func (gw *Gateway) TriggerLogin() bool {
	if !gw.loginActive.CompareAndSwap(false, true) {
		return false // já em andamento
	}
	go func() {
		defer gw.loginActive.Store(false)

		gw.setLoginStatus(LoginStatus{Active: true, Message: "Fechando browser atual…"})

		gw.swapMu.Lock()
		if gw.browser != nil {
			gw.browser.Close()
			gw.browser = nil
			for i := range gw.workers {
				gw.workers[i] = nil
			}
		}
		gw.swapMu.Unlock()

		gw.setLoginStatus(LoginStatus{Active: true, Message: "Abrindo Chrome para login…"})

		cfg := gw.cfg
		cfg.Headless = false
		b, err := StartBrowser(gw.ctx, cfg)
		if err != nil {
			slog.Error("login: falhou ao abrir browser", "err", err)
			gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Message: "Erro ao abrir o Chrome: " + err.Error()})
			return
		}

		if err := gw.factory.Open(b.BootCtx); err != nil {
			b.Close()
			slog.Error("login: falhou ao abrir provedor", "err", err)
			gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Message: "Erro ao navegar ao provedor: " + err.Error()})
			return
		}

		gw.setLoginStatus(LoginStatus{Active: true, Message: "Aguardando login na janela do Chrome…"})
		slog.Info("login: janela aberta, aguardando login do usuário")

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		timeout := time.NewTimer(5 * time.Minute)
		defer timeout.Stop()

		for {
			select {
			case <-gw.ctx.Done():
				b.Close()
				gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Message: "Servidor encerrado durante o login"})
				return
			case <-b.BootCtx.Done():
				gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Message: "Janela do Chrome fechada antes do login ser confirmado"})
				return
			case <-timeout.C:
				b.Close()
				gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Message: "Timeout: login não completado em 5 minutos"})
				return
			case <-ticker.C:
				state, _, err := gw.factory.State(b.BootCtx)
				if err != nil {
					continue
				}
				if state == stateLoggedIn {
					slog.Info("login: sessão confirmada")
					gw.setLoginStatus(LoginStatus{Active: true, Message: "Sessão confirmada! Gravando e restaurando o gateway…"})

					// cortesia: deixa redirects e cookies assentarem antes do
					// close gracioso (que grava o profile em disco)
					select {
					case <-gw.ctx.Done():
						b.Close()
						return
					case <-b.BootCtx.Done():
					case <-time.After(3 * time.Second):
					}

					// O browser de login NÃO vira o browser de produção: ele é
					// headed por natureza e, se o usuário fechasse a janela (ou
					// o timeout de 2min antigo disparasse), instalávamos um
					// browser morto no gateway — "browser down" no painel até o
					// próximo request se auto-curar. Close gracioso + relança
					// com a config normal (respeita BIFROST_HEADLESS).
					b.Close()

					gw.swapMu.Lock()
					rerr := gw.recoverLocked()
					gw.swapMu.Unlock()
					if rerr != nil {
						slog.Error("login: falhou ao restaurar gateway", "err", rerr)
						gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Message: "Sessão ok, mas falhou ao restaurar o browser: " + rerr.Error()})
						return
					}
					gw.setLoginStatus(LoginStatus{Done: true, Ok: true, Message: "Login concluído com sucesso!"})
					slog.Info("login: gateway restaurado com nova sessão", "pool_size", gw.cfg.PoolSize)
					return
				}
			}
		}
	}()
	return true
}
