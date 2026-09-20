package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func newTestGateway() *Gateway {
	return NewGateway(context.Background(), Config{PoolSize: 1}, &geminiFactory{})
}

// blockingFactory: State pendura até o contexto morrer — simula a página
// engasgada do incidente 2026-09-20 (Evaluate preso no main thread).
type blockingFactory struct {
	geminiFactory
}

func (f *blockingFactory) State(ctx context.Context) (pageState, string, error) {
	<-ctx.Done()
	return stateUnknown, "", ctx.Err()
}

// TestProbeStateTimesOutInsteadOfHanging: sem o teto, o probe de status
// pendurava /health e /panel/data JUNTO com a página engasgada (dur=2m44s
// no log do incidente). Com o teto, falha rápido e o status segue.
func TestProbeStateTimesOutInsteadOfHanging(t *testing.T) {
	old := stateProbeTimeout
	stateProbeTimeout = 50 * time.Millisecond
	defer func() { stateProbeTimeout = old }()

	gw := &Gateway{factory: &blockingFactory{}}
	b := &Browser{BootCtx: context.Background()}
	start := time.Now()
	_, err := gw.probeState(b)
	if err == nil {
		t.Fatal("probe com página engasgada deveria devolver erro")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("probe pendurou %v; deveria falhar no teto curto (~50ms)", elapsed)
	}
}

func TestStatusDuringPanelLogin(t *testing.T) {
	gw := newTestGateway()

	gw.loginActive.Store(true)
	st := gw.status()
	if st["status"] != "login" {
		t.Errorf("status durante login = %q; want %q", st["status"], "login")
	}
	if st["browser"] != "up" {
		t.Errorf("browser durante login = %q; want up (browser de login está de pé)", st["browser"])
	}
}

func TestAcquireDuringPanelLogin(t *testing.T) {
	gw := newTestGateway()
	gw.loginActive.Store(true) // login em andamento: browser de produção desmontado

	_, _, _, err := gw.acquire(context.Background(), "")
	if err == nil {
		t.Fatal("acquire durante login deveria falhar com erro claro")
	}
	if !strings.Contains(err.Error(), "login em andamento") {
		t.Errorf("erro confuso durante login: %v", err)
	}
}

func TestAcquireAllShardsNoSession(t *testing.T) {
	gw := newTestGateway()
	for _, s := range gw.shards {
		s.noSession.Store(true)
	}
	_, _, _, err := gw.acquire(context.Background(), "")
	if err == nil {
		t.Fatal("acquire com todos os shards sem sessão deveria falhar (não hang)")
	}
	if !strings.Contains(err.Error(), "nenhuma sessão") {
		t.Errorf("esperava erro de sessão, veio: %v", err)
	}
}

func TestStatusAllNoSession(t *testing.T) {
	gw := newTestGateway()
	for _, s := range gw.shards {
		s.noSession.Store(true)
	}
	st := gw.status()
	if st["status"] != "degraded" || st["session"] != "missing" {
		t.Errorf("status com todos sem sessão = %v; want degraded/session missing", st)
	}
}

func TestConversationAffinityBinding(t *testing.T) {
	gw := newTestGateway() // 1 shard; a afinidade é testável pelo mapa
	key := "abc123"
	if _, ok := gw.convLookup(key); ok {
		t.Fatal("chave desconhecida não deveria ter afinidade")
	}
	gw.convBind(key, 0)
	if idx, ok := gw.convLookup(key); !ok || idx != 0 {
		t.Errorf("afinidade registrada não recuperada: %d %v", idx, ok)
	}
	gw.convBind("", 0) // chave vazia: no-op, não deve panicar
}

func TestConversationKeyStableAcrossTurns(t *testing.T) {
	turn1 := []Message{
		{Role: "system", Content: "sys do agente"},
		{Role: "user", Content: "crie a landing page"},
	}
	turn2 := []Message{
		{Role: "system", Content: "sys do agente"},
		{Role: "user", Content: "crie a landing page"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "agora adicione um hero"},
		{Role: "system", Content: "PROTOCOLO", Control: true},
	}
	if conversationKey(turn1) != conversationKey(turn2) {
		t.Error("chave deveria ser estável entre turnos do mesmo agente")
	}
	other := []Message{
		{Role: "system", Content: "sys do agente"},
		{Role: "user", Content: "outra conversa qualquer"},
	}
	if conversationKey(turn1) == conversationKey(other) {
		t.Error("conversas diferentes deveriam ter chaves diferentes")
	}
}

func TestLoadConfigProfiles(t *testing.T) {
	t.Setenv("BIFROST_PROFILES", " /data/p1 , /data/p2 ,, /data/p3 ")
	cfg := LoadConfig()
	if len(cfg.Profiles) != 3 {
		t.Fatalf("esperava 3 profiles, veio %d: %v", len(cfg.Profiles), cfg.Profiles)
	}
	if cfg.Profiles[0] != "/data/p1" || cfg.Profile != "/data/p1" {
		t.Errorf("Profile deveria sincronizar com o primeiro: %q / %q", cfg.Profile, cfg.Profiles[0])
	}
}

func TestNewGatewayDedupesProfiles(t *testing.T) {
	gw := NewGateway(context.Background(), Config{
		Profiles: []string{"/a", "/a", "/b"},
		PoolSize: 1,
	}, &geminiFactory{})
	if len(gw.shards) != 2 {
		t.Fatalf("profiles duplicados deveriam colapsar: %d shards", len(gw.shards))
	}
	if gw.shards[0].name != "a" || gw.shards[1].name != "b" {
		t.Errorf("nomes de shard errados: %s, %s", gw.shards[0].name, gw.shards[1].name)
	}
}

func TestSnapshotIncludesLoginStatus(t *testing.T) {
	gw := newTestGateway()
	gw.loginActive.Store(true)
	gw.setLoginStatus(LoginStatus{Active: true, Profile: "chrome-profile", Message: "Aguardando login na janela do Chrome…"})

	snap := thePanel.snapshot(gw)
	lg, ok := snap["login"].(LoginStatus)
	if !ok {
		t.Fatalf("snapshot sem campo login utilizável: %T", snap["login"])
	}
	if !lg.Active || lg.Profile != "chrome-profile" {
		t.Errorf("estado do login não propagado ao painel: %+v", lg)
	}
	if _, ok := snap["shards"]; !ok {
		t.Error("snapshot sem lista de shards")
	}
}
