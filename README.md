# Bifrost

Gateway HTTP compatível com a API da OpenAI que usa a **interface web do Gemini** como backend. Sem API paga: o Bifrost controla um Chromium real (via Chrome DevTools Protocol) autenticado com a sua própria sessão do Gemini e expõe tudo como `POST /v1/chat/completions`.

```text
OpenCode / cliente OpenAI
            |
            | HTTP (OpenAI-compatible)
            v
         Bifrost  ──chromedp/CDP──▶  Chromium  ──▶  gemini.google.com
```

## Status

MVP funcional, testado de ponta a ponta (2026-09-05):

- [x] `GET /health` (ok / degraded com motivo)
- [x] `GET /v1/models` e `GET /v1/models/{model}`
- [x] `POST /v1/chat/completions` (**com e sem streaming** — SSE `chat.completion.chunk` + `[DONE]`)
- [x] Chromium persistente, headless, com sessão de login manual
- [x] **Seleção de modelo**: 4 modos da conta mapeados + `BIFROST_MODEL` como default + `bifrost modes` para manutenção
- [x] Erros no envelope OpenAI (`{"error":{message,type,code}}`), 404/405/415 em JSON, CORS, `X-Request-Id`, access log
- [x] **Conversa nova por requisição** (independência entre chamadas, sem acúmulo de histórico)
- [x] **Auto-recuperação**: Chromium morto → relança sozinho no próximo request; health honesto (`busy` durante geração); lock singleton órfão (chrome morto sem graceful close) → detecta, limpa e relança
- [x] **Fila limitada**: 1 executando + 4 esperando; acima disso `429`
- [x] **Desconexão do cliente aborta a geração** (stream e não-stream) — sem esperar o fim no vácuo
- [x] **Extração estruturada**: code blocks viram fences de markdown (sem o rótulo da linguagem vazando), títulos com `#`, listas com marcadores
- [x] **Function calling simulado**: `tools` do request viram protocolo no prompt; chamadas viram `tool_calls` no formato OpenAI (stream e não-stream); resultados de tool voltam como mensagens `[TOOL]`; retratativa automática de recusa
- [x] **API key opcional** (`Authorization: Bearer` ou `X-Api-Key`)
- [x] **Painel de observação** (Evolution API-style): `/panel` — dashboard HTML com estado ao vivo (polling), fila, contadores, logs e histórico de requests
- [x] **Docker**: imagem multi-stage (Go + google-chrome-stable), compose com bind mount do profile, healthcheck — ver [Docker](#docker-produção-e-desenvolvimento)
- [ ] múltiplos providers (fora de escopo do MVP)

## Endpoints

| Rota | Método | Descrição |
|---|---|---|
| `/health` | GET | `{"status":"ok"}` ou `{"status":"degraded","browser":"down"|"session":"missing"}` |
| `/v1/models` | GET | lista de modelos |
| `/v1/models/{model}` | GET | detalhe do modelo |
| `/v1/chat/completions` | POST | chat completion (corpo OpenAI; exige `Content-Type: application/json`) |
| `/panel` | GET | dashboard de observação (HTML, polling 2s) |
| `/panel/data` | GET | JSON: status do browser, fila, contadores, logs recentes, histórico e request ativo |
| `/` | GET | redireciona para `/panel` |

Exemplo:

```bash
curl -s http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini-web-flash","messages":[{"role":"user","content":"Oi"}]}'
```

Resposta: objeto `chat.completion` com `choices[0].message.content`, `finish_reason:"stop"`, `logprobs:null` e `usage` (estimado em ~4 chars/token — a UI web não expõe tokens reais).

### Streaming

`"stream": true` responde em SSE (`text/event-stream`): primeiro chunk com `delta:{role:"assistant"}`, chunks `chat.completion.chunk` com o conteúdo conforme a geração avança, chunk final com `finish_reason:"stop"`, `usage` quando pedido via `"stream_options":{"include_usage":true}` e `data: [DONE]`.

```bash
curl -N http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini-web-flash-lite","stream":true,"messages":[{"role":"user","content":"Oi"}]}'
```

Como o texto vem do DOM da UI web, a emissão é **por parágrafo** (bloco estável), não por token: uma part só vai ao cliente quando existe há 3 polls (~0,9s) sem mudar e já tem irmã depois dela — o Gemini continua escrevendo um parágrafo depois de já criar o elemento seguinte, e re-parseia markdown da part viva (asteriscos viram `<strong>`, parágrafos se dividem), então "texto presente" não significa "texto estável". Part já emitida que é re-renderizada depois (raro) deixa o cliente com a versão anterior — o stream segue até o fim; truncar seria pior. Desconectar no meio aborta a geração no servidor.

### Function calling (simulado)

A UI web do Gemini não tem protocolo nativo de ferramentas — o Bifrost simula, ponta a ponta no formato OpenAI:

1. As `tools` do request entram no prompt como bloco `[SYSTEM]` no fim: framing de agente (as ferramentas são reais, instaladas e autorizadas pelo usuário), os schemas e um exemplo de uso completo;
2. O modelo chama uma ferramenta com um **code block contendo apenas o JSON** `{"name": ..., "arguments": {...}}`. A detecção é pelo conteúdo com **nome de ferramenta declarada** — o rótulo de linguagem do code block não serve, porque a UI do Gemini substitui rótulos desconhecidos por um genérico localizado ("Snippet de código");
3. O Bifrost traduz para `choices[0].message.tool_calls` + `finish_reason:"tool_calls"` (no streaming, `delta.tool_calls` indexados; os blocos de chamada **nunca** vazam como conteúdo);
4. O cliente executa e devolve `{"role":"tool", "tool_call_id", "content"}`; no histórico serializado, chamadas do assistente voltam como fences e resultados como mensagens `[TOOL nome]`.

O prior de segurança do Gemini web ("não tenho acesso ao seu computador") é **probabilístico** — mais forte para ferramentas de arquivo/comando do que para consultas. O Bifrost inspeciona o começo da resposta e, em recusa, re-executa com uma mensagem corretiva (no streaming, antes de qualquer byte chegar ao cliente; até 2 retratativas, a última sem inspeção). Efetivo em ~9 de cada 10 turnos; cada retratativa custa uma geração (~6s). `tool_choice` aceita `auto` (default), `none` e `required`.

### Painel de observação

Acesse em `http://localhost:8081/panel`. O painel faz polling a cada 2s no `/panel/data` (JSON) e renderiza:

- **Estado do browser e fila** — se está ocupado, quantos na fila, capacidade
- **Contadores** — requests, erros, recusas de fila, recusas de tool, tool calls
- **Request ativo ao vivo** — modelo, prompt (primeiros 100 chars), se tem tools, tempo decorrido, retries
- **Histórico** — os 30 requests mais recentes, ordenados do mais recente ao mais antigo (cor verde/vermelha = ok/erro)
- **Logs** — as 12 linhas de log mais recentes, cor por nível (debug cinza, info preto, warn laranja, error vermelho)

Os dados ficam em ring buffers em memória — somem no restart. A rota `/` redireciona automaticamente para `/panel`. Também serve de cheat-sheet: o botão ao lado do título (`/panel`) copia o JSON mais recente no clipboard.

## Modelos

| id | modo do Gemini | uso |
|---|---|---|
| `gemini-web` | mantém o modo atual da conversa | padrão |
| `gemini-web-flash-lite` | **3.5 Flash Lite** | o mais rápido (~4,5s) |
| `gemini-web-flash` | **3.8 Flash** | rápido, geral (~5s) |
| `gemini-web-pro` | **3.1 Pro** | raciocínio (~7s) |
| `gemini-web-pro-extended` | **Pro Estendido** ("Raciocínio complexo") | raciocínio profundo (~7s+) |

O request escolhe via `"model"`; omitindo, vale `BIFROST_MODEL` (default `gemini-web`), validado no boot. A troca de modo só acontece quando o atual é diferente — ler o rótulo do botão custa um evaluate.

Os ids vêm do registro `geminiModes` (gemini.go), alimentado pela tabela do comando `bifrost modes`. Atenção: o **item do menu ≠ rótulo ativo** ("3.5 Flash Lite" vira "Gemini Flash-Lite") e a confirmação usa `endsWith` porque "Gemini Flash" é prefixo de "Gemini Flash-Lite". Se o Gemini renomear modos, rode `bifrost modes` e atualize a tabela.

## Configuração (env)

| Var | Default | Descrição |
|---|---|---|
| `BIFROST_ADDR` | `:8080` | endereço HTTP (na máquina do autor `:8080` está ocupado; usa-se `:8081`) |
| `BIFROST_PROFILE` | `./data/chrome-profile` | profile persistente do Chromium |
| `BIFROST_HEADLESS` | `false` | `true` roda com `--headless=new` |
| `BIFROST_CHROME` | (auto) | caminho do binário do Chrome/Chromium, se não estiver no PATH |
| `BIFROST_API_KEY` | (vazio) | se definida, exige `Authorization: Bearer` ou `X-Api-Key` (`/health` fica aberto) |
| `BIFROST_LOG` | `info` | `debug`\|`info`\|`warn`\|`error` |
| `BIFROST_MODEL` | `gemini-web` | modelo quando o request omite `model`; recusado no boot se inválido |
| `BIFROST_PASSWORD_STORE` | `gnome-libsecret` | keystore do Chrome: `gnome-libsecret` (desktop) ou `basic` (container). Tem de ser o **mesmo em todo acesso ao profile** — quem abre com store errado não decifra (e descarta) os cookies do login |
| `BIFROST_NO_SANDBOX` | `false` | `true` adiciona `--no-sandbox --disable-dev-shm-usage` (necessário em container; o seccomp default do Docker bloqueia o sandbox do Chrome) |

## Como rodar

```bash
BIFROST_ADDR=:8081 BIFROST_HEADLESS=true go run . serve   # servidor (default sem args)
go run . test "escreva hello world em Go"                 # milestone no terminal (browser próprio)
go run . login                                           # confirma sessão persistente (sempre headed)
go run . inspect                                         # despejo do DOM do Gemini (diagnóstico de seletores)
go run . modes                                           # tabela item↔rótulo dos modos da conta (manutenção)
pkill -x bifrost                                         # encerra qualquer instância
```

Comandos que abrem browser próprio (`test`, `inspect`, `login`) não podem rodar com o `serve` no ar — o profile é único (SingletonLock) e a segunda instância aborta. Com o servidor no ar, teste via `curl`.

Chrome morto sem graceful close (SIGKILL de container, crash) deixa o lock singleton órfão — e cada container tem hostname diferente, então o próximo Chrome acha que "outra máquina" o prende. O Bifrost detecta esse caso (testa se o socket singleton responde; socket morto = órfão), limpa os locks e relança sozinho. Lock de uma instância viva continua sendo erro, por design.

## Docker (produção e desenvolvimento)

Imagem multi-stage: estágio Go compila o binário estático; runtime `debian:bookworm-slim` + `google-chrome-stable` do repositório do Google (não Chromium — o login do Google é mais confiável com o branding Chrome). Usuário não-root com **uid 1000** (mesmo do host → bind mount do profile sem chown), healthcheck no `/health`. A imagem já fixa por ENV os padrões de container: `BIFROST_HEADLESS=true`, `BIFROST_PASSWORD_STORE=basic`, `BIFROST_NO_SANDBOX=true`, porta interna `:8080`.

Duas diferenças do host, e por quê:

1. **Keystore `basic`**: dentro do container não existe gnome-keyring; os cookies são cifrados com chave interna do próprio Chrome. Regra invariável: o keystore tem de ser o mesmo em todo acesso ao profile — o login e o serve do container usam `basic` os dois. Cookies cifrados pelo desktop (`gnome-libsecret`, v11) **não** são decifrados pelo `basic` — e vice-versa não há problema: `basic` (v10) é legível pelos dois. Só que o Chrome do host **re-cifra** para v11 ao escrever, o que quebraria a sessão do container de novo. Conclusão: **um profile, um keystore** — containerizado, o profile não volta ao host (ou volta com `BIFROST_PASSWORD_STORE=basic`).
2. **`--no-sandbox`**: o seccomp default do Docker bloqueia o sandbox de namespaces do Chrome; sem a flag o processo morre no boot. O isolamento fica por conta do container (não-root, sem privilégios extras, função única é o Gemini). Não use este Chrome para navegação arbitrária.

### Rodar

```bash
docker compose up -d --build     # build + serve; host :8081 → container :8080
docker compose logs -f bifrost   # logs
curl -s http://localhost:8081/health
docker compose down              # encerra
```

### Primeira execução (login, uma vez)

O login continua **manual e fora do CDP** (o Google bloqueia login com debugger anexado). A janela do Chrome abre no seu X11 via socket mount:

```bash
xhost +local:   # permite o container abrir janela no seu display

# 1. Chrome limpo (SEM CDP) com o profile do container, keystore basic
docker compose run --rm --entrypoint google-chrome-stable \
  -e DISPLAY="$DISPLAY" -v /tmp/.X11-unix:/tmp/.X11-unix \
  bifrost \
  --user-data-dir=/data/chrome-profile --password-store=basic \
  --no-first-run --no-default-browser-check --no-sandbox \
  "https://gemini.google.com/app"

# 2. Faça o login (senha, 2FA), espere o Gemini carregar suas conversas,
#    feche a janela normalmente (grava os cookies no profile)

# 3. Confirme a persistência (esse sim é o Bifrost, com CDP)
docker compose run --rm -e DISPLAY="$DISPLAY" -v /tmp/.X11-unix:/tmp/.X11-unix \
  bifrost login      # deve imprimir "gemini session ok" em ~3s

xhost -local:   # revoga a permissão do display
```

> Migração do host para o container: a sessão atual do profile foi cifrada com `gnome-libsecret`; o container (basic) não a lê — é preciso refazer o login **uma vez** pelo fluxo acima. A partir daí o perfil pertence ao container.

Com a sessão ativa, os outros comandos rodam headless dentro do container (sem X11):

```bash
docker compose run --rm bifrost test "escreva hello world em Go"
docker compose run --rm bifrost modes    # regenera a tabela de modos (manutenção)
```

## Primeira execução sem Docker (login manual no host)

O Google **bloqueia login em navegador com debugger anexado** ("Este navegador ou app pode não ser seguro") — por design, e o Bifrost não contorna proteções. O fluxo é: o login acontece **uma vez**, num Chrome normal (sem CDP), usando o mesmo profile do projeto:

```bash
# 1. Chrome limpo com o profile do projeto
google-chrome-stable \
  --user-data-dir="$PWD/data/chrome-profile" \
  --no-first-run --no-default-browser-check \
  "https://gemini.google.com/app"

# 2. Faça login com sua conta (senha, 2FA), espere o Gemini carregar suas conversas
# 3. Feche a janela normalmente (grava os cookies no profile)
# 4. Confirme a persistência:
go run . login    # deve imprimir "gemini session ok" em ~3s
```

A partir daí o Bifrost (headed ou headless) reutiliza a sessão. Se algum dia a sessão expirar, repita o fluxo.

## Código

| Arquivo | Responsabilidade |
|---|---|
| `main.go` | CLI (`login`/`inspect`/`test`/`serve`), ciclo de vida e rotas do servidor (chi) |
| `browser.go` | Controlador do Chromium: flags de lançamento, keystore, close graceful, health |
| `gemini.go` | **Todos os seletores do Gemini** (`GeminiSelectors`), `Gemini.Complete` (mutex, digitação, envio, espera por DOM, troca de modo) |
| `openai.go` | Tipos e handlers OpenAI, middleware REST (request-id, CORS, access log, recover, auth) |
| `gateway.go` | Supervisão do Chromium: relança se morrer, serializa execuções, limita fila |
| `panel.go` | Painel de observação: ring buffers em memória, contadores, rastro de requests, tee do slog, handlers HTTP |
| `config.go` | Env vars |

Princípios do `Complete` (uma requisição por vez, `sync.Mutex`):

- digitar via `Input.insertText` (eventos de input que o editor Angular/Quill espera);
- enviar com **Enter** (fallback: botão de envio por aria-label);
- esperar por **sinais reais do DOM**: nova resposta com texto estável por 3 polls de 300ms e sem botão "parar" — nunca `sleep` fixo;
- timeout de 3s para confirmar envio, 3s para trocar modo, 3min para a geração (via `context.Context`).

## Armadilhas resolvidas (custaram o debugging — documentadas no código)

1. **`--enable-automation`**: o chromedp passa por padrão (herança do Puppeteer antigo); seta `navigator.webdriver=true` e faz o Google bloquear o login. Removido.
2. **`--password-store=basic`**: default do chromedp; no Linux o Chrome normal cifra cookies (v11) com a chave do **gnome-keyring**, e o Chromium com "basic" não decifra — e chega a **descartar** os cookies do login manual. Fixado `gnome-libsecret` nos dois lados.
3. **Detecção de sessão**: o Gemini **deslogado renderiza a UI completa** (com caixa de prompt!), então prompt ≠ logado. Logado = link `a[href*="SignOutOptions"]` (o da conta); deslogado = botão "Fazer login". E o link de conta do usuário logado aponta para `accounts.google.com` — nunca use `href` de accounts como sinal de login.
4. **Headless**: Chrome 132+ removeu o headless antigo; é preciso `--headless=new`.
5. **Nomes de modo não coincidem**: o item do menu diz "3.5 Flash Lite", mas o rótulo ativo do botão diz "Gemini Flash-Lite". Por isso `geminiMode{MenuItem, Label}`.
6. **1 Chromium, 1 profile, 1 instância**: segunda instância aborta (SingletonLock) — por design.

## Produção

**Recomendado: Docker** (seção acima) — `restart: unless-stopped` cobre quedas do processo, e o gateway relança o Chromium sozinho dentro do container.

**Resiliência** (testadas):

- Chromium morto no meio do serviço → próximo request relança sozinho (~5s) e responde 200; `/health` mostra `degraded` enquanto isso;
- Fila: 1 execução + 4 em espera; a 6ª chamada simultânea recebe `429 queue_full` na hora; quem espera demais recebe `504 queue_timeout`;
- `/health` nunca bloqueia: durante uma geração responde `{"status":"ok","busy":"true"}`;
- Panics em handler viram `500` JSON (o processo não cai).

Alternativa sem Docker, **serviço (systemd user)** — o Bifrost precisa rodar dentro da sessão do usuário (o gnome-keyring desbloqueado é o que decifra os cookies do profile):

```bash
go build -o ~/.local/bin/bifrost .
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/bifrost.service <<'EOF'
[Unit]
Description=Bifrost — gateway OpenAI-compatible sobre o Gemini Web
After=graphical-session.target

[Service]
Environment=BIFROST_ADDR=:8081
Environment=BIFROST_HEADLESS=true
# Environment=BIFROST_API_KEY=uma-chave-bem-longa
ExecStart=%h/.local/bin/bifrost serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload && systemctl --user enable --now bifrost
journalctl --user -u bifrost -f          # logs
```

## Limitações conhecidas

- Uma requisição executa por vez; até 4 esperam na fila (acima disso `429`, espera excessiva `504`);
- Streaming emite por parágrafo estável (não por token) — consequência de ler o DOM da UI web; part já emitida que o Gemini re-renderiza depois (raro) fica com a versão anterior no cliente;
- Function calling é simulado via prompt: o Gemini web às vezes recusa ferramentas de arquivo/comando — o Bifrost retenta com correção (~90% de sucesso efetivo); recusa persistente chega como texto e o agente cliente reage;
- Tokens do `usage` são estimados;
- Sem autenticação na API (`api_key` ignorada) — para uso local;
- `login` sempre abre janela (headless é ignorado nesse comando, por óbvio);
- Painel é volátil: contadores e requests somem no restart (ring buffers em memória).
