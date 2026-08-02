package fairqueue

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxResourceBytes      = 120
	maxTenantBytes        = 480
	maxTaskIDBytes        = 128
	maxCursorBytes        = 512
	maxHighWaterBytes     = 191
	maxRecoveryPageSize   = 10_000
	maxResourceDuration   = 24 * time.Hour
	tenantQueuePrefix     = "bkcrab.fair.q."
	deadLetterQueuePrefix = "bkcrab.fair.dlq."
)

var (
	resourcePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,119}$`)
	taskTypePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

// TaskIDValidator validates the resource-specific canonical syntax after the
// transport-independent length, UTF-8, and control-character checks pass.
type TaskIDValidator func(string) bool

type CapacityLimits struct {
	GlobalConcurrency       int
	PerUserBaseConcurrency  int
	PerUserBurstConcurrency int
	BorrowEnabled           bool
}

func (l CapacityLimits) Validate() error {
	if l.GlobalConcurrency <= 0 {
		return errors.New("fairqueue: global concurrency must be positive")
	}
	if l.PerUserBaseConcurrency <= 0 {
		return errors.New("fairqueue: per-user base concurrency must be positive")
	}
	if l.PerUserBurstConcurrency <= 0 {
		return errors.New("fairqueue: per-user burst concurrency must be positive")
	}
	if l.PerUserBaseConcurrency > l.PerUserBurstConcurrency {
		return errors.New("fairqueue: per-user base concurrency exceeds burst concurrency")
	}
	if l.PerUserBurstConcurrency > l.GlobalConcurrency {
		return errors.New("fairqueue: per-user burst concurrency exceeds global concurrency")
	}
	return nil
}

type ResourceConfig struct {
	Key            string
	ValidateTaskID TaskIDValidator

	LocalWorkers                int
	GlobalConcurrency           int
	PerUserBaseConcurrency      int
	PerUserBurstConcurrency     int
	BorrowEnabled               bool
	ReconcileInterval           time.Duration
	ExpiredRunningSweepInterval time.Duration
	ReconcilePageSize           int
	ReservationTTL              time.Duration
	ReservationHeartbeat        time.Duration
	PrepareTimeout              time.Duration
	ProvisionalTTL              time.Duration
	ProcessingTurnTTL           time.Duration
	RecoveryDrainTimeout        time.Duration
	DispatchInterval            time.Duration
	PublishAttemptTimeout       time.Duration
}

func (c ResourceConfig) CapacityLimits() CapacityLimits {
	return CapacityLimits{
		GlobalConcurrency:       c.GlobalConcurrency,
		PerUserBaseConcurrency:  c.PerUserBaseConcurrency,
		PerUserBurstConcurrency: c.PerUserBurstConcurrency,
		BorrowEnabled:           c.BorrowEnabled,
	}
}

func (c ResourceConfig) Validate() error {
	if err := ValidateResource(c.Key); err != nil {
		return err
	}
	if c.ValidateTaskID == nil {
		return errors.New("fairqueue: task ID validator is required")
	}
	if c.LocalWorkers <= 0 {
		return errors.New("fairqueue: local workers must be positive")
	}
	if err := c.CapacityLimits().Validate(); err != nil {
		return err
	}
	if err := ValidatePageLimit(c.ReconcilePageSize); err != nil {
		return fmt.Errorf("fairqueue: reconcile page size: %w", err)
	}
	durations := []struct {
		name  string
		value time.Duration
	}{
		{"reconcile interval", c.ReconcileInterval},
		{"expired-running sweep interval", c.ExpiredRunningSweepInterval},
		{"reservation TTL", c.ReservationTTL},
		{"reservation heartbeat", c.ReservationHeartbeat},
		{"prepare timeout", c.PrepareTimeout},
		{"provisional TTL", c.ProvisionalTTL},
		{"processing-turn TTL", c.ProcessingTurnTTL},
		{"recovery drain timeout", c.RecoveryDrainTimeout},
		{"dispatch interval", c.DispatchInterval},
		{"publish-attempt timeout", c.PublishAttemptTimeout},
	}
	for _, item := range durations {
		if item.value <= 0 || item.value > maxResourceDuration {
			return fmt.Errorf("fairqueue: %s must be in (0,%s]", item.name, maxResourceDuration)
		}
	}
	if c.ReservationHeartbeat >= c.ReservationTTL {
		return errors.New("fairqueue: reservation heartbeat must be shorter than reservation TTL")
	}
	if c.PrepareTimeout >= c.ProvisionalTTL {
		return errors.New("fairqueue: prepare timeout must be shorter than provisional TTL")
	}
	if c.ProvisionalTTL >= c.RecoveryDrainTimeout {
		return errors.New("fairqueue: provisional TTL must be shorter than recovery drain timeout")
	}
	if c.ProcessingTurnTTL >= c.RecoveryDrainTimeout {
		return errors.New("fairqueue: processing-turn TTL must be shorter than recovery drain timeout")
	}
	if c.PublishAttemptTimeout >= c.RecoveryDrainTimeout {
		return errors.New("fairqueue: publish-attempt timeout must be shorter than recovery drain timeout")
	}
	return nil
}

type Registry struct {
	resources map[string]ResourceConfig
}

func NewRegistry(configs ...ResourceConfig) (*Registry, error) {
	registry := &Registry{resources: make(map[string]ResourceConfig, len(configs))}
	for _, config := range configs {
		if err := config.Validate(); err != nil {
			return nil, fmt.Errorf("fairqueue: invalid resource %q: %w", config.Key, err)
		}
		if _, exists := registry.resources[config.Key]; exists {
			return nil, fmt.Errorf("fairqueue: duplicate resource %q", config.Key)
		}
		registry.resources[config.Key] = config
	}
	return registry, nil
}

func (r *Registry) Lookup(resource string) (ResourceConfig, bool) {
	if r == nil {
		return ResourceConfig{}, false
	}
	config, ok := r.resources[resource]
	return config, ok
}

func (r *Registry) ValidateMessage(message Message) error {
	if err := message.Validate(); err != nil {
		return err
	}
	config, ok := r.Lookup(message.Resource)
	if !ok {
		return fmt.Errorf("fairqueue: unknown resource %q", message.Resource)
	}
	if err := validateTaskIDShape(message.TaskID); err != nil {
		return err
	}
	if !config.ValidateTaskID(message.TaskID) {
		return fmt.Errorf("fairqueue: invalid task ID for resource %q", message.Resource)
	}
	return nil
}

func ValidateResource(resource string) error {
	if len(resource) == 0 || len(resource) > maxResourceBytes || !resourcePattern.MatchString(resource) {
		return errors.New("fairqueue: resource must be 1..120 canonical ASCII bytes")
	}
	return nil
}

func ValidateTenantID(tenant string) error {
	if err := validateBoundedText("tenant ID", tenant, maxTenantBytes, false); err != nil {
		return err
	}
	return nil
}

func ValidateTaskType(taskType string) error {
	if !taskTypePattern.MatchString(taskType) {
		return errors.New("fairqueue: task type must be canonical ASCII")
	}
	return nil
}

func ValidateTaskID(taskID string) error {
	return validateTaskIDShape(taskID)
}

func validateTaskIDShape(taskID string) error {
	return validateBoundedText("task ID", taskID, maxTaskIDBytes, false)
}

func validateBoundedText(name, value string, maxBytes int, allowEmpty bool) error {
	if (!allowEmpty && value == "") || len(value) > maxBytes {
		return fmt.Errorf("fairqueue: %s must be %s%d UTF-8 bytes", name, map[bool]string{true: "at most ", false: "1.."}[allowEmpty], maxBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("fairqueue: %s is not valid UTF-8", name)
	}
	for _, character := range value {
		if character == 0 || unicode.IsControl(character) {
			return fmt.Errorf("fairqueue: %s contains a control character", name)
		}
	}
	return nil
}

// ValidateRAGIndexTaskID accepts only the canonical base-10 representation of
// a positive signed BIGINT. Generic transport code must register this validator
// explicitly instead of assuming every resource uses numeric IDs.
func ValidateRAGIndexTaskID(taskID string) bool {
	if err := validateTaskIDShape(taskID); err != nil || taskID == "0" || taskID[0] == '0' {
		return false
	}
	for _, character := range taskID {
		if character < '0' || character > '9' {
			return false
		}
	}
	value, err := strconv.ParseUint(taskID, 10, 63)
	return err == nil && value > 0 && value <= math.MaxInt64
}

func ValidatePageLimit(limit int) error {
	if limit <= 0 || limit > maxRecoveryPageSize {
		return fmt.Errorf("fairqueue: page limit must be in 1..%d", maxRecoveryPageSize)
	}
	return nil
}

func ValidateCursor(cursor string) error {
	return validateBoundedText("cursor", cursor, maxCursorBytes, true)
}

func ValidateHighWater(highWater string) error {
	if err := validateBoundedText("high water", highWater, maxHighWaterBytes, false); err != nil {
		return err
	}
	// MySQL journals this value in an ascii_bin column. Retain opaque printable
	// ASCII exactly, while rejecting values that a boundary might normalize.
	if strings.TrimSpace(highWater) != highWater {
		return errors.New("fairqueue: high water must not have surrounding whitespace")
	}
	for i := 0; i < len(highWater); i++ {
		if highWater[i] < 0x20 || highWater[i] > 0x7e {
			return errors.New("fairqueue: high water must be printable ASCII")
		}
	}
	return nil
}

func TenantHash(resource, tenant string) (string, error) {
	if err := ValidateResource(resource); err != nil {
		return "", err
	}
	if err := ValidateTenantID(tenant); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(resource + "\x00" + tenant))
	return hex.EncodeToString(digest[:]), nil
}

func TenantQueueName(resource, tenant string) (string, error) {
	hash, err := TenantHash(resource, tenant)
	if err != nil {
		return "", err
	}
	return tenantQueuePrefix + resource + "." + hash, nil
}

func TenantRoutingKey(resource, tenant string) (string, error) {
	hash, err := TenantHash(resource, tenant)
	if err != nil {
		return "", err
	}
	return resource + "." + hash, nil
}

func DeadLetterQueueName(resource string) (string, error) {
	if err := ValidateResource(resource); err != nil {
		return "", err
	}
	return deadLetterQueuePrefix + resource, nil
}

func StableReservationToken(resource, taskID string, claimGeneration uint64) (string, error) {
	if err := ValidateResource(resource); err != nil {
		return "", err
	}
	if err := validateTaskIDShape(taskID); err != nil {
		return "", err
	}
	if claimGeneration == 0 || claimGeneration > math.MaxInt64 {
		return "", errors.New("fairqueue: claim generation must be in 1..MaxInt64")
	}
	digest := sha256.Sum256([]byte(resource + "\x00" + taskID + "\x00" + strconv.FormatUint(claimGeneration, 10)))
	return "r:" + hex.EncodeToString(digest[:]), nil
}
