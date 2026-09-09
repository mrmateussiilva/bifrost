package main

import (
	"context"
	"strings"
	"testing"
)

func newTestGateway() *Gateway {
	return NewGateway(context.Background(), Config{PoolSize: 1}, &geminiFactory{})
}

func TestStatusDuringPanelLogin(t *testing.T) {
	gw := newTestGateway()
	if got := gw.status()["status"]; got != "degraded" && got != "ok" {
		t.Logf("status sem browser: %q (degraded esperado)", got)
	}

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

	_, _, err := gw.acquire(context.Background())
	if err == nil {
		t.Fatal("acquire durante login deveria falhar com erro claro")
	}
	if !strings.Contains(err.Error(), "login em andamento") {
		t.Errorf("erro confuso durante login: %v", err)
	}
}

func TestSnapshotIncludesLoginStatus(t *testing.T) {
	gw := newTestGateway()
	gw.loginActive.Store(true)
	gw.setLoginStatus(LoginStatus{Active: true, Message: "Aguardando login na janela aberta…"})

	snap := thePanel.snapshot(gw)
	lg, ok := snap["login"].(LoginStatus)
	if !ok {
		t.Fatalf("snapshot sem campo login utilizável: %T", snap["login"])
	}
	if !lg.Active || lg.Message != "Aguardando login na janela aberta…" {
		t.Errorf("estado do login não propagado ao painel: %+v", lg)
	}
}
