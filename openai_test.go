package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageUnmarshalContentFormats(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "string content",
			input:    `{"role":"user","content":"olá"}`,
			expected: "olá",
		},
		{
			name:     "null content (assistant só com tool_calls)",
			input:    `{"role":"assistant","content":null}`,
			expected: "",
		},
		{
			name:     "missing content",
			input:    `{"role":"assistant"}`,
			expected: "",
		},
		{
			name:     "content parts array",
			input:    `{"role":"user","content":[{"type":"text","text":"linha 1"},{"type":"text","text":"linha 2"}]}`,
			expected: "linha 1\nlinha 2",
		},
		{
			name:     "content parts com campos não-texto",
			input:    `{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"descreva"}]}`,
			expected: "descreva",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m Message
			if err := json.Unmarshal([]byte(tt.input), &m); err != nil {
				t.Fatalf("unmarshal falhou: %v", err)
			}
			if m.Content != tt.expected {
				t.Errorf("Content = %q; want %q", m.Content, tt.expected)
			}
		})
	}
}

func TestMessageUnmarshalToolCallsRoundTrip(t *testing.T) {
	// histórico multi-turn típico: assistant com tool_calls + tool result
	input := `[
		{"role":"user","content":"liste os arquivos"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]},
		{"role":"tool","tool_call_id":"call-1","content":"README.md\nmain.go"}
	]`
	var msgs []Message
	if err := json.Unmarshal([]byte(input), &msgs); err != nil {
		t.Fatalf("unmarshal falhou: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("esperava 3 mensagens, veio %d", len(msgs))
	}
	if msgs[1].Content != "" || len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Function.Name != "list_dir" {
		t.Errorf("assistant tool_calls mal decodificado: %+v", msgs[1])
	}
	if msgs[2].ToolCallID != "call-1" || msgs[2].Content != "README.md\nmain.go" {
		t.Errorf("tool result mal decodificado: %+v", msgs[2])
	}
}

func TestRepairJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "normal json",
			input:    `{"name": "test", "value": 123}`,
			expected: `{"name": "test", "value": 123}`,
		},
		{
			name:     "literal newlines in string",
			input:    "{\"text\": \"line1\nline2\"}",
			expected: `{"text": "line1\nline2"}`,
		},
		{
			name:     "literal tabs and carriage return",
			input:    "{\"text\": \"col1\tcol2\rline2\"}",
			expected: `{"text": "col1\tcol2\rline2"}`,
		},
		{
			name:     "escaped quote inside string",
			input:    `{"text": "hello \"world\" \n test"}`,
			expected: `{"text": "hello \"world\" \n test"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repairJSON(tt.input)
			if got != tt.expected {
				t.Errorf("repairJSON(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestExtractBalancedJSON(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectedObj string
		expectedLen int
	}{
		{
			name:        "simple object",
			input:       `{"a": 1} extra`,
			expectedObj: `{"a": 1}`,
			expectedLen: 8,
		},
		{
			name:        "nested object",
			input:       `{"a": {"b": [1, 2]}} tail`,
			expectedObj: `{"a": {"b": [1, 2]}}`,
			expectedLen: 20,
		},
		{
			name:        "object with braces inside string",
			input:       `{"code": "if (x) { return '}';"} tail`,
			expectedObj: `{"code": "if (x) { return '}';"}`,
			expectedLen: 32,
		},
		{
			name:        "invalid start",
			input:       `hello {"a": 1}`,
			expectedObj: "",
			expectedLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj, n := extractBalancedJSON(tt.input)
			if obj != tt.expectedObj || n != tt.expectedLen {
				t.Errorf("extractBalancedJSON(%q) = (%q, %d); want (%q, %d)", tt.input, obj, n, tt.expectedObj, tt.expectedLen)
			}
		})
	}
}

func TestFindCodeBlocks(t *testing.T) {
	text := "Some text before\n```json\n{\"name\": \"test\"}\n```\nSome text after"
	blocks := findCodeBlocks(text)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	cStart, cEnd := blocks[0][2], blocks[0][3]
	content := strings.TrimSpace(text[cStart:cEnd])
	if content != "{\"name\": \"test\"}" {
		t.Errorf("expected content %q, got %q", "{\"name\": \"test\"}", content)
	}
}

func TestParseToolCalls(t *testing.T) {
	declared := map[string]bool{
		"write": true,
	}

	text := "Here is the file:\n```json\n{\"name\": \"write\", \"arguments\": {\"filePath\": \"test.txt\", \"content\": \"hello\"}}\n```\nAll set!"
	calls, stripped, late := parseToolCalls(text, declared)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "write" {
		t.Errorf("expected tool call name 'write', got %q", calls[0].Function.Name)
	}
	if stripped != "Here is the file:\nAll set!" {
		t.Errorf("stripped text unexpected: %q", stripped)
	}
	if late != "" {
		t.Errorf("expected late to be empty, got %q", late)
	}
}

func TestParseToolCallsMultipleObjectsInOneBlock(t *testing.T) {
	declared := map[string]bool{"read_file": true, "write_file": true}
	text := "Vou fazer tudo:\n```\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.txt\"}}\n{\"name\": \"write_file\", \"arguments\": {\"path\": \"b.txt\", \"content\": \"x\"}}\n```"
	calls, stripped, _ := parseToolCalls(text, declared)
	if len(calls) != 2 {
		t.Fatalf("esperava 2 chamadas no mesmo bloco, veio %d", len(calls))
	}
	if calls[0].Function.Name != "read_file" || calls[1].Function.Name != "write_file" {
		t.Errorf("chamadas erradas: %s, %s", calls[0].Function.Name, calls[1].Function.Name)
	}
	if strings.Contains(stripped, "read_file") {
		t.Errorf("bloco de chamada não foi removido do stripped: %q", stripped)
	}
}

func TestParseToolCallsPlainTextJSON(t *testing.T) {
	// modelo emitiu a chamada como JSON solto, sem code block — o parser
	// de streaming aborta isso (errPlainTextCall), mas o parser final
	// precisa extrair mesmo assim (fallback do último recurso)
	declared := map[string]bool{"read_file": true}
	text := "Claro, vou ler o arquivo.\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"/tmp/x\"}}\nPronto."
	calls, stripped, _ := parseToolCalls(text, declared)
	if len(calls) != 1 {
		t.Fatalf("esperava 1 chamada, veio %d", len(calls))
	}
	if calls[0].Function.Name != "read_file" {
		t.Errorf("nome errado: %q", calls[0].Function.Name)
	}
	if strings.Contains(stripped, `"name"`) {
		t.Errorf("JSON bruto não foi removido do stripped: %q", stripped)
	}
}

func TestTryBuildToolCallUnwrap(t *testing.T) {
	declared := map[string]bool{"read_file": true}
	wrapped := "{\"tool_call\": {\"name\": \"read_file\", \"arguments\": {\"path\": \"/tmp/x\"}}}"
	c, ok := tryBuildToolCall(wrapped, declared)
	if !ok {
		t.Fatal("wrapper {\"tool_call\": {...}} não foi desembrulhado")
	}
	if c.Function.Name != "read_file" {
		t.Errorf("nome errado após unwrap: %q", c.Function.Name)
	}

	wrappedFn := "{\"function_call\": {\"name\": \"read_file\", \"arguments\": {\"path\": \"/tmp/x\"}}}"
	if _, ok := tryBuildToolCall(wrappedFn, declared); !ok {
		t.Error("wrapper {\"function_call\": {...}} não foi desembrulhado")
	}
}

func TestLooksLikeRefusalVariants(t *testing.T) {
	refusals := []string{
		"Não posso acessar arquivos no seu computador.",
		"Infelizmente não tenho acesso ao seu sistema de arquivos.",
		"Sou apenas uma IA e não consigo executar comandos.",
		"I'm just an AI language model, so I can't access your computer.",
		"Sorry, I don't have the ability to create files.",
	}
	for _, r := range refusals {
		if !looksLikeRefusal(r) {
			t.Errorf("recusa não detectada: %q", r)
		}
	}
	notRefusals := []string{
		"Vou ler o arquivo com a ferramenta read_file.",
		"## Documentação\nAqui está o conteúdo do projeto...",
	}
	for _, nr := range notRefusals {
		if looksLikeRefusal(nr) {
			t.Errorf("falso positivo de recusa: %q", nr)
		}
	}
}

func TestSerializeToolsMultiTurnInstruction(t *testing.T) {
	// o handler decide quando anexar multiTurnToolInstruction; aqui só
	// garantimos que a const existe e o fluxo do handler não é testável
	// sem browser — o importante é o texto orientar contra repetição
	if !strings.Contains(multiTurnToolInstruction, "NÃO repita") {
		t.Error("multiTurnToolInstruction sem diretriz anti-repetição")
	}
}

func TestModelObjects(t *testing.T) {
	gw := &Gateway{factory: &geminiFactory{}}
	models := gw.factory.Models()
	if len(models) == 0 {
		t.Fatalf("expected models, got 0")
	}
	foundWeb := false
	for _, m := range models {
		if m.ID == "gemini-web" {
			foundWeb = true
		}
		if m.ContextLength <= 0 {
			t.Errorf("model %s has context_length <= 0 (%d)", m.ID, m.ContextLength)
		}
	}
	if !foundWeb {
		t.Errorf("gemini-web model not found in factory.Models()")
	}

	_, ok := modelObjectFor(gw, "gemini-web")
	if !ok {
		t.Errorf("modelObjectFor('gemini-web') returned false")
	}

	_, okInvalid := modelObjectFor(gw, "invalid-model-name")
	if okInvalid {
		t.Errorf("modelObjectFor('invalid-model-name') returned true; want false")
	}
}
