// Package config loads the YAML configuration file with env overrides.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is immutable after Load; all secrets come from file or env,
// never from the repository.
type Config struct {
	Telegram Telegram `yaml:"telegram"`
	LLM      LLM       `yaml:"llm"`
	Context  Context   `yaml:"context"`
	Server   Server    `yaml:"server"`
	Log      Log       `yaml:"log"`
}

type Telegram struct {
	APIID       int    `yaml:"api_id"`
	APIHash     string `yaml:"api_hash"`
	SessionFile string `yaml:"session_file"`
}

type LLM struct {
	Endpoints       []Endpoint `yaml:"endpoints"`
	TimeoutSeconds  int        `yaml:"timeout_seconds"`
	MaxRetries      int        `yaml:"max_retries"`
	Temperature     float64    `yaml:"temperature"`
	MaxTokens       int        `yaml:"max_tokens"`
	SummaryMaxToken int        `yaml:"summary_max_tokens"`
}

type Endpoint struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}

type Context struct {
	WindowWatermark   int     `yaml:"window_watermark"`
	WindowKeepRatio   float64 `yaml:"window_keep_ratio"`
	SummaryMax        int     `yaml:"summary_max"`
	IdleRecompactHrs  int     `yaml:"idle_recompact_hours"`
	MaxInputChars     int     `yaml:"max_input_chars"`
}

type Server struct {
	Listen   string `yaml:"listen"`
	Password string `yaml:"password"`
	DataDir  string `yaml:"data_dir"`
}

type Log struct {
	Level string `yaml:"level"`
}

// PasswordEnv is the env var that overrides Server.Password. The server
// listens on a public interface by default, so an empty password must
// refuse to boot rather than silently expose the chat stream.
const PasswordEnv = "TGBRIDGE_PASSWORD"

func defaults() *Config {
	return &Config{
		Telegram: Telegram{SessionFile: "data/session.json"},
		LLM: LLM{
			TimeoutSeconds: 15,
			MaxRetries:     2,
			Temperature:    1.3,
			MaxTokens:      2048,
		},
		Context: Context{
			WindowWatermark:  4000,
			WindowKeepRatio: 0.5,
			SummaryMax:       400,
			IdleRecompactHrs: 12,
			MaxInputChars:    500,
		},
		Server: Server{Listen: "0.0.0.0:8080", DataDir: "data"},
		Log:    Log{Level: "info"},
	}
}

// Load reads path, applies defaults, validates, and applies env overrides.
func Load(path string) (*Config, error) {
	cfg := defaults()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if cfg.Telegram.APIID == 0 || cfg.Telegram.APIHash == "" {
		return nil, fmt.Errorf("telegram.api_id and telegram.api_hash are required (my.telegram.org)")
	}
	// Phone is deliberately not configurable: the login form in the
	// panel starts empty and never prefills stored numbers.
	if len(cfg.LLM.Endpoints) == 0 {
		return nil, fmt.Errorf("at least one llm.endpoints entry is required")
	}
	for i, ep := range cfg.LLM.Endpoints {
		if ep.BaseURL == "" || ep.APIKey == "" || ep.Model == "" {
			return nil, fmt.Errorf("llm.endpoints[%d]: base_url, api_key and model are required", i)
		}
		if ep.Name == "" {
			ep.Name = fmt.Sprintf("ep%d", i)
			cfg.LLM.Endpoints[i] = ep
		}
	}
	if cfg.Context.WindowWatermark < 500 {
		return nil, fmt.Errorf("context.window_watermark too small")
	}
	if p := os.Getenv(PasswordEnv); p != "" {
		cfg.Server.Password = p
	}
	if cfg.Server.Password == "" {
		return nil, fmt.Errorf("server password required: set it in yaml or %s", PasswordEnv)
	}
	return cfg, nil
}
