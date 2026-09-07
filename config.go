package main

import (
	"os"
	"strconv"
)

// Config agrupa as poucas configurações do Bifrost, lidas direto do ambiente.
type Config struct {
	Addr          string
	Profile       string
	Headless      bool
	ChromePath    string
	APIKey        string
	LogLevel      string
	Model         string // modelo default quando o request omite "model"
	PasswordStore string // keystore do Chrome: gnome-libsecret (desktop) ou basic (container)
	NoSandbox     bool   // container Docker: seccomp default bloqueia o sandbox do Chrome
}

func LoadConfig() Config {
	return Config{
		Addr:          envOr("BIFROST_ADDR", ":8080"),
		Profile:       envOr("BIFROST_PROFILE", "./data/chrome-profile"),
		Headless:      envBool("BIFROST_HEADLESS", false),
		ChromePath:    envOr("BIFROST_CHROME", ""),
		APIKey:        envOr("BIFROST_API_KEY", ""),
		LogLevel:      envOr("BIFROST_LOG", "info"),
		Model:         envOr("BIFROST_MODEL", ""),
		PasswordStore: envOr("BIFROST_PASSWORD_STORE", "gnome-libsecret"),
		NoSandbox:     envBool("BIFROST_NO_SANDBOX", false),
	}
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
