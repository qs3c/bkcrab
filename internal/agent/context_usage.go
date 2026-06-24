package agent

import (
	"math"
	"sync"
)

const (
	contextUsageSourceProvider = "provider"
	contextUsageSourceEstimate = "estimate"

	contextUsageCalibrationAlpha       = 0.1
	contextUsageCalibrationMinRatio    = 0.5
	contextUsageCalibrationMaxRatio    = 2.0
	contextUsageCalibrationMinEstimate = 1000
)

type contextUsageCalibrator struct {
	mu      sync.Mutex
	factors map[string]float64
}

func (c *contextUsageCalibrator) estimate(key string, raw int) int {
	if raw <= 0 {
		return 0
	}
	factor := c.factor(key)
	return int(math.Ceil(float64(raw) * factor))
}

func (c *contextUsageCalibrator) observe(key string, estimated, actual int) {
	if estimated < contextUsageCalibrationMinEstimate || actual <= 0 {
		return
	}
	ratio := float64(actual) / float64(estimated)
	if ratio < contextUsageCalibrationMinRatio {
		ratio = contextUsageCalibrationMinRatio
	}
	if ratio > contextUsageCalibrationMaxRatio {
		ratio = contextUsageCalibrationMaxRatio
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.factors == nil {
		c.factors = make(map[string]float64)
	}
	old := c.factors[key]
	if old <= 0 {
		old = 1
	}
	c.factors[key] = old*(1-contextUsageCalibrationAlpha) + ratio*contextUsageCalibrationAlpha
}

func (c *contextUsageCalibrator) factor(key string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.factors == nil {
		return 1
	}
	factor := c.factors[key]
	if factor <= 0 {
		return 1
	}
	return factor
}
