package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"
)

// queuedRequests: quantas requisições podem esperar além das que executam.
// Acima disso, 429 em vez de cliente preso por minutos.
const queuedRequests = 4

// LoginStatus descreve o estado atual do fluxo de login iniciado pelo painel.
type LoginStatus struct {
	Active  bool   `json:"active"`
	Done    bool   `json:"done"`
	Ok      bool   `json:"ok"`
	Profile string `json:"profile,omitempty"`
	Message string `json:"message"`
}

// errLoginInProgress: login de painel em andamento neste profile — o browser
// de login detém o user-data-dir; relançar aqui falharia com singleton.
var errLoginInProgress = errors.New("login em andamento pelo painel; aguarde a conclusão e tente de novo")

// shard: um browser amarrado a um profile (conta), com seu pool de abas.
// Multi-profile = N shards em round-robin — o limite de uso do Gemini web
// é por CONTA, então N contas multiplicam a capacidade.
type shard struct {
	name      string     // base do diretório do profile (exibição)
	cfg       Config     // config com Profile deste shard
	mu        sync.Mutex // browser/workers em seções críticas curtas
	browser   *Browser
	workers   []LLMWorker
	sem       chan int    // tokens de aba (0..poolSize-1)
	noSession atomic.Bool // sessão ausente: fora da rotação até login
	suspect   atomic.Bool // browser doente (página que nem reload/navegação destrava): relançar no próximo acquire
}

// Gateway supervisiona os shards: round-robin com AFINIDADE de conversa
// (turnos consecutivos do mesmo agente caem no shard onde a conversa
// aderente já vive), relança browsers mortos e limita a fila de espera.
type Gateway struct {
	cfg     Config          // config global (model default etc.); por-shard: shard.cfg
	ctx     context.Context // ciclo de vida do servidor (browser não morre com um request)
	factory ProviderFactory
	shards  []*shard
	rr      atomic.Int64  // ponteiro de round-robin
	queue   chan struct{} // admissão total (todas as abas + espera)
	free    chan struct{} // sinal de aba liberada (acordar waiters)
	convMu  sync.Mutex
	conv    map[string]int // chave de conversa → shard (afinidade)

	loginActive atomic.Bool
	loginShard  int
	loginMu     sync.RWMutex
	loginStatus LoginStatus
}

func NewGateway(ctx context.Context, cfg Config, factory ProviderFactory) *Gateway {
	profiles := cfg.Profiles
	if len(profiles) == 0 {
		profiles = []string{cfg.Profile}
	}
	poolSize := cfg.PoolSize
	if poolSize < 1 {
		poolSize = 1
	}
	totalTabs := 0
	shards := make([]*shard, 0, len(profiles))
	seen := map[string]bool{}
	for _, p := range profiles {
		if seen[p] {
			continue // profile duplicado: um browser por user-data-dir
		}
		seen[p] = true
		sc := cfg
		sc.Profile = p
		s := &shard{
			name:    filepath.Base(p),
			cfg:     sc,
			workers: make([]LLMWorker, poolSize),
			sem:     make(chan int, poolSize),
		}
		for i := 0; i < poolSize; i++ {
			s.sem <- i
		}
		shards = append(shards, s)
		totalTabs += poolSize
	}
	return &Gateway{
		cfg:     cfg,
		ctx:     ctx,
		factory: factory,
		shards:  shards,
		queue:   make(chan struct{}, totalTabs+queuedRequests),
		free:    make(chan struct{}, totalTabs),
		conv:    map[string]int{},
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

// poke acorda waiters do acquire (sinal de aba liberada).
func (gw *Gateway) poke() {
	select {
	case gw.free <- struct{}{}:
	default:
	}
}

// convLookup devolve o shard dono da conversa (afinidade).
func (gw *Gateway) convLookup(key string) (int, bool) {
	if key == "" {
		return 0, false
	}
	gw.convMu.Lock()
	defer gw.convMu.Unlock()
	idx, ok := gw.conv[key]
	if ok && idx >= 0 && idx < len(gw.shards) {
		return idx, true
	}
	return 0, false
}

// convBind registra a conversa no shard que a atendeu. Mapa com teto: ao
// passar de 1024 chaves, zera — conversas mortas não acumulam para sempre;
// uma conversa viva re-registra no próximo turno.
func (gw *Gateway) convBind(key string, idx int) {
	if key == "" {
		return
	}
	gw.convMu.Lock()
	if len(gw.conv) > 1024 {
		gw.conv = map[string]int{}
	}
	gw.conv[key] = idx
	gw.convMu.Unlock()
}

// Estados de tryShard.
const (
	shardGot  = iota // aba adquirida (worker válido)
	shardBusy        // sem aba livre
	shardSkip        // fora da rotação (sem sessão / login em andamento)
)

// tryShard tenta adquirir uma aba do shard de forma NÃO-bloqueante,
// relançando o browser se morto. Estados: shardGot (worker pronto),
// shardBusy (todas as abas ocupadas), shardSkip + err (falhou: recover
// ou login em andamento neste profile), shardSkip sem err (fora da
// rotação: sessão ausente).
func (gw *Gateway) tryShard(idx int) (int, LLMWorker, func(), error) {
	s := gw.shards[idx]
	if s.noSession.Load() {
		return shardSkip, nil, nil, nil
	}
	var i int
	select {
	case i = <-s.sem:
	default:
		return shardBusy, nil, nil, nil
	}
	release := func() { s.sem <- i; gw.poke() }
	s.mu.Lock()
	if s.browser == nil || !s.browser.Alive() || s.suspect.Load() {
		if gw.loginActive.Load() && gw.loginShard == idx {
			s.mu.Unlock()
			release()
			return shardSkip, nil, nil, errLoginInProgress
		}
		if err := gw.recoverShardLocked(s); err != nil {
			s.mu.Unlock()
			release()
			return shardSkip, nil, nil, fmt.Errorf("relançar chromium (profile %s): %w", s.name, err)
		}
	}
	w := s.workers[i]
	s.mu.Unlock()
	return shardGot, w, release, nil
}

// acquire devolve uma aba de um shard: AFINIDADE primeiro (o shard dono da
// conversa mantém o estado da conversa aderente), round-robin depois. Sem
// aba livre em shard algum, espera o sinal de liberação. Devolve o índice
// do shard (para marcar sessão ausente quando o worker reclamar).
func (gw *Gateway) acquire(ctx context.Context, convKey string) (int, LLMWorker, func(), error) {
	// afinidade: turnos consecutivos voltam ao shard da conversa
	if idx, ok := gw.convLookup(convKey); ok {
		if st, w, rel, err := gw.tryShard(idx); st == shardGot {
			return idx, w, rel, nil
		} else if err != nil {
			_ = err // o scan abaixo reflete melhor o estado do conjunto
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return -1, nil, nil, err
		}
		start := int(gw.rr.Add(1)) % len(gw.shards)
		var lastErr error
		anyBusy := false
		for i := 0; i < len(gw.shards); i++ {
			idx := (start + i) % len(gw.shards)
			st, w, rel, err := gw.tryShard(idx)
			if st == shardGot {
				gw.convBind(convKey, idx)
				return idx, w, rel, nil
			}
			if err != nil {
				lastErr = err
				continue
			}
			if st == shardBusy {
				anyBusy = true
			}
		}
		// nada livre: se alguém está OCUPADO, esperar vale a pena (vai
		// liberar); se todos estão fora da rotação, esperar é hang — erro.
		if anyBusy {
			select {
			case <-ctx.Done():
				return -1, nil, nil, ctx.Err()
			case <-gw.free:
			}
			continue
		}
		if lastErr != nil {
			return -1, nil, nil, lastErr
		}
		return -1, nil, nil, errors.New("nenhuma sessão disponível; abra o painel para conectar um profile")
	}
}

// recoverShardLocked relança o browser do shard e recria suas abas. Deve
// ser chamado com s.mu bloqueado.
func (gw *Gateway) recoverShardLocked(s *shard) error {
	if s.browser != nil {
		slog.Warn("browser morto; relançando", "profile", s.name)
		s.browser.Close()
	} else {
		slog.Info("browser starting", "profile", s.name)
	}

	b, err := StartBrowser(gw.ctx, s.cfg)
	if err != nil {
		return err
	}
	s.browser = b

	for i := 0; i < len(s.workers); i++ {
		var tabCtx context.Context
		if i == 0 {
			// Reaproveita a aba de boot (about:blank inicial)
			tabCtx = b.BootCtx
		} else {
			tabCtx, _ = chromedp.NewContext(b.AllocCtx)
			// o chromedp cancela as abas quando o allocCtx morre
		}
		if err := gw.factory.Open(tabCtx); err != nil {
			b.Close()
			return fmt.Errorf("falha ao navegar aba %d: %w", i, err)
		}
		s.workers[i] = gw.factory.NewWorker(tabCtx)
	}
	slog.Info("browser up", "profile", s.name, "pool_size", len(s.workers))
	s.suspect.Store(false) // browser novo: estado limpo
	return nil
}

// warmup sobe os browsers de TODOS os shards e pré-marca os que estão sem
// sessão — sem isso, a primeira requisição de cada profile vazio esbarrava
// num 503 de cold-start; com a marca, a rotação já começa certeira e o
// painel mostra quais profiles precisam de login.
func (gw *Gateway) warmup(ctx context.Context) error {
	started := 0
	var lastErr error
	for _, s := range gw.shards {
		s.mu.Lock()
		if s.browser == nil || !s.browser.Alive() {
			if err := gw.recoverShardLocked(s); err != nil {
				lastErr = err
				browser := s.browser
				s.mu.Unlock()
				if browser != nil {
					browser.Close()
				}
				slog.Error("warmup: browser não subiu", "profile", s.name, "err", err)
				continue
			}
		}
		b := s.browser
		s.mu.Unlock()
		started++
		state, err := gw.probeState(b)
		if err != nil {
			slog.Warn("warmup: página não respondeu ao probe", "profile", s.name, "err", err)
			continue
		}
		if state != stateLoggedIn {
			s.noSession.Store(true)
			slog.Warn("warmup: sessão ausente — login pelo painel", "profile", s.name)
		}
	}
	if started == 0 && lastErr != nil {
		return fmt.Errorf("nenhum browser subiu: %w", lastErr)
	}
	return nil
}

// markNoSession tira o shard da rotação (a próxima requisição dele seria
// 503 gemini_not_logged_in; sem a marca, cada turno esbarraria de novo).
func (gw *Gateway) markNoSession(idx int) {
	if idx < 0 || idx >= len(gw.shards) {
		return
	}
	s := gw.shards[idx]
	if !s.noSession.Swap(true) {
		slog.Warn("sessão ausente; profile fora da rotação até login", "profile", s.name)
	}
}

// markBrowserSuspect marca o browser do shard para RELANÇO no próximo
// acquire: página que nem reload nem página nova destravaram — o processo
// do Chromium não executa comandos CDP; só restart resolve. O request que
// detectou já falhou (503 gemini_page_unresponsive); o próximo acquire
// fecha e sobe um browser novo (workers recriados, sessão do profile
// preservada). O shard NÃO sai da rotação: a recuperação é automática.
func (gw *Gateway) markBrowserSuspect(idx int) {
	if idx < 0 || idx >= len(gw.shards) {
		return
	}
	s := gw.shards[idx]
	if !s.suspect.Swap(true) {
		slog.Warn("browser marcado para relanço (página não destrava nem com reload)", "profile", s.name)
	}
}

// queueDepth reporta quantos requests esperam (para o painel).
func (gw *Gateway) queueDepth() int {
	return len(gw.queue)
}

// stateProbeTimeout: teto do probe de estado em status()/warmup. A sonda
// é um Evaluate na página — se o renderer estiver engasgado (conversa
// gigante), o Evaluate pendura INDEFINIDAMENTE no BootCtx (sem deadline);
// sem este teto, /health e /panel/data travavam junto com a página e o
// gateway inteiro aparentava morte. Var (não const) para encurtar em teste.
var stateProbeTimeout = 5 * time.Second

// probeState sonda o estado da página com teto curto: página viva → estado
// real; página engasgada/morta → erro rápido. Quem consome (status, warmup)
// trata o erro como shard doente em vez de bloquear. O teto deriva do
// BootCtx (o Evaluate precisa do executor da aba no contexto).
func (gw *Gateway) probeState(b *Browser) (pageState, error) {
	pctx, cancel := context.WithTimeout(b.BootCtx, stateProbeTimeout)
	defer cancel()
	state, _, err := gw.factory.State(pctx)
	return state, err
}

// status agrega a saúde dos shards sem bloquear atrás de geração em
// andamento. Shards ocupados contam como ok (estão gerando = sessão
// funciona). Shard sem sessão é marcado — a rotação se auto-cura. Shard
// cuja página não responde ao probe (renderer engasgado) conta como DOWN:
// health/painel devolvem resposta na hora, nunca penduram na página.
func (gw *Gateway) status() map[string]string {
	if gw.loginActive.Load() {
		return map[string]string{"status": "login", "browser": "up"}
	}
	nOK, nDown, nMissing := 0, 0, 0
	for _, s := range gw.shards {
		select {
		case i := <-s.sem:
			s.sem <- i
		default:
			nOK++ // ocupado gerando
			continue
		}
		if s.noSession.Load() {
			nMissing++
			continue
		}
		s.mu.Lock()
		alive := s.browser != nil && s.browser.Alive()
		b := s.browser
		s.mu.Unlock()
		if !alive || b == nil {
			nDown++
			continue
		}
		state, err := gw.probeState(b)
		if err != nil {
			// probe não respondeu no teto: página engasgada conta como down
			// (o shard segue na rotação — um request pode destravá-lo com
			// reload; health não pode esperar por isso)
			nDown++
			continue
		}
		if state != stateLoggedIn {
			s.noSession.Store(true)
			nMissing++
			continue
		}
		nOK++
	}
	switch {
	case nOK > 0:
		return map[string]string{"status": "ok", "profiles": fmt.Sprintf("%d/%d", nOK, len(gw.shards))}
	case nDown > 0:
		return map[string]string{"status": "degraded", "browser": "down"}
	default:
		return map[string]string{"status": "degraded", "session": "missing"}
	}
}

// shardStatus é o retrato de um shard para o painel.
type shardStatus struct {
	Profile string `json:"profile"`
	Session string `json:"session"` // ok | busy | missing | down
	Busy    bool   `json:"busy"`
}

// shardsStatus devolve o estado individual de cada profile (painel).
func (gw *Gateway) shardsStatus() []shardStatus {
	out := make([]shardStatus, 0, len(gw.shards))
	for _, s := range gw.shards {
		st := shardStatus{Profile: s.name, Session: "ok"}
		select {
		case i := <-s.sem:
			s.sem <- i
		default:
			st.Busy = true
			st.Session = "busy"
			out = append(out, st)
			continue
		}
		if s.noSession.Load() {
			st.Session = "missing"
			out = append(out, st)
			continue
		}
		s.mu.Lock()
		alive := s.browser != nil && s.browser.Alive()
		s.mu.Unlock()
		if !alive {
			st.Session = "down"
		}
		out = append(out, st)
	}
	return out
}

// firstShardNeedingLogin: o profile default do botão de login do painel.
func (gw *Gateway) firstShardNeedingLogin() int {
	for i, s := range gw.shards {
		if s.noSession.Load() {
			return i
		}
	}
	return 0
}

// close encerra os browsers supervisionados.
func (gw *Gateway) close() {
	for _, s := range gw.shards {
		s.mu.Lock()
		if s.browser != nil {
			s.browser.Close()
			s.browser = nil
		}
		s.mu.Unlock()
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

// TriggerLogin inicia o fluxo de login interativo do shard idx a partir do
// painel. Os DEMAIS shards seguem servindo — só o profile em login sai do
// ar (o browser de login detém o user-data-dir dele).
func (gw *Gateway) TriggerLogin(idx int) bool {
	if idx < 0 || idx >= len(gw.shards) {
		return false
	}
	// ChatGPT: o login pelo painel abre Chrome com CDP anexado — o
	// Cloudflare Turnstile do chatgpt.com REJEITA (desafio falha no clique).
	// O caminho que passa é o login EXTERNO do `bifrost login` (Chrome
	// limpo, sem automação); orienta em vez de abrir a janela que falharia.
	if gw.factory.Name() == "chatgpt" {
		gw.setLoginStatus(LoginStatus{
			Done:    true,
			Ok:      false,
			Profile: gw.shards[idx].name,
			Message: "ChatGPT: use `bifrost login` no host — o Cloudflare rejeita o login em Chrome automatizado (janela externa, sem CDP)",
		})
		return true
	}
	if !gw.loginActive.CompareAndSwap(false, true) {
		return false // já em andamento
	}
	gw.loginShard = idx
	s := gw.shards[idx]
	go func() {
		defer gw.loginActive.Store(false)

		gw.setLoginStatus(LoginStatus{Active: true, Profile: s.name, Message: "Fechando browser atual (" + s.name + ")…"})

		s.mu.Lock()
		if s.browser != nil {
			s.browser.Close()
			s.browser = nil
			for i := range s.workers {
				s.workers[i] = nil
			}
		}
		s.mu.Unlock()

		gw.setLoginStatus(LoginStatus{Active: true, Profile: s.name, Message: "Abrindo Chrome para login (" + s.name + ")…"})

		cfg := s.cfg
		cfg.Headless = false
		b, err := StartBrowser(gw.ctx, cfg)
		if err != nil {
			slog.Error("login: falhou ao abrir browser", "profile", s.name, "err", err)
			gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Profile: s.name, Message: "Erro ao abrir o Chrome: " + err.Error()})
			return
		}

		if err := gw.factory.Open(b.BootCtx); err != nil {
			b.Close()
			slog.Error("login: falhou ao abrir provedor", "profile", s.name, "err", err)
			gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Profile: s.name, Message: "Erro ao navegar ao provedor: " + err.Error()})
			return
		}

		gw.setLoginStatus(LoginStatus{Active: true, Profile: s.name, Message: "Aguardando login na janela do Chrome (" + s.name + ")…"})
		slog.Info("login: janela aberta, aguardando login do usuário", "profile", s.name)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		timeout := time.NewTimer(5 * time.Minute)
		defer timeout.Stop()

		for {
			select {
			case <-gw.ctx.Done():
				b.Close()
				gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Profile: s.name, Message: "Servidor encerrado durante o login"})
				return
			case <-b.BootCtx.Done():
				gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Profile: s.name, Message: "Janela do Chrome fechada antes do login ser confirmado"})
				return
			case <-timeout.C:
				b.Close()
				gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Profile: s.name, Message: "Timeout: login não completado em 5 minutos"})
				return
			case <-ticker.C:
				// probe com teto: página de login engasgada não pode
				// bloquear o loop (o timeout de 5 minutos precisa seguir
				// alcançável)
				pctx, pcancel := context.WithTimeout(b.BootCtx, stateProbeTimeout)
				state, _, err := gw.factory.State(pctx)
				pcancel()
				if err != nil {
					continue
				}
				if state == stateLoggedIn {
					slog.Info("login: sessão confirmada", "profile", s.name)
					gw.setLoginStatus(LoginStatus{Active: true, Profile: s.name, Message: "Sessão confirmada! Gravando e restaurando o gateway…"})

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
					// headed por natureza e, se o usuário fechasse a janela,
					// instalaríamos um browser morto no shard. Close gracioso +
					// relança com a config normal (respeita BIFROST_HEADLESS).
					b.Close()

					s.mu.Lock()
					rerr := gw.recoverShardLocked(s)
					s.mu.Unlock()
					if rerr != nil {
						slog.Error("login: falhou ao restaurar shard", "profile", s.name, "err", rerr)
						gw.setLoginStatus(LoginStatus{Done: true, Ok: false, Profile: s.name, Message: "Sessão ok, mas falhou ao restaurar o browser: " + rerr.Error()})
						return
					}
					s.noSession.Store(false)
					gw.setLoginStatus(LoginStatus{Done: true, Ok: true, Profile: s.name, Message: "Login concluído: " + s.name})
					slog.Info("login: shard restaurado com nova sessão", "profile", s.name, "pool_size", len(s.workers))
					return
				}
			}
		}
	}()
	return true
}
