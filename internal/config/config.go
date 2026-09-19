// Package config loads queryforge's configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr string

	// MetadataDSN is queryforge's own database: jobs and measurement history.
	MetadataDSN string
	// TargetDSN is the database whose queries are being optimised. It is
	// treated as read-only apart from index experiments, which are always
	// rolled back. Point it at a replica: index experiments take a brief
	// exclusive lock on the table they build against.
	TargetDSN string

	RedisAddr     string
	RedisPassword string
	RedisDB       int

	KafkaBrokers []string
	JobTopic     string
	DLQTopic     string
	ConsumerGrp  string

	GeminiAPIKey string
	GeminiModel  string
	LLMTimeout   time.Duration

	QueryTimeout      time.Duration
	MeasurementRuns   int
	WarmupRuns        int
	MinImprovementPct float64
	MaxCandidates     int
	MaxIndexTests     int
	TestIndexes       bool

	WorkerConcurrency  int
	JobTimeout         time.Duration
	MaxDeliveryAttempt int
}

func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:    env("HTTP_ADDR", ":8082"),
		MetadataDSN: env("METADATA_DSN", "postgres://queryforge:queryforge@localhost:5434/queryforge?sslmode=disable"),
		TargetDSN:   env("TARGET_DSN", "postgres://queryforge:queryforge@localhost:5434/shop?sslmode=disable"),

		RedisAddr:     env("REDIS_ADDR", "localhost:6381"),
		RedisPassword: env("REDIS_PASSWORD", ""),
		RedisDB:       envInt("REDIS_DB", 0),

		KafkaBrokers: strings.Split(env("KAFKA_BROKERS", "localhost:9096"), ","),
		JobTopic:     env("KAFKA_JOB_TOPIC", "sql.optimize.requested"),
		DLQTopic:     env("KAFKA_DLQ_TOPIC", "sql.optimize.dlq"),
		ConsumerGrp:  env("KAFKA_CONSUMER_GROUP", "queryforge-workers"),

		GeminiAPIKey: env("GEMINI_API_KEY", ""),
		GeminiModel:  env("GEMINI_MODEL", "gemini-flash-latest"),
		LLMTimeout:   envDur("LLM_TIMEOUT", 90*time.Second),

		QueryTimeout:      envDur("QUERY_TIMEOUT", 30*time.Second),
		MeasurementRuns:   envInt("MEASUREMENT_RUNS", 5),
		WarmupRuns:        envInt("WARMUP_RUNS", 1),
		MinImprovementPct: envFloat("MIN_IMPROVEMENT_PCT", 10),
		MaxCandidates:     envInt("MAX_CANDIDATES", 4),
		MaxIndexTests:     envInt("MAX_INDEX_TESTS", 3),
		TestIndexes:       envBool("TEST_INDEXES", true),

		WorkerConcurrency:  envInt("WORKER_CONCURRENCY", 2),
		JobTimeout:         envDur("JOB_TIMEOUT", 15*time.Minute),
		MaxDeliveryAttempt: envInt("MAX_DELIVERY_ATTEMPTS", 3),
	}
	if c.MeasurementRuns < 3 {
		// Fewer than three runs has no meaningful median, and this service's
		// entire claim rests on the median being trustworthy.
		return nil, fmt.Errorf("MEASUREMENT_RUNS must be at least 3, got %d", c.MeasurementRuns)
	}
	if c.MinImprovementPct < 0 || c.MinImprovementPct > 100 {
		return nil, fmt.Errorf("MIN_IMPROVEMENT_PCT must be between 0 and 100")
	}
	return c, nil
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}
