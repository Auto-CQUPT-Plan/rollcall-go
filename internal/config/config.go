package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
)

type Config struct {
	Username             string `json:"username"`
	Password             string `json:"password"`
	CurriculumAPI        string `json:"curriculum_api"`
	CurriculumPreMinutes int    `json:"curriculum_pre_minutes"`
	HTTPPort             *int   `json:"http_port"`
	CenterServerURL      string `json:"center_server_url"`
	CenterServerSecret   string `json:"center_server_secret"`
	AutoLocationCheckin  bool   `json:"auto_location_checkin"`
}

var (
	Cfg      Config
	ClientID string
	DataDir  string

	PauseSharedRollcall atomic.Bool
)

func Load() error {
	DataDir = "data"
	if err := os.MkdirAll(DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	// Defaults
	defaultPort := 8080
	Cfg = Config{
		CurriculumPreMinutes: 10,
		HTTPPort:             &defaultPort,
		AutoLocationCheckin:  true,
	}

	// Load from file
	cfgPath := filepath.Join(DataDir, "config.json")
	if data, err := os.ReadFile(cfgPath); err == nil {
		if err := json.Unmarshal(data, &Cfg); err != nil {
			slog.Warn("Failed to parse config.json, using defaults", "error", err)
		}
	}

	// Environment variable overrides
	applyEnvOverrides()

	if Cfg.Username == "" || Cfg.Password == "" {
		return fmt.Errorf("username and password are required")
	}

	// Load or generate client ID
	ClientID = loadClientID()
	slog.Info("Configuration loaded", "client_id", ClientID)

	return nil
}

func applyEnvOverrides() {
	if v := os.Getenv("EDGE_USERNAME"); v != "" {
		Cfg.Username = v
	}
	if v := os.Getenv("EDGE_PASSWORD"); v != "" {
		Cfg.Password = v
	}
	if v := os.Getenv("EDGE_CURRICULUM_API"); v != "" {
		Cfg.CurriculumAPI = v
	}
	if v := os.Getenv("EDGE_CURRICULUM_PRE_MINUTES"); v != "" {
		var m int
		if _, err := fmt.Sscanf(v, "%d", &m); err == nil {
			Cfg.CurriculumPreMinutes = m
		}
	}
	if v, ok := os.LookupEnv("EDGE_HTTP_PORT"); ok {
		if v == "" {
			Cfg.HTTPPort = nil
		} else {
			var p int
			if _, err := fmt.Sscanf(v, "%d", &p); err == nil {
				Cfg.HTTPPort = &p
			}
		}
	}
	if v := os.Getenv("EDGE_CENTER_SERVER_URL"); v != "" {
		Cfg.CenterServerURL = v
	}
	if v := os.Getenv("EDGE_CENTER_SERVER_SECRET"); v != "" {
		Cfg.CenterServerSecret = v
	}
	if v := os.Getenv("EDGE_AUTO_LOCATION_CHECKIN"); v != "" {
		lower := strings.ToLower(v)
		Cfg.AutoLocationCheckin = lower == "true" || lower == "1" || lower == "yes"
	}
}

func loadClientID() string {
	// Priority: env var > file > generate
	if v := os.Getenv("EDGE_CLIENT_ID"); v != "" {
		return v
	}

	idPath := filepath.Join(DataDir, "client_id.txt")
	if data, err := os.ReadFile(idPath); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id
		}
	}

	id := uuid.New().String()
	if err := os.WriteFile(idPath, []byte(id), 0o644); err != nil {
		slog.Warn("Failed to save client_id", "error", err)
	}
	return id
}

func CookiesPath() string {
	return filepath.Join(DataDir, "cookies.json")
}

func CurriculumCachePath() string {
	return filepath.Join(DataDir, "curriculum_cache.json")
}
