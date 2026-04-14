package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port         string
	DBPath       string
	WorkerCount  int
	PollInterval time.Duration
	HTTPTimeout  time.Duration
}

func Load() *Config {
	return &Config{
		Port:         getEnv("PORT", "8080"),
		DBPath:       getEnv("DB_PATH", "rc_jixiang.db"),
		WorkerCount:  getEnvInt("WORKER_COUNT", 5),
		PollInterval: time.Duration(getEnvInt("POLL_INTERVAL_SECONDS", 2)) * time.Second,
		HTTPTimeout:  time.Duration(getEnvInt("HTTP_TIMEOUT_SECONDS", 30)) * time.Second,
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}
