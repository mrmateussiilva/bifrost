package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Painel de observação (estilo Evolution API): estado do gateway, request
// em execução, histórico recente e logs — tudo em MEMÓRIA, sem
// dependências. O HTML é embutido no binário e consulta /panel/data; é o
// primeiro frontend do projeto (o MVP bania frontend por escopo — mantido
// server-rendered e zero-dep por espírito).
//
// Rastros de requests incluem preview do prompt: quando BIFROST_API_KEY
// está definida, /panel fica atrás da mesma autenticação do resto.

const (
	panelMaxRequests = 50
	panelMaxLogs     = 120
)

// reqRecord é o rastro de uma requisição admitida (pós-fila).
type reqRecord struct {
	ID           string    `json:"id"`
	Started      time.Time `json:"started"`
	Model        string    `json:"model"`
	Stream       bool      `json:"stream"`
	Tools        int       `json:"tools"`
	Prompt       string    `json:"prompt"`
	Status       string    `json:"status"`
	ToolCalls    int       `json:"tool_calls"`
	Chars        int       `json:"chars"`
	Retries      int       `json:"retries"`
	Duration     float64   `json:"duration_s"`
	FullPrompt   string    `json:"full_prompt,omitempty"`
	FullResponse string    `json:"full_response,omitempty"`
}

type panelCounters struct {
	Requests  int `json:"requests"`
	Errors    int `json:"errors"`
	Rejected  int `json:"rejected"`
	Retries   int `json:"refusal_retries"`
	ToolCalls int `json:"tool_calls"`
}

type panelData struct {
	mu       sync.Mutex
	started  time.Time
	requests []reqRecord // mais recente no fim
	logs     []string
	active   *reqRecord
	counters panelCounters
}

var thePanel = &panelData{started: time.Now()}

// begin registra o início de um request admitido e o marca como ativo.
func (p *panelData) begin(model string, stream bool, tools int, prompt string) *reqRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec := &reqRecord{
		ID:      time.Now().Format("15:04:05.000"),
		Started: time.Now(),
		Model:   model,
		Stream:  stream,
		Tools:   tools,
		Prompt:  prompt,
	}
	p.active = rec
	return rec
}

// end finaliza o rastro (status vazio = erro) e o move ao histórico.
func (p *panelData) end(rec *reqRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == rec {
		p.active = nil
	}
	rec.Duration = time.Since(rec.Started).Seconds()
	if rec.Status == "" {
		rec.Status = "erro"
	}
	if rec.Status != "ok" && rec.Status != "tool_calls" {
		p.counters.Errors++
	}
	p.counters.Requests++
	p.counters.ToolCalls += rec.ToolCalls
	p.requests = append(p.requests, *rec)
	if len(p.requests) > panelMaxRequests {
		p.requests = p.requests[len(p.requests)-panelMaxRequests:]
	}
}

// mutate altera campos do rastro sob lock (o snapshot lê concorrentemente).
func (p *panelData) mutate(rec *reqRecord, f func(*reqRecord)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(rec)
}

// addChars acumula caracteres emitidos no stream (progresso ao vivo).
func (p *panelData) addChars(rec *reqRecord, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec.Chars += n
}

// noteRetry conta uma retratativa de recusa de ferramenta.
func (p *panelData) noteRetry(rec *reqRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec.Retries++
	p.counters.Retries++
}

// reject conta um request barrado antes de executar (fila cheia, timeout).
func (p *panelData) reject() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counters.Rejected++
}

func (p *panelData) pushLog(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logs = append(p.logs, line)
	if len(p.logs) > panelMaxLogs {
		p.logs = p.logs[len(p.logs)-panelMaxLogs:]
	}
}

// ---------------------------------------------------------------------------
// Tee do slog: as mesmas linhas do stderr alimentam o painel.
// ---------------------------------------------------------------------------

type teeHandler struct {
	inner slog.Handler
	panel *panelData
	opts  *slog.HandlerOptions
}

func (p *panelData) logTee(inner slog.Handler, lvl slog.Level) slog.Handler {
	return &teeHandler{inner: inner, panel: p, opts: &slog.HandlerOptions{Level: lvl}}
}

func (h *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var b bytes.Buffer
	_ = slog.NewTextHandler(&b, h.opts).Handle(ctx, r.Clone())
	h.panel.pushLog(strings.TrimRight(b.String(), "\n"))
	return h.inner.Handle(ctx, r)
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{inner: h.inner.WithAttrs(attrs), panel: h.panel, opts: h.opts}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{inner: h.inner.WithGroup(name), panel: h.panel, opts: h.opts}
}

// ---------------------------------------------------------------------------
// Handlers HTTP do painel.
// ---------------------------------------------------------------------------

// promptPreview comprime o prompt serializado para exibição.
func promptPreview(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func handlePanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(panelHTML))
}

func handlePanelData(gw *Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, thePanel.snapshot(gw))
	}
}

// handlePanelLogin inicia o fluxo de login interativo (POST /panel/login).
func handlePanelLogin(gw *Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !gw.TriggerLogin() {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "login já em andamento",
			})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "iniciado",
		})
	}
}

// handlePanelLoginStatus devolve o estado atual do fluxo de login (GET /panel/login/status).
func handlePanelLoginStatus(gw *Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, gw.GetLoginStatus())
	}
}

// snapshot monta o estado completo do painel. gw.status() roda FORA do
// lock do painel (pode avaliar o DOM quando ocioso).
func (p *panelData) snapshot(gw *Gateway) map[string]any {
	st := gw.status()
	queueCap := 1 + queuedRequests
	login := gw.GetLoginStatus()

	p.mu.Lock()
	defer p.mu.Unlock()

	browser, session := "up", "ok"
	busy := st["busy"] == "true"
	if st["browser"] == "down" {
		browser = "down"
	}
	if st["session"] == "missing" {
		session = "missing"
	}

	out := map[string]any{
		"status":   st["status"],
		"busy":     busy,
		"browser":  browser,
		"session":  session,
		"login":    login,
		"queue":    map[string]int{"depth": gw.queueDepth(), "cap": queueCap},
		"uptime_s": int(time.Since(p.started).Seconds()),
		"counters": p.counters,
		"requests": func() []reqRecord {
			n := len(p.requests)
			if n == 0 {
				return []reqRecord{}
			}
			rev := make([]reqRecord, n)
			for i, rrec := range p.requests {
				rev[n-1-i] = rrec
			}
			return rev
		}(),
		"logs": p.logs,
	}
	if p.active != nil {
		a := *p.active
		out["active"] = map[string]any{
			"id":        a.ID,
			"model":     a.Model,
			"stream":    a.Stream,
			"tools":     a.Tools,
			"prompt":    a.Prompt,
			"chars":     a.Chars,
			"retries":   a.Retries,
			"elapsed_s": time.Since(a.Started).Seconds(),
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// HTML embutido — dark polido, sem dependências, poll de 3s em /panel/data.
// Cards com ícones SVG, histórico expandível (prompt+resposta), sparkline.
// ---------------------------------------------------------------------------

const panelHTML = `<!doctype html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Bifrost — painel</title>
<style>
@import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&display=swap');
:root{
  --bg: #07090f;
  --surface: rgba(20, 24, 38, 0.45);
  --surface2: rgba(30, 35, 55, 0.65);
  --border: rgba(255, 255, 255, 0.08);
  --border-hover: rgba(255, 255, 255, 0.16);
  --tx: #f0f2f8; --mut: #8b95a8; --mut2: #657085;
  --ok: #4ade80; --ok-bg: rgba(74, 222, 128, 0.15);
  --err: #f87171; --err-bg: rgba(248, 113, 113, 0.15);
  --warn: #fbbf24; --warn-bg: rgba(251, 191, 36, 0.15);
  --acc: #60a5fa; --acc-bg: rgba(96, 165, 250, 0.15);
  --purple: #c084fc; --purple-bg: rgba(192, 132, 252, 0.15);
  --radius: 12px; --radius-lg: 16px;
}
*{box-sizing:border-box;margin:0;padding:0}
body{
  background: var(--bg);
  background-image: radial-gradient(circle at 15% 10%, rgba(96,165,250,0.06) 0%, transparent 40%),
                    radial-gradient(circle at 85% 90%, rgba(192,132,252,0.06) 0%, transparent 40%);
  background-attachment: fixed;
  color: var(--tx);
  font: 13.5px/1.6 'Inter', -apple-system, sans-serif;
  padding: 32px; max-width: 1200px; margin: 0 auto;
}

/* ── header ── */
.header{display:flex;align-items:center;gap:12px;margin-bottom:24px;flex-wrap:wrap}
.header h1{font-size:20px;font-weight:700;letter-spacing:-.3px}
.header .subtitle{color:var(--mut);font-size:12.5px;margin-left:auto}
.badge{display:inline-flex;align-items:center;gap:4px;padding:3px 10px;border-radius:20px;
  font-size:11.5px;font-weight:600;white-space:nowrap}
.b-ok{background:var(--ok-bg);color:var(--ok)}
.b-err{background:var(--err-bg);color:var(--err)}
.b-warn{background:var(--warn-bg);color:var(--warn)}
.b-acc{background:var(--acc-bg);color:var(--acc)}
.b-purple{background:var(--purple-bg);color:var(--purple)}
.dot{width:7px;height:7px;border-radius:50%;background:currentColor;flex-shrink:0}
.pulse .dot{animation:pulse 1.6s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.3}}

/* ── status cards ── */
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:14px;margin-bottom:28px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius-lg);padding:18px 20px;
  display:flex;flex-direction:column;gap:6px;transition:all .25s ease;
  backdrop-filter:blur(16px);-webkit-backdrop-filter:blur(16px);
  box-shadow: 0 4px 24px rgba(0,0,0,0.1)}
.card:hover{border-color:var(--border-hover);transform:translateY(-3px);box-shadow:0 8px 32px rgba(0,0,0,0.2)}
.card-icon{width:34px;height:34px;border-radius:10px;display:flex;align-items:center;justify-content:center;margin-bottom:6px;
  background: rgba(255,255,255,0.03); border: 1px solid rgba(255,255,255,0.05)}
.card-icon svg{width:18px;height:18px}
.card-label{font-size:11px;color:var(--mut);text-transform:uppercase;letter-spacing:.08em;font-weight:600}
.card-value{font-size:24px;font-weight:700;line-height:1.1;letter-spacing:-0.5px}
.card-sub{font-size:11.5px;color:var(--mut);margin-top:2px}

/* ── active request ── */
.active-box{background:var(--surface);border:1px solid var(--acc);border-radius:var(--radius-lg);
  padding:20px 24px;margin-bottom:28px;
  backdrop-filter:blur(16px);-webkit-backdrop-filter:blur(16px);
  box-shadow: 0 0 0 1px rgba(96,165,250,0.1), 0 8px 32px rgba(96,165,250,0.08)}
.active-box.idle{border-color:var(--border);opacity:.8;box-shadow:none}
.active-header{display:flex;align-items:center;gap:8px;flex-wrap:wrap;margin-bottom:10px}
.active-title{font-weight:600;font-size:14px;letter-spacing:-0.2px}
.active-meta{color:var(--mut);font-size:12px;margin-top:2px}
.active-prompt{color:var(--mut);font-size:12.5px;font-style:italic;margin-top:10px;
  padding:12px 14px;background:var(--surface2);border-radius:8px;word-break:break-word;
  border: 1px solid rgba(255,255,255,0.03)}
.progress-bar{height:3px;background:var(--border);border-radius:2px;margin-top:10px;overflow:hidden}
.progress-fill{height:100%;background:var(--acc);border-radius:2px;
  animation:shimmer 1.5s ease-in-out infinite;background-size:200% 100%;
  background-image:linear-gradient(90deg,var(--acc) 0%,#a5c8ff 50%,var(--acc) 100%)}
@keyframes shimmer{0%{background-position:200% 0}100%{background-position:-200% 0}}

/* ── section headers ── */
section{margin-bottom:28px}
.section-title{font-size:11px;color:var(--mut);text-transform:uppercase;letter-spacing:.07em;
  font-weight:600;margin-bottom:10px;display:flex;align-items:center;gap:8px}
.section-title::after{content:'';flex:1;height:1px;background:var(--border)}

/* ── history table ── */
.table-wrap{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius-lg);overflow:hidden;
  backdrop-filter:blur(16px);-webkit-backdrop-filter:blur(16px);box-shadow: 0 4px 24px rgba(0,0,0,0.1)}
table{width:100%;border-collapse:collapse;font-size:12.5px}
thead{background:rgba(0,0,0,0.2)}
th{text-align:left;padding:12px 16px;color:var(--mut);font-size:11px;text-transform:uppercase;
  letter-spacing:.08em;font-weight:600;border-bottom:1px solid var(--border)}
tbody tr{border-bottom:1px solid rgba(255,255,255,0.04);cursor:pointer;transition:background .2s}
tbody tr:last-child{border-bottom:none}
tbody tr:hover{background:rgba(255,255,255,0.03)}
tbody tr.expanded{background:rgba(255,255,255,0.04)}
td{padding:10px 16px;vertical-align:middle}
.mono{font-family:ui-monospace,"SF Mono",Menlo,monospace;font-size:11.5px}
.model-name{font-weight:500}
.chevron{transition:transform .25s ease;display:inline-block;color:var(--mut);font-size:10px}
.chevron.open{transform:rotate(90deg)}
.detail-row td{padding:0}
.detail-inner{padding:16px 20px;background:rgba(0,0,0,0.2);border-top:1px solid var(--border)}
.detail-grid{display:grid;grid-template-columns:1fr 1fr;gap:16px}
@media(max-width:700px){.detail-grid{grid-template-columns:1fr}}
.detail-label{font-size:10.5px;color:var(--mut);text-transform:uppercase;letter-spacing:.08em;
  font-weight:600;margin-bottom:8px}
.detail-content{background:var(--surface2);border:1px solid var(--border);border-radius:8px;
  padding:12px 14px;font:12px/1.6 ui-monospace,"SF Mono",Menlo,monospace;
  max-height:240px;overflow:auto;white-space:pre-wrap;word-break:break-all;color:var(--tx)}
.detail-content.empty{color:var(--mut);font-style:italic}

/* ── sparkline ── */
.spark{display:inline-block;vertical-align:middle}

/* ── logs ── */
.log-box{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius-lg);
  padding:14px 16px;font:11.5px/1.7 ui-monospace,"SF Mono",Menlo,monospace;
  max-height:280px;overflow:auto;backdrop-filter:blur(16px);-webkit-backdrop-filter:blur(16px);
  box-shadow: 0 4px 24px rgba(0,0,0,0.1)}
.log-line{white-space:pre-wrap;word-break:break-all}
.log-debug{color:var(--mut2)}
.log-info{color:var(--tx)}
.log-warn{color:var(--warn)}
.log-error{color:var(--err)}

/* ── counters row ── */
.counters{display:flex;gap:24px;flex-wrap:wrap;padding:14px 20px;
  background:var(--surface);border-radius:var(--radius-lg);border:1px solid var(--border);margin-bottom:20px;
  backdrop-filter:blur(16px);-webkit-backdrop-filter:blur(16px);box-shadow: 0 4px 24px rgba(0,0,0,0.1)}
.cnt{display:flex;align-items:baseline;gap:6px}
.cnt-val{font-size:20px;font-weight:700;letter-spacing:-0.5px}
.cnt-lbl{font-size:11.5px;color:var(--mut);text-transform:uppercase;letter-spacing:.05em;font-weight:500}
.cnt-err .cnt-val{color:var(--err);text-shadow:0 0 12px rgba(248,113,113,0.3)}
.cnt-warn .cnt-val{color:var(--warn);text-shadow:0 0 12px rgba(251,191,36,0.3)}
.cnt-acc .cnt-val{color:var(--purple);text-shadow:0 0 12px rgba(192,132,252,0.3)}

/* scrollbar */
::-webkit-scrollbar{width:6px;height:6px}
::-webkit-scrollbar-track{background:transparent}
::-webkit-scrollbar-thumb{background:var(--border);border-radius:3px}
</style>
</head>
<body>

<div class="header">
  <h1>
    <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
         style="vertical-align:-4px;margin-right:6px;color:var(--acc)">
      <path d="M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z"/>
      <line x1="4" y1="22" x2="4" y2="15"/>
    </svg>Bifrost
  </h1>
  <span class="badge" id="st">…</span>
  <span class="badge b-acc pulse" id="busyBadge" style="display:none">
    <span class="dot"></span>gerando
  </span>
  <span class="subtitle" id="subtitle">gateway OpenAI-compatible · uptime —</span>
  <button id="hdrLogin" onclick="startLogin()" title="Abrir janela de login e criar/reconectar a sessão"
    style="background:var(--surface2);color:var(--tx);border:1px solid var(--border);border-radius:8px;
           padding:6px 14px;font-weight:600;font-size:12px;cursor:pointer">
    Nova sessão
  </button>
</div>

<div class="cards" id="cards"></div>

<div id="activeBox"></div>

<div id="loginBanner" style="display:none;background:var(--warn-bg);border:1px solid var(--warn);border-radius:var(--radius-lg);padding:16px 20px;margin-bottom:24px">
  <div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
    <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="var(--warn)" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>
    <span style="font-weight:600;color:var(--warn)" id="loginTitle">Sessão não encontrada</span>
    <button id="loginBtn" onclick="startLogin()"
      style="margin-left:auto;background:var(--warn);color:#000;border:none;border-radius:6px;
             padding:7px 16px;font-weight:600;font-size:12.5px;cursor:pointer">
      Reconectar sessão
    </button>
  </div>
  <div id="loginStatus" style="display:none;margin-top:10px;font-size:12.5px;color:var(--tx)"></div>
</div>

<section>
  <div class="section-title">Métricas</div>
  <div class="counters" id="counters"></div>
</section>

<section>
  <div class="section-title">Requisições recentes</div>
  <div class="table-wrap">
    <table>
      <thead>
        <tr>
          <th></th>
          <th>Hora</th>
          <th>Modelo</th>
          <th>Modo</th>
          <th>Tools</th>
          <th>Status</th>
          <th>Duração</th>
          <th>Chars</th>
          <th>Barra</th>
        </tr>
      </thead>
      <tbody id="tbody"></tbody>
    </table>
  </div>
</section>

<section>
  <div class="section-title">Logs</div>
  <div class="log-box" id="logbox"></div>
</section>

<script>
const $ = id => document.getElementById(id);
const esc = s => String(s??'').replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
const dur = s => s==null?'—':(s<1?Math.round(s*1000)+'ms':s.toFixed(1)+'s');
const up  = s => {
  if(s<60) return s+'s';
  if(s<3600) return Math.floor(s/60)+'m '+String(s%60).padStart(2,'0')+'s';
  return Math.floor(s/3600)+'h '+Math.floor(s%3600/60)+'m';
};

function stBadge(st){
  if(st==='ok') return ['b-ok','ok'];
  if(st==='login') return ['b-warn pulse','login em andamento'];
  if(st==='degraded') return ['b-err','degraded'];
  return ['b-err', st||'—'];
}
function stCell(s){
  if(s==='ok')         return '<span class="badge b-ok">ok</span>';
  if(s==='tool_calls') return '<span class="badge b-purple">tool_calls</span>';
  return '<span class="badge b-err">'+esc(s)+'</span>';
}

const SVG_SESSION = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>';
const SVG_BROWSER = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="2" y="3" width="20" height="14" rx="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/></svg>';
const SVG_QUEUE   = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="8" y1="6" x2="21" y2="6"/><line x1="8" y1="12" x2="21" y2="12"/><line x1="8" y1="18" x2="21" y2="18"/><line x1="3" y1="6" x2="3.01" y2="6"/><line x1="3" y1="12" x2="3.01" y2="12"/><line x1="3" y1="18" x2="3.01" y2="18"/></svg>';
const SVG_REQS    = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/></svg>';
const SVG_UPTIME  = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>';
const SVGS = {session:SVG_SESSION, browser:SVG_BROWSER, queue:SVG_QUEUE, reqs:SVG_REQS, uptime:SVG_UPTIME};

function card(icon, iconColor, label, valueHTML, sub){
  return '<div class="card">'
    +'<div class="card-icon" style="background:'+iconColor+'22;color:'+iconColor+'">'+SVGS[icon]+'</div>'
    +'<div class="card-label">'+label+'</div>'
    +'<div class="card-value">'+valueHTML+'</div>'
    +(sub ? '<div class="card-sub">'+sub+'</div>' : '')
    +'</div>';
}

function spark(val, max, color){
  const w=60, h=14, pct = max>0 ? val/max : 0;
  const fill = Math.max(2, Math.round(pct*w));
  return '<svg class="spark" width="'+w+'" height="'+h+'" viewBox="0 0 '+w+' '+h+'">'
    +'<rect x="0" y="3" width="'+w+'" height="'+(h-6)+'" rx="3" fill="var(--border)"/>'
    +'<rect x="0" y="3" width="'+fill+'" height="'+(h-6)+'" rx="3" fill="'+color+'"/>'
    +'</svg>';
}

const expanded = new Set();
function toggleRow(id){
  const btn = document.querySelector('tr[data-id="'+id+'"] .chevron');
  const det = document.getElementById('det-'+id);
  if(!det) return;
  if(expanded.has(id)){
    expanded.delete(id);
    det.style.display='none';
    if(btn) btn.classList.remove('open');
  } else {
    expanded.add(id);
    det.style.display='';
    if(btn) btn.classList.add('open');
  }
}

function logClass(line){
  if(/level=DEBUG/i.test(line)) return 'log-debug';
  if(/level=WARN/i.test(line))  return 'log-warn';
  if(/level=ERROR/i.test(line)) return 'log-error';
  return 'log-info';
}

async function tick(){
  try{
    const d = await (await fetch('/panel/data')).json();
    const c = d.counters||{};

    const [bc,bt] = stBadge(d.status);
    $('st').className='badge '+bc;
    $('st').textContent=bt;
    $('busyBadge').style.display = d.busy ? '' : 'none';
    $('subtitle').textContent = 'gateway OpenAI-compatible · uptime '+up(d.uptime_s||0);

    const sessOk = d.session==='ok';
    const brwOk  = d.browser==='up';
    const qDepth = (d.queue&&d.queue.depth)||0;
    const qCap   = (d.queue&&d.queue.cap)||5;
    const lg     = d.login||{};

    // banner de sessão: visível quando falta sessão ou quando há login em
    // andamento (mesmo disparado por outra aba/usuário do painel)
    const banner = $('loginBanner');
    const lbtn = $('loginBtn');
    if(lg.active){
      banner.style.display='';
      $('loginTitle').textContent='Login em andamento';
      $('loginStatus').style.display='';
      $('loginStatus').innerHTML='⏳ '+esc(lg.message||'…');
      if(lbtn) lbtn.disabled=true;
    } else if(!sessOk && !loginPolling){
      banner.style.display='';
      $('loginTitle').textContent='Sessão não encontrada';
      if(lbtn) lbtn.disabled=false;
    } else if(sessOk){
      banner.style.display='none';
      if(loginPolling){ stopLoginPoll(true); }
    }
    $('cards').innerHTML = [
      card('session', lg.active?'var(--warn)':(sessOk?'var(--ok)':'var(--err)'), 'Sessão',
        lg.active
          ? '<span class="badge b-warn pulse">em login…</span>'
          : '<span class="badge '+(sessOk?'b-ok':'b-err')+'">'+(sessOk?'logada':'faltando')+'</span>'),
      card('browser', brwOk?'var(--ok)':'var(--err)', 'Browser',
        '<span class="badge '+(brwOk?'b-ok':'b-err')+'">'+(brwOk?'de pé':'caído')+'</span>'),
      card('queue', 'var(--acc)', 'Fila',
        '<span style="font-size:22px;font-weight:700">'+qDepth+'</span>',
        'de '+qCap+' slots · '+(d.busy?'ocupado':'ocioso')),
      card('reqs', 'var(--purple)', 'Requisições',
        String(c.requests||0),
        (c.errors||0)+' erro'+(c.errors!==1?'s':'')+' · '+(c.rejected||0)+' barrada'+(c.rejected!==1?'s':'')),
      card('uptime', 'var(--warn)', 'Uptime', up(d.uptime_s||0)),
    ].join('');

    $('counters').innerHTML =
      '<div class="cnt"><span class="cnt-val">'+(c.requests||0)+'</span><span class="cnt-lbl">requisições</span></div>'
      +'<div class="cnt cnt-err"><span class="cnt-val">'+(c.errors||0)+'</span><span class="cnt-lbl">erros</span></div>'
      +'<div class="cnt cnt-warn"><span class="cnt-val">'+(c.rejected||0)+'</span><span class="cnt-lbl">barradas</span></div>'
      +'<div class="cnt cnt-acc"><span class="cnt-val">'+(c.tool_calls||0)+'</span><span class="cnt-lbl">tool calls</span></div>'
      +'<div class="cnt"><span class="cnt-val">'+(c.refusal_retries||0)+'</span><span class="cnt-lbl">recusas recuperadas</span></div>';

    if(d.active){
      const a = d.active;
      $('activeBox').innerHTML = '<div class="active-box">'
        +'<div class="active-header">'
        +'<span class="active-title">Executando</span>'
        +'<span class="badge b-acc">'+esc(a.model.replace('gemini-web-',''))+'</span>'
        +'<span class="badge '+(a.stream?'b-acc':'b-purple')+'">'+(a.stream?'stream':'bloco')+'</span>'
        +(a.tools?'<span class="badge b-warn">'+a.tools+' tool'+(a.tools>1?'s':'')+'</span>':'')
        +(a.retries?'<span class="badge b-err">'+a.retries+' retratativa'+(a.retries>1?'s':'')+'</span>':'')
        +'<span style="margin-left:auto;color:var(--mut);font-size:12px">'+dur(a.elapsed_s)+' · '+a.chars+' chars</span>'
        +'</div>'
        +'<div class="active-prompt">'+esc(a.prompt)+'</div>'
        +'<div class="progress-bar"><div class="progress-fill" style="width:100%"></div></div>'
        +'</div>';
    } else {
      $('activeBox').innerHTML = '<div class="active-box idle">'
        +'<div class="active-header">'
        +'<span style="color:var(--mut);font-size:13px">Nenhum request em execução</span>'
        +'</div></div>';
    }

    const reqs = d.requests||[];
    const maxDur = reqs.reduce(function(m,r){return Math.max(m,r.duration_s||0);},0);
    const keepExp = new Set(expanded);

    $('tbody').innerHTML = reqs.map(function(r){
      const isExp = keepExp.has(r.id);
      const color = r.status==='ok'?'var(--ok)':r.status==='tool_calls'?'var(--purple)':'var(--err)';
      const pc = r.full_prompt||'';
      const rc = r.full_response||'';
      const detailRow = '<tr class="detail-row" id="det-'+r.id+'" style="display:'+(isExp?'':'none')+'">'
        +'<td colspan="9"><div class="detail-inner"><div class="detail-grid">'
        +'<div><div class="detail-label">Prompt enviado</div>'
        +'<div class="detail-content'+(pc?'':' empty')+'">'+(pc?esc(pc):'(não disponível)')+'</div></div>'
        +'<div><div class="detail-label">Resposta do Gemini</div>'
        +'<div class="detail-content'+(rc?'':' empty')+'">'+(rc?esc(rc):'(não disponível)')+'</div></div>'
        +'</div></div></td></tr>';
      return '<tr data-id="'+r.id+'" onclick="toggleRow(\''+r.id+'\')" class="'+(isExp?'expanded':'')+'">'
        +'<td><span class="chevron '+(isExp?'open':'')+'">&#9658;</span></td>'
        +'<td class="mono">'+esc(r.id)+'</td>'
        +'<td class="model-name">'+esc(r.model.replace('gemini-web-',''))+'</td>'
        +'<td><span class="badge '+(r.stream?'b-acc':'b-purple')+'" style="font-size:10.5px">'+(r.stream?'stream':'bloco')+'</span></td>'
        +'<td>'+(r.tools||'—')+'</td>'
        +'<td>'+stCell(r.status)+'</td>'
        +'<td class="mono">'+dur(r.duration_s)+'</td>'
        +'<td>'+(r.chars||'—')+'</td>'
        +'<td>'+spark(r.duration_s||0, maxDur, color)+'</td>'
        +'</tr>'+detailRow;
    }).join('') || '<tr><td colspan="9" style="color:var(--mut);text-align:center;padding:20px">nenhuma ainda</td></tr>';

    $('logbox').innerHTML = (d.logs||[]).map(function(l){
      return '<div class="log-line '+logClass(l)+'">'+esc(l)+'</div>';
    }).join('') || '<div class="log-line log-debug">nenhum log ainda</div>';

    const lb = $('logbox');
    lb.scrollTop = lb.scrollHeight;

  } catch(e){
    $('st').className='badge b-err';
    $('st').textContent='offline';
  }
}
let loginPolling = null;

function stopLoginPoll(success){
  if(loginPolling){ clearInterval(loginPolling); loginPolling=null; }
  const btn = $('loginBtn');
  const st  = $('loginStatus');
  if(btn) btn.disabled=false;
  if(success){
    st.style.display='none';
    $('loginBanner').style.display='none';
  }
}

async function startLogin(){
  const btn = $('loginBtn');
  const st  = $('loginStatus');
  // visível mesmo com sessão ok: login proativo pelo botão do header
  $('loginBanner').style.display='';
  $('loginTitle').textContent='Conectar sessão';
  if(btn) btn.disabled=true;
  st.style.display='';
  st.innerHTML='<span style="color:var(--acc)">⏳ Iniciando…</span>';

  try{
    const r = await fetch('/panel/login', {method:'POST'});
    if(r.status===409){
      st.innerHTML='<span style="color:var(--warn)">Login já em andamento</span>';
      if(btn) btn.disabled=false;
      return;
    }
    if(!r.ok){ throw new Error('HTTP '+r.status); }
  } catch(e){
    st.innerHTML='<span style="color:var(--err)">Erro ao iniciar: '+esc(String(e))+'</span>';
    if(btn) btn.disabled=false;
    return;
  }

  // poll /panel/login/status a cada 1.5s
  loginPolling = setInterval(async function(){
    try{
      const s = await (await fetch('/panel/login/status')).json();
      const icon = s.done ? (s.ok ? '✅' : '❌') : '⏳';
      st.innerHTML = icon+' '+esc(s.message||'…');
      if(s.done){
        stopLoginPoll(s.ok);
        if(!s.ok){
          const btn2 = $('loginBtn');
          if(btn2) btn2.disabled=false;
        }
      }
    } catch(e){ /* ignora falha de rede temporária */ }
  }, 1500);
}

setInterval(tick, 3000);
tick();
</script>
</body></html>`
