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
	ID        string    `json:"id"`
	Started   time.Time `json:"started"`
	Model     string    `json:"model"`
	Stream    bool      `json:"stream"`
	Tools     int       `json:"tools"`
	Prompt    string    `json:"prompt"`
	Status    string    `json:"status"`
	ToolCalls int       `json:"tool_calls"`
	Chars     int       `json:"chars"`
	Retries   int       `json:"retries"`
	Duration  float64   `json:"duration_s"`
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

// snapshot monta o estado completo do painel. gw.status() roda FORA do
// lock do painel (pode avaliar o DOM quando ocioso).
func (p *panelData) snapshot(gw *Gateway) map[string]any {
	st := gw.status()
	queueCap := 1 + queuedRequests

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
// HTML embutido — dark, sem dependências, poll de 3s em /panel/data.
// ---------------------------------------------------------------------------

const panelHTML = `<!doctype html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Bifrost — painel</title>
<style>
:root{--bg:#0e1116;--card:#161b22;--border:#2d333b;--tx:#c9d1d9;--mut:#8b949e;
--ok:#3fb950;--err:#f85149;--warn:#d29922;--acc:#58a6ff}
*{box-sizing:border-box;margin:0}
body{background:var(--bg);color:var(--tx);font:14px/1.5 system-ui,sans-serif;
padding:20px;max-width:1100px;margin:0 auto}
h1{font-size:18px;margin-bottom:2px}.sub{color:var(--mut);font-size:12px}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(145px,1fr));
gap:10px;margin:18px 0}
.card{background:var(--card);border:1px solid var(--border);border-radius:8px;
padding:10px 12px}
.card .k{color:var(--mut);font-size:10.5px;text-transform:uppercase;letter-spacing:.05em}
.card .v{font-size:17px;font-weight:600;margin-top:2px}
.badge{display:inline-block;padding:1px 8px;border-radius:10px;font-size:11px;
font-weight:600;vertical-align:2px}
.b-ok{background:#1b4721;color:var(--ok)}.b-err{background:#4a1e1e;color:var(--err)}
.b-warn{background:#4a3a12;color:var(--warn)}.b-info{background:#12283f;color:var(--acc)}
.active{background:var(--card);border:1px solid var(--acc);border-radius:8px;
padding:12px 14px;margin:12px 0}
.active .t{font-weight:600}.active .meta{color:var(--mut);font-size:12px;margin-top:2px}
.pv{color:var(--mut);font-style:italic;margin-top:4px;font-size:12.5px}
table{width:100%;border-collapse:collapse;font-size:12.5px}
th,td{text-align:left;padding:5px 8px;border-bottom:1px solid var(--border)}
th{color:var(--mut);font-size:10.5px;text-transform:uppercase}
.mono{font-family:ui-monospace,monospace}
section h2{font-size:12px;color:var(--mut);margin:18px 0 6px;text-transform:uppercase;
letter-spacing:.05em}
pre{background:var(--card);border:1px solid var(--border);border-radius:8px;
padding:10px;font:11.5px/1.5 ui-monospace,monospace;overflow:auto;max-height:260px;
white-space:pre-wrap;word-break:break-all}
</style>
</head>
<body>
<h1>Bifrost <span class="badge" id="st">…</span> <span class="badge b-info" id="busy" style="display:none">gerando</span></h1>
<div class="sub" id="sub">gateway OpenAI-compatible sobre o Gemini Web · uptime —</div>
<div class="cards" id="cards"></div>
<div id="activeBox"></div>
<section><h2>Requisições recentes</h2>
<table><thead><tr><th>hora</th><th>modelo</th><th>modo</th><th>tools</th>
<th>chamadas</th><th>status</th><th>dur</th><th>chars</th></tr></thead>
<tbody id="tbody"></tbody></table></section>
<section><h2>Logs</h2><pre id="logs">…</pre></section>
<script>
const el=id=>document.getElementById(id);
const esc=s=>String(s??'').replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
const dur=s=>s==null?'—':(s<1?Math.round(s*1000)+'ms':s.toFixed(1)+'s');
const up=s=>s<60?s+'s':s<3600?Math.floor(s/60)+'m '+(s%60)+'s':Math.floor(s/3600)+'h '+Math.floor(s%3600/60)+'m';
function stBadge(st){
 if(st==='ok')return['b-ok','ok'];
 if(st==='degraded')return['b-err','degraded'];
 return['b-err',st||'—'];
}
function stCell(s){
 if(s==='ok')return'<span class="badge b-ok">ok</span>';
 if(s==='tool_calls')return'<span class="badge b-info">tool_calls</span>';
 return'<span class="badge b-err">'+esc(s)+'</span>';
}
async function tick(){
 try{
  const d=await(await fetch('data')).json();
  const[bc,bt]=stBadge(d.status);
  el('st').className='badge '+bc;el('st').textContent=bt;
  el('busy').style.display=d.busy?'':'none';
  el('sub').textContent='gateway OpenAI-compatible sobre o Gemini Web · uptime '+up(d.uptime_s);
  const q=d.queue.depth+'/'+d.queue.cap;
  const c=d.counters;
  const sess=d.session==='ok'?['b-ok','logada']:['b-err','faltando'];
  const brw=d.browser==='up'?['b-ok','de pé']:['b-err','caído'];
  el('cards').innerHTML=[
   card('Sessão Google','<span class="badge '+sess[0]+'">'+sess[1]+'</span>'),
   card('Browser','<span class="badge '+brw[0]+'">'+brw[1]+'</span>'),
   card('Fila',q),
   card('Requisições',c.requests+' <span style="color:var(--mut);font-size:12px">· '+c.errors+' err · '+c.rejected+' barradas</span>'),
   card('Recusas recuperadas',c.refusal_retries),
   card('Tool calls',c.tool_calls),
  ].join('');
  if(d.active){
   const a=d.active;
   el('activeBox').innerHTML='<div class="active"><div class="t">executando · '
    +esc(a.model)+' <span class="badge b-info">'+(a.stream?'stream':'bloco')+'</span>'
    +(a.tools?' <span class="badge b-warn">'+a.tools+' tools</span>':'')
    +(a.retries?' <span class="badge b-warn">'+a.retries+' retratativa'+(a.retries>1?'s':'')+'</span>':'')
    +'</div><div class="meta">'+dur(a.elapsed_s)+' · '+a.chars+' chars</div>'
    +'<div class="pv">'+esc(a.prompt)+'</div></div>';
  }else el('activeBox').innerHTML='';
  el('tbody').innerHTML=(d.requests||[]).map(r=>'<tr><td class="mono">'+esc(r.id)
   +'</td><td>'+esc(r.model.replace('gemini-web-',''))+'</td><td>'+(r.stream?'stream':'bloco')
   +'</td><td>'+(r.tools||'—')+'</td><td>'+(r.tool_calls||'—')+'</td><td>'+stCell(r.status)
   +'</td><td class="mono">'+dur(r.duration_s)+'</td><td>'+(r.chars||'—')+'</td></tr>').join('')
   ||'<tr><td colspan="8" style="color:var(--mut)">nenhuma ainda</td></tr>';
  el('logs').textContent=(d.logs||[]).join('\n')||'…';
 }catch(e){
  el('st').className='badge b-err';el('st').textContent='offline';
 }
}
function card(k,v){return'<div class="card"><div class="k">'+k+'</div><div class="v">'+v+'</div></div>'}
setInterval(tick,3000);tick();
</script>
</body></html>`
