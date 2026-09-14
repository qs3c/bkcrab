package sandbox

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Limits are deployment-only: a user cannot raise shared-host budgets through
// their agent settings. Zero leaves a limit disabled for existing installations.
type Limits struct {
	QuotaImage     string
	WorkspaceBytes int64
	WorkspaceFiles int
	MaxContainers  int
	MaxPerUser     int
	MaxQueued      int
	QueueTimeout   time.Duration
	CPU            string
	Memory         string
	PIDs           int
}

func LoadLimits() (Limits, error) {
	l := Limits{MaxQueued: 64, QueueTimeout: 60 * time.Second}
	for name, dst := range map[string]*int{
		"BKCRAB_SANDBOX_MAX_CONTAINERS": &l.MaxContainers,
		"BKCRAB_SANDBOX_MAX_PER_USER":   &l.MaxPerUser,
		"BKCRAB_SANDBOX_MAX_QUEUED":     &l.MaxQueued,
		"BKCRAB_SANDBOX_PIDS":           &l.PIDs,
	} {
		if v := os.Getenv(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return l, fmt.Errorf("%s must be a nonnegative integer", name)
			}
			*dst = n
		}
	}
	if v := os.Getenv("BKCRAB_SANDBOX_QUEUE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return l, fmt.Errorf("BKCRAB_SANDBOX_QUEUE_TIMEOUT must be positive")
		}
		l.QueueTimeout = d
	}
	l.CPU = os.Getenv("BKCRAB_SANDBOX_CPUS")
	if l.CPU != "" {
		n, err := strconv.ParseFloat(l.CPU, 64)
		if err != nil || !(n > 0) || n > 1024 {
			return l, fmt.Errorf("BKCRAB_SANDBOX_CPUS must be in (0,1024]")
		}
	}
	l.Memory = os.Getenv("BKCRAB_SANDBOX_MEMORY")
	l.QuotaImage = os.Getenv("BKCRAB_SANDBOX_QUOTA_IMAGE")
	if l.QuotaImage != "" {
		var err error
		l.WorkspaceBytes, err = strconv.ParseInt(os.Getenv("BKCRAB_WORKSPACE_USER_BYTES"), 10, 64)
		if err != nil || l.WorkspaceBytes < 1024 {
			return l, fmt.Errorf("BKCRAB_WORKSPACE_USER_BYTES must be >=1024 with quota image")
		}
		l.WorkspaceFiles, err = strconv.Atoi(os.Getenv("BKCRAB_WORKSPACE_USER_FILES"))
		if err != nil || l.WorkspaceFiles < 1 {
			return l, fmt.Errorf("BKCRAB_WORKSPACE_USER_FILES must be positive with quota image")
		}
	}
	return l, nil
}
