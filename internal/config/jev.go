package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/qs3c/bkcrab/internal/jev"
)

// JevCfg is deployment-only. It is never serialized into user settings.
type JevCfg struct {
	Enabled                                                      bool
	Endpoint                                                     string
	APIKey                                                       string `json:"-"`
	Model                                                        string
	TimeoutMS, Concurrency, Retries                              int
	MaxRequestCostUSD, InputCostPerMillion, OutputCostPerMillion float64
	MemoryGuardMode                                              string
}

func (c JevCfg) LogValue() slog.Value {
	return slog.GroupValue(slog.Bool("enabled", c.Enabled), slog.String("model", c.Model), slog.String("memoryGuardMode", c.MemoryGuardMode))
}
func loadJevEnv() JevCfg {
	c := JevCfg{Endpoint: "https://api.typesafe.ai/v1/systemone", Model: "jev-1.13.0", TimeoutMS: 5000, Concurrency: 4, Retries: 1, MaxRequestCostUSD: .01, InputCostPerMillion: .042, MemoryGuardMode: "off"}
	c.Enabled = os.Getenv("BKCRAB_JEV_ENABLED") == "true"
	for k, p := range map[string]*string{"ENDPOINT": &c.Endpoint, "API_KEY": &c.APIKey, "MODEL": &c.Model, "MEMORY_GUARD_MODE": &c.MemoryGuardMode} {
		if v, ok := os.LookupEnv("BKCRAB_JEV_" + k); ok {
			*p = v
		}
	}
	for k, p := range map[string]*int{"TIMEOUT_MS": &c.TimeoutMS, "CONCURRENCY": &c.Concurrency, "RETRIES": &c.Retries} {
		if v, ok := os.LookupEnv("BKCRAB_JEV_" + k); ok {
			n, e := strconv.Atoi(v)
			if e != nil {
				n = -1
			}
			*p = n
		}
	}
	for k, p := range map[string]*float64{"MAX_REQUEST_COST_USD": &c.MaxRequestCostUSD, "INPUT_COST_USD_PER_MILLION": &c.InputCostPerMillion, "OUTPUT_COST_USD_PER_MILLION": &c.OutputCostPerMillion} {
		if v, ok := os.LookupEnv("BKCRAB_JEV_" + k); ok {
			n, e := strconv.ParseFloat(v, 64)
			if e != nil {
				n = -1
			}
			*p = n
		}
	}
	return c
}
func (c JevCfg) NewClient() (*jev.Client, error) {
	switch c.MemoryGuardMode {
	case "", "off", "shadow", "enforce":
	default:
		return nil, fmt.Errorf("invalid Jev memory guard mode")
	}
	if !c.Enabled {
		if c.MemoryGuardMode != "" && c.MemoryGuardMode != "off" {
			return nil, fmt.Errorf("Jev memory guard requires BKCRAB_JEV_ENABLED")
		}
		return nil, nil
	}
	return jev.New(jev.Options{Endpoint: c.Endpoint, APIKey: c.APIKey, Model: c.Model, Timeout: time.Duration(c.TimeoutMS) * time.Millisecond, Concurrency: c.Concurrency, Retries: c.Retries, MaxCostUSD: c.MaxRequestCostUSD, InputCostPerMillion: c.InputCostPerMillion, OutputCostPerMillion: c.OutputCostPerMillion})
}
