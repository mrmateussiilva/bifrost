package main

import (
	"os"
	"strconv"
	"strings"
)

// Config agrupa as poucas configurações do Bifrost, lidas direto do ambiente.
type Config struct {
	Addr          string   // ex: ":8080"
	Profile       string   // user-data-dir do Chromium (modo single-profile)
	Profiles      []string // multi-profile: BIFROST_PROFILES, vírgula-separados (round-robin)
	Headless      bool     // se true, oculta a janela do Chromium
	ChromePath    string   // path do binário (vazio = default do chromedp)
	APIKey        string   // se preenchido, exige Authorization: Bearer
	LogLevel      string   // debug, info, warn, error
	Provider      string   // provedor LLM default ("gemini" ou "chatgpt")
	Model         string   // modelo default quando o request omite "model"
	PasswordStore string   // keystore do Chrome: gnome-libsecret (desktop) ou basic (container)
	NoSandbox     bool     // container Docker: seccomp default bloqueia o sandbox do Chrome
	PoolSize      int      // abas por profile (concorrência)
}

func LoadConfig() Config {
	cfg := Config{
		Addr:          envOr("BIFROST_ADDR", ":8080"),
		Profile:       envOr("BIFROST_PROFILE", "./data/chrome-profile"),
		Headless:      envBool("BIFROST_HEADLESS", false),
		ChromePath:    os.Getenv("BIFROST_CHROME"),
		APIKey:        os.Getenv("BIFROST_API_KEY"),
		LogLevel:      envOr("BIFROST_LOG", "info"),
		Provider:      envOr("BIFROST_PROVIDER", "gemini"),
		Model:         envOr("BIFROST_MODEL", ""),
		PasswordStore: envOr("BIFROST_PASSWORD_STORE", "gnome-libsecret"),
		NoSandbox:     envBool("BIFROST_NO_SANDBOX", false),
		PoolSize:      envInt("BIFROST_POOL_SIZE", 1),
	}
	// multi-profile: N contas em round-robin (o limite de uso é por conta).
	// Profile vira o primeiro da lista — os comandos de diagnóstico (login,
	// inspect, test) operam nele; o painel cuida do login dos demais.
	if v := os.Getenv("BIFROST_PROFILES"); v != "" {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.Profiles = append(cfg.Profiles, p)
			}
		}
		if len(cfg.Profiles) > 0 {
			cfg.Profile = cfg.Profiles[0]
		}
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if i < 1 {
		return 1
	}
	return i
}

// envNonNegative lê um int >= 0 do ambiente (ausente/inválido → default).
// Diferente do envInt: 0 é valor VÁLIDO (desliga a funcionalidade) — usado
// pelos orçamentos de prompt (BIFROST_TOOL_RESULT_MAX, BIFROST_MAX_PROMPT).
func envNonNegative(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}
