package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// queuedRequests: quantas requisições podem esperar além da que executa.
// Acima disso, 429 em vez de cliente preso por minutos.
const queuedRequests = 4

// Gateway supervisiona o Chromium: relança se morrer (auto-recuperação),
// serializa as execuções (uma por vez, como o design manda) e limita a
// fila de espera.
type Gateway struct {
	cfg   Config
	ctx   context.Context // ciclo de vida do servidor (browser não morre com um request)
	sem   chan struct{}   // cap 1: uma execução por vez, com cancelamento por contexto
	swapMu sync.Mutex     // protege browser/gemini em seções críticas curtas
	browser *Browser
	gemini  *Gemini
	queue   chan struct{} // cap 1+queuedRequests: admissão total
}

func NewGateway(ctx context.Context, cfg Config) *Gateway {
	return &Gateway{
		cfg:   cfg,
		ctx:   ctx,
		sem:   make(chan struct{}, 1),
		queue: make(chan struct{}, 1+queuedRequests),
	}
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

// acquire devolve o Gemini vivo, relançando o Chromium se tiver morrido.
// Espera a vez respeitando o contexto do chamador.
func (gw *Gateway) acquire(ctx context.Context) (*Gemini, func(), error) {
	select {
	case gw.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	release := func() { <-gw.sem }

	gw.swapMu.Lock()
	browser, gemini := gw.browser, gw.gemini
	dead := browser == nil || !browser.Alive()
	gw.swapMu.Unlock()

	if dead {
		gw.swapMu.Lock()
		if gw.browser != nil {
			slog.Warn("browser morto; relançando")
			gw.browser.Close()
		} else {
			slog.Info("browser starting")
		}
		gw.swapMu.Unlock()

		b, err := StartBrowser(gw.ctx, gw.cfg)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("relançar chromium: %w", err)
		}
		if err := OpenGemini(b.Ctx); err != nil {
			b.Close()
			release()
			return nil, nil, err
		}
		gw.swapMu.Lock()
		gw.browser, gw.gemini = b, NewGemini(b.Ctx)
		gemini = gw.gemini
		gw.swapMu.Unlock()
		slog.Info("browser recovered")
	}
	return gemini, release, nil
}

// status reporta saúde sem bloquear atrás de uma geração em andamento.
func (gw *Gateway) status() map[string]string {
	// alguém executando? não dá para verificar agora — e isso é informação
	select {
	case gw.sem <- struct{}{}:
		<-gw.sem
	default:
		return map[string]string{"status": "ok", "busy": "true"}
	}

	gw.swapMu.Lock()
	defer gw.swapMu.Unlock()
	if gw.browser == nil || !gw.browser.Alive() {
		return map[string]string{"status": "degraded", "browser": "down"}
	}
	state, _, err := GeminiState(gw.browser.Ctx)
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
