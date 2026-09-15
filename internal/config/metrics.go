package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

type EnvMetrics struct {
	Enabled bool
	Addr    string
	err     error
}

func loadMetricsEnv() EnvMetrics {
	c := EnvMetrics{Addr: "127.0.0.1:18954"}
	if s := os.Getenv("BKCRAB_METRICS_ENABLED"); s != "" {
		var err error
		c.Enabled, err = strconv.ParseBool(s)
		if err != nil {
			c.err = fmt.Errorf("invalid BKCRAB_METRICS_ENABLED")
		}
	}
	if s := os.Getenv("BKCRAB_METRICS_ADDR"); s != "" {
		c.Addr = s
	}
	return c
}

func (c EnvMetrics) Validate() error {
	if c.err != nil {
		return c.err
	}
	if !c.Enabled {
		return nil
	}
	_, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("BKCRAB_METRICS_ADDR must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("BKCRAB_METRICS_ADDR requires a port between 1 and 65535")
	}
	return nil
}
