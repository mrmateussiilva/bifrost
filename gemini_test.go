package main

import (
	"strings"
	"testing"
)

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
	serialized := SerializeMessages(send)
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

	serialized := SerializeMessages(msgs)

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
