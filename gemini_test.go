package main

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestElideToolResult(t *testing.T) {
	// sob o teto: intacto
	small := strings.Repeat("a", 100)
	if got := elideToolResult(small, 200); got != small {
		t.Errorf("conteúdo sob o teto não deveria mudar: %d chars", len(got))
	}
	// teto 0: desligado
	if got := elideToolResult(strings.Repeat("a", 100), 0); len(got) != 100 {
		t.Errorf("max=0 deveria desligar a elisão: %d chars", len(got))
	}

	// acima do teto: cabeça + marcador + cauda, runa-seguro, determinístico
	fat := strings.Repeat("é", 30000) // multibyte: cortes precisam respeitar runa
	got := elideToolResult(fat, 8192)
	if !utf8.ValidString(got) {
		t.Error("elisão quebrou UTF-8 no meio de runa")
	}
	if len(got) > 8192 {
		t.Errorf("elisão deveria ficar sob o teto: %d chars", len(got))
	}
	if !strings.Contains(got, "bifrost:") || !strings.Contains(got, "omitidos") {
		t.Errorf("marcador de elisão ausente: %q", got[:min(200, len(got))])
	}
	if elideToolResult(fat, 8192) != got {
		t.Error("elisão deveria ser determinística (prefixo estável entre turnos)")
	}
	// cauda preservada: o fim do resultado deve sobreviver
	if !strings.HasSuffix(strings.TrimSpace(got), strings.TrimSpace(fat[len(fat)-100:])) {
		t.Error("cauda do resultado deveria sobreviver à elisão")
	}
}

func TestSerializeMessagesElidesFatToolResults(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "rode os testes"},
		{Role: "assistant", ToolCalls: []toolCall{{
			ID: "c1", Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "bash", Arguments: `{"cmd":"go test ./..."}`},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("ok ", 10000)}, // ~40KB
	}
	out, err := serializeMessagesCap(msgs, 8192, 0) // sem orçamento global
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[TOOL bash]") {
		t.Error("rótulo [TOOL bash] ausente")
	}
	if !strings.Contains(out, "omitidos") {
		t.Error("resultado gordo deveria vir elidado com marcador")
	}
	if len(out) > 12000 {
		t.Errorf("resultado de 40KB deveria cair para ~8KB: total %d", len(out))
	}
	if !strings.Contains(out, "rode os testes") {
		t.Error("conteúdo de user não deveria ser tocado")
	}
}

func TestSerializeMessagesGlobalSqueeze(t *testing.T) {
	// 6 resultados de 6KB (sob o por-resultado de 8KB) + orçamento global
	// apertado: os MAIS ANTIGOS viram só-cabeça até o total caber; as
	// últimas 6 mensagens (working set) ficam inteiras — o resultado mais
	// recente sobrevive sem aperto.
	fat := strings.Repeat("r", 6000)
	msgs := []Message{{Role: "user", Content: "seis leituras"}}
	for i := 0; i < 6; i++ {
		id := string(rune('a' + i))
		msgs = append(msgs,
			Message{Role: "assistant", ToolCalls: []toolCall{{
				ID: id, Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "read", Arguments: `{}`},
			}}},
			Message{Role: "tool", ToolCallID: id, Content: fat},
		)
	}
	// 3 resultados recentes poupados (18KB) + 3 antigos apertados (~1,5KB)
	// + mensagens: orçamento que só fecha com os antigos apertados
	const globalMax = 22000
	out, outErr := serializeMessagesCap(msgs, 8192, globalMax)
	if outErr != nil {
		t.Fatal(outErr)
	}
	if len(out) > globalMax {
		t.Errorf("orçamento global estourado mesmo após aperto: %d > %d", len(out), globalMax)
	}
	// determinístico entre chamadas (reenvio do mesmo histórico)
	again, againErr := serializeMessagesCap(msgs, 8192, globalMax)
	if againErr != nil || again != out {
		t.Error("aperto global deveria ser determinístico")
	}
	// o resultado MAIS RECENTE sobrevive inteiro (working set poupado)
	if !strings.Contains(out, strings.Repeat("r", 500)) {
		t.Error("resultado recente deveria sobreviver sem aperto")
	}
	// os antigos foram apertados: marcador de omissão presente
	if !strings.Contains(out, "omitidos") {
		t.Error("resultados antigos deveriam vir apertados com marcador")
	}
}

func TestSerializeMessagesSqueezesAssistantAndUserText(t *testing.T) {
	// histórico sem UM resultado de tool, mas com texto gordo de assistant
	// e user: o aperto global precisa alcançar esses também — senão o
	// orçamento nunca fecha (sessão de coding agent tem código como texto)
	msgs := []Message{
		{Role: "user", Content: strings.Repeat("u", 4000)},
		{Role: "assistant", Content: strings.Repeat("a", 4000)},
		{Role: "user", Content: strings.Repeat("u", 4000)}, // antigo, gordo
		{Role: "assistant", Content: "pequena recente"},
		{Role: "user", Content: "pequena recente"},
		{Role: "assistant", Content: "pequena recente"},
		{Role: "user", Content: "pequena recente"},
		{Role: "assistant", Content: "última resposta"},
	}
	const globalMax = 6000
	out, outErr := serializeMessagesCap(msgs, 8192, globalMax)
	if outErr != nil {
		t.Fatal(outErr)
	}
	if len(out) > globalMax {
		t.Errorf("orçamento global deveria fechar apertando texto antigo: %d > %d", len(out), globalMax)
	}
	if !strings.Contains(out, "omitidos") {
		t.Error("texto antigo de assistant/user deveria vir apertado com marcador")
	}
	// working set poupado: as últimas mensagens inteiras
	if !strings.Contains(out, "última resposta") || !strings.Contains(out, "pequena recente") {
		t.Error("mensagens recentes deveriam ficar inteiras")
	}
}

func TestSerializeMessagesNoGlobalBudgetKeepsPerResult(t *testing.T) {
	// orçamento global desligado (0): só a elisão por-resultado age
	msgs := []Message{
		{Role: "user", Content: "x"},
		{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("y", 5000)},
	}
	out, outErr := serializeMessagesCap(msgs, 8192, 0)
	if outErr != nil {
		t.Fatal(outErr)
	}
	if strings.Contains(out, "omitidos") {
		t.Error("resultado sob o teto por-resultado não deveria ser elidado")
	}
}

func mkCall(id, name, args string) toolCall {
	return toolCall{
		ID: id, Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: name, Arguments: args},
	}
}

func TestSerializeMessagesElidesToolCallArgs(t *testing.T) {
	// O BURACO DOS 244KB: argumentos de write/edit carregam arquivos
	// inteiros — acima de 4KB viram marcador (fence segue JSON válido e o
	// mapa id→nome usa só o nome)
	bigArgs := `{"filePath":"/x/Sidebar.tsx","content":"` + strings.Repeat("x", 20000) + `"}`
	msgs := []Message{
		{Role: "user", Content: "edita o arquivo"},
		{Role: "assistant", ToolCalls: []toolCall{mkCall("c1", "edit", bigArgs)}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	}
	out, err := serializeMessagesCap(msgs, 8192, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bifrost_args_omitidos") {
		t.Error("argumentos gordos deveriam vir marcados")
	}
	if strings.Contains(out, strings.Repeat("x", 100)) {
		t.Error("argumentos gordos não deveriam ir inteiros")
	}
	if !strings.Contains(out, `"name": "edit"`) {
		t.Error("nome da chamada deveria permanecer no fence")
	}
	if !strings.Contains(out, "[TOOL edit]") {
		t.Error("rótulo [TOOL edit] ausente — o mapa id→nome não depende dos argumentos")
	}
}

func TestSerializeMessagesSqueezesOldToolCallArgs(t *testing.T) {
	// chamada ANTIGA com argumentos medianos (2KB, sob o teto sempre-ativo
	// de 4KB): o passe 2 do aperto global os marca; e o aperto NÃO pode
	// vazar para as mensagens originais (cópia profunda — o casamento de
	// prefixo do sticky usa os dados crus)
	args := `{"content":"` + strings.Repeat("y", 2000) + `"}`
	msgs := []Message{
		{Role: "user", Content: "edita"},
		{Role: "assistant", ToolCalls: []toolCall{mkCall("c1", "edit", args)}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "user", Content: "próximo passo 1"},
		{Role: "user", Content: "próximo passo 2"},
		{Role: "user", Content: "próximo passo 3"},
		{Role: "user", Content: "próximo passo 4"},
		{Role: "user", Content: "próximo passo 5"},
		{Role: "user", Content: "próximo passo 6"},
	}
	out, err := serializeMessagesCap(msgs, 8192, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bifrost_args_omitidos") {
		t.Error("argumentos de chamada antiga deveriam vir marcados no aperto")
	}
	if msgs[1].ToolCalls[0].Function.Arguments != args {
		t.Fatal("aperto vazou para as mensagens originais — casamento de prefixo do sticky corrompido")
	}
}

func TestSerializeMessagesSystemNeverElided(t *testing.T) {
	// system do opencode (AGENTS.md embutido) é gordo mas é a IDENTIDADE
	// do agente: nunca elidado, nem pelo teto por-mensagem, nem pelo
	// último recurso — só conta para o orçamento
	big := strings.Repeat("s", 20000) // > msgContentMax (12KB)
	msgs := []Message{
		{Role: "system", Content: big},
		{Role: "system", Content: big, Control: true},
		{Role: "user", Content: "oi"},
	}
	out, err := serializeMessagesCap(msgs, 8192, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "omitidos") {
		t.Error("system/Control não deveriam ser elidados")
	}
	// 2 blocos × 20000 's' = 40000 — tudo inteiro (headers não têm 's'
	// minúsculo)
	if strings.Count(out, "s") != 40000 {
		t.Errorf("ambos os blocos de system deveriam ir inteiros: %d 's' no total", strings.Count(out, "s"))
	}
}

func TestSerializeMessagesPromptTooLarge(t *testing.T) {
	// system + protocolo sozinhos acima do orçamento: elisão completa não
	// fecha → ERRO honesto (digitar 244KB wedgaria a página)
	big := strings.Repeat("s", 8000)
	msgs := []Message{{Role: "system", Content: big}, {Role: "user", Content: "oi"}}
	_, err := serializeMessagesCap(msgs, 8192, 4000)
	if err == nil || !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("esperava ErrPromptTooLarge, veio: %v", err)
	}
}

func TestSerializeMessagesLastResortSqueezesWorkingSet(t *testing.T) {
	// só conteúdo RECENTE gordo (paste gigante do usuário no último
	// turno): último recurso degrada o working set em vez de falhar —
	// contexto recente pior > erro para o cliente
	paste := strings.Repeat("p", 20000)
	msgs := []Message{
		{Role: "user", Content: "oi"},
		{Role: "assistant", Content: "olá"},
		{Role: "user", Content: paste},
	}
	const globalMax = 6000
	out, err := serializeMessagesCap(msgs, 8192, globalMax)
	if err != nil {
		t.Fatalf("último recurso deveria evitar o erro: %v", err)
	}
	if len(out) > globalMax {
		t.Errorf("orçamento estourado mesmo com working set degradado: %d", len(out))
	}
	if !strings.Contains(out, "omitidos") {
		t.Error("paste recente deveria vir degradado com marcador")
	}
	if !strings.Contains(out, paste[:200]) {
		t.Error("cabeça do paste recente deveria sobreviver")
	}
}

func TestStickyHelpers(t *testing.T) {
	// historyBase/controlMsgs: separação histórico × blocos de controle
	msgs := []Message{
		{Role: "system", Content: "sys prompt"},
		{Role: "user", Content: "oi"},
		{Role: "system", Content: "PROTOCOLO", Control: true},
		{Role: "system", Content: "CORRECAO", Nudge: true},
	}
	base := historyBase(msgs)
	if len(base) != 2 || base[0].Content != "sys prompt" || base[1].Content != "oi" {
		t.Fatalf("historyBase incorreto: %+v", base)
	}
	ctrls := controlMsgs(msgs)
	if len(ctrls) != 2 || ctrls[0].Content != "PROTOCOLO" || ctrls[1].Content != "CORRECAO" {
		t.Fatalf("controlMsgs incorreto: %+v", ctrls)
	}
	if !hasNudge(msgs) {
		t.Error("hasNudge deveria detectar a correção")
	}
	if hasNudge(base) {
		t.Error("hasNudge não deveria acionar sem correção")
	}

	// prefixMatch: turno seguinte do agente é continuação
	prev := []Message{
		{Role: "system", Content: "sys prompt"},
		{Role: "user", Content: "cria o arquivo"},
	}
	cur := []Message{
		{Role: "system", Content: "sys prompt"},
		{Role: "user", Content: "cria o arquivo"},
		{Role: "assistant", ToolCalls: []toolCall{{ID: "c1", Type: "function"}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	}
	if !prefixMatch(prev, cur) {
		t.Error("histórico crescido deveria casar como prefixo")
	}
	if prefixMatch(cur, prev) {
		t.Error("histórico menor não é prefixo do maior")
	}
	divergente := []Message{
		{Role: "system", Content: "OUTRO sys"},
		{Role: "user", Content: "cria o arquivo"},
	}
	if prefixMatch(prev, divergente) {
		t.Error("sistema diferente não deveria casar")
	}

	// mensagens de assistant com tool calls iguais casam; argumento diferente não
	a := Message{Role: "assistant", ToolCalls: []toolCall{{
		ID: "c1", Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "write_file", Arguments: `{"path":"a.txt"}`},
	}}}
	b := Message{Role: "assistant", ToolCalls: []toolCall{{
		ID: "c1", Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "write_file", Arguments: `{"path":"a.txt"}`},
	}}}
	if !messagesEqual(a, b) {
		t.Error("tool calls idênticos deveriam casar")
	}
	b.ToolCalls[0].Function.Arguments = `{"path":"b.txt"}`
	if messagesEqual(a, b) {
		t.Error("argumentos diferentes não deveriam casar")
	}
}

func TestStickyDeltaElidesTextAssistants(t *testing.T) {
	// turno 2 de um loop de agente: o delta do histórico é
	// [assistant(tool_calls), tool(result)] — o fence do tool_call fica
	// (recap minúsculo + mapeamento do nome p/ o [TOOL nome]); respostas
	// de TEXTO do assistant (o grosso do payload) não são reenviadas.
	prev := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "cria o arquivo"},
	}
	cur := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "cria o arquivo"},
		{Role: "assistant", ToolCalls: []toolCall{{
			ID: "c1", Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "write_file", Arguments: `{"path":"a.txt"}`},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok, criado"},
		{Role: "assistant", Content: "Arquivo criado com sucesso! Aqui está o relatório longo da operação..."},
		{Role: "user", Content: "agora le o b.txt"},
		{Role: "system", Content: "PROTOCOLO", Control: true},
	}
	base := historyBase(cur)
	delta := elideTextAssistants(base[len(prev):])

	var sawTextAssistant bool
	for _, m := range delta {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			sawTextAssistant = true
		}
	}
	if sawTextAssistant {
		t.Fatalf("resposta de texto do assistant deveria ser elidida do delta: %+v", delta)
	}
	if len(delta) != 3 { // assistant(tool_calls), tool, user
		t.Fatalf("delta inesperado: %+v", delta)
	}

	send := append(delta, controlMsgs(cur)...)
	serialized, serr := SerializeMessages(send)
	if serr != nil {
		t.Fatal(serr)
	}
	if !strings.Contains(serialized, "[TOOL write_file]") {
		t.Errorf("resultado da ferramenta sem rótulo de nome no delta:\n%s", serialized)
	}
	if !strings.Contains(serialized, "agora le o b.txt") {
		t.Errorf("nova mensagem do usuário ausente no delta:\n%s", serialized)
	}
	if !strings.Contains(serialized, "PROTOCOLO") {
		t.Errorf("protocolo ausente no prompt do delta:\n%s", serialized)
	}
	if strings.Contains(serialized, "relatório longo") {
		t.Errorf("texto do assistant vazou no delta:\n%s", serialized)
	}
}

func TestSerializeMessages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello, list files."},
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []toolCall{
				{
					ID:   "call-123",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{
						Name:      "list_dir",
						Arguments: `{"path": "."}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call-123",
			Content:    "README.md\nmain.go",
		},
	}

	serialized, serr := SerializeMessages(msgs)
	if serr != nil {
		t.Fatal(serr)
	}

	if !strings.Contains(serialized, "[SYSTEM]\nYou are a helpful assistant.") {
		t.Errorf("missing system prompt in serialized output:\n%s", serialized)
	}
	if !strings.Contains(serialized, "[USER]\nHello, list files.") {
		t.Errorf("missing user prompt in serialized output:\n%s", serialized)
	}
	if !strings.Contains(serialized, `{"name": "list_dir", "arguments": {"path": "."}}`) {
		t.Errorf("missing tool call fence in serialized output:\n%s", serialized)
	}
	if !strings.Contains(serialized, "[TOOL list_dir]\nREADME.md\nmain.go") {
		t.Errorf("missing tool result header in serialized output:\n%s", serialized)
	}
}
