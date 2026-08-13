package fairqueue

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestPolicyReservationDecision(t *testing.T) {
	limits := CapacityLimits{
		GlobalConcurrency:       8,
		PerUserBaseConcurrency:  2,
		PerUserBurstConcurrency: 4,
		BorrowEnabled:           true,
	}
	tests := []struct {
		name     string
		snapshot CapacitySnapshot
		want     ReservationDecision
	}{
		{"below base is regular", CapacitySnapshot{3, 1, 3}, ReservationRegular},
		{"sole active borrows", CapacitySnapshot{3, 2, 1}, ReservationBorrowed},
		{"competition denies borrowing", CapacitySnapshot{3, 2, 2}, ReservationDeniedCompetition},
		{"global full has priority", CapacitySnapshot{8, 4, 1}, ReservationDeniedGlobalFull},
		{"tenant burst has priority over borrowing", CapacitySnapshot{4, 4, 1}, ReservationDeniedTenantBurst},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DecideReservation(limits, test.snapshot)
			if err != nil || got != test.want {
				t.Fatalf("DecideReservation() = %q, %v; want %q", got, err, test.want)
			}
		})
	}

	withoutBorrow := limits
	withoutBorrow.BorrowEnabled = false
	if got, err := DecideReservation(withoutBorrow, CapacitySnapshot{3, 2, 1}); err != nil || got != ReservationDeniedBorrowOff {
		t.Fatalf("borrow disabled = %q, %v", got, err)
	}

	equalBaseBurst := limits
	equalBaseBurst.PerUserBurstConcurrency = equalBaseBurst.PerUserBaseConcurrency
	if got, err := DecideReservation(equalBaseBurst, CapacitySnapshot{3, 2, 1}); err != nil || got != ReservationDeniedTenantBurst {
		t.Fatalf("base=burst = %q, %v", got, err)
	}
}

func TestPolicyRejectsInvalidState(t *testing.T) {
	valid := CapacityLimits{8, 2, 4, true}
	tests := []struct {
		name     string
		limits   CapacityLimits
		snapshot CapacitySnapshot
	}{
		{"zero global", CapacityLimits{0, 1, 1, true}, CapacitySnapshot{0, 0, 1}},
		{"base above burst", CapacityLimits{8, 3, 2, true}, CapacitySnapshot{0, 0, 1}},
		{"burst above global", CapacityLimits{2, 1, 3, true}, CapacitySnapshot{0, 0, 1}},
		{"negative global", valid, CapacitySnapshot{-1, 0, 1}},
		{"negative tenant", valid, CapacitySnapshot{0, -1, 1}},
		{"tenant exceeds global", valid, CapacitySnapshot{1, 2, 1}},
		{"no active tenant", valid, CapacitySnapshot{0, 0, 0}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if decision, err := DecideReservation(test.limits, test.snapshot); err == nil || decision != "" {
				t.Fatalf("DecideReservation() = %q, %v; want error", decision, err)
			}
		})
	}
}

func validTestResourceConfig() ResourceConfig {
	return ResourceConfig{
		Key:                         "rag.index",
		ValidateTaskID:              ValidateRAGIndexTaskID,
		LocalWorkers:                16,
		GlobalConcurrency:           8,
		PerUserBaseConcurrency:      2,
		PerUserBurstConcurrency:     4,
		BorrowEnabled:               true,
		ReconcileInterval:           time.Second,
		ExpiredRunningSweepInterval: 2 * time.Second,
		ReconcilePageSize:           100,
		ReservationTTL:              30 * time.Second,
		ReservationHeartbeat:        10 * time.Second,
		PrepareTimeout:              5 * time.Second,
		ProvisionalTTL:              15 * time.Second,
		ProcessingTurnTTL:           20 * time.Second,
		RecoveryDrainTimeout:        2 * time.Minute,
		DispatchInterval:            time.Second,
		PublishAttemptTimeout:       10 * time.Second,
	}
}

func TestMessageConfigValidation(t *testing.T) {
	valid := validTestResourceConfig()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if valid.LocalWorkers <= valid.GlobalConcurrency {
		t.Fatal("fixture must prove local workers need not be bounded by global concurrency")
	}

	invalid := []struct {
		name   string
		mutate func(*ResourceConfig)
	}{
		{"resource", func(config *ResourceConfig) { config.Key = "RAG Index" }},
		{"validator", func(config *ResourceConfig) { config.ValidateTaskID = nil }},
		{"workers", func(config *ResourceConfig) { config.LocalWorkers = 0 }},
		{"base", func(config *ResourceConfig) { config.PerUserBaseConcurrency = 5 }},
		{"burst", func(config *ResourceConfig) { config.PerUserBurstConcurrency = 9 }},
		{"page", func(config *ResourceConfig) { config.ReconcilePageSize = maxRecoveryPageSize + 1 }},
		{"duration", func(config *ResourceConfig) { config.DispatchInterval = 25 * time.Hour }},
		{"heartbeat", func(config *ResourceConfig) { config.ReservationHeartbeat = config.ReservationTTL }},
		{"prepare", func(config *ResourceConfig) { config.PrepareTimeout = config.ProvisionalTTL }},
		{"provisional drain", func(config *ResourceConfig) { config.ProvisionalTTL = config.RecoveryDrainTimeout }},
		{"turn drain", func(config *ResourceConfig) { config.ProcessingTurnTTL = config.RecoveryDrainTimeout }},
		{"publish drain", func(config *ResourceConfig) { config.PublishAttemptTimeout = config.RecoveryDrainTimeout }},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("Validate() succeeded")
			}
		})
	}

	if _, err := NewRegistry(valid, valid); err == nil {
		t.Fatal("duplicate registry resource accepted")
	}
	if _, err := NewRegistry(); err != nil {
		t.Fatalf("empty registry: %v", err)
	}
}

func TestMessageResourceSpecificTaskIDValidation(t *testing.T) {
	valid := []string{"1", "9", "12345", "9223372036854775807"}
	for _, taskID := range valid {
		if !ValidateRAGIndexTaskID(taskID) {
			t.Fatalf("valid RAG task ID %q rejected", taskID)
		}
	}
	invalid := []string{"", "0", "-1", "+1", "01", "1.0", "9223372036854775808"}
	for _, taskID := range invalid {
		if ValidateRAGIndexTaskID(taskID) {
			t.Fatalf("invalid RAG task ID %q accepted", taskID)
		}
	}

	alwaysTrue := validTestResourceConfig()
	alwaysTrue.Key = "image.generate"
	alwaysTrue.ValidateTaskID = func(string) bool { return true }
	registry, err := NewRegistry(alwaysTrue)
	if err != nil {
		t.Fatal(err)
	}
	message := Message{
		Version:  ProtocolVersion,
		Resource: alwaysTrue.Key,
		TenantID: "tenant",
		TaskType: "image_generate",
		TaskID:   strings.Repeat("x", maxTaskIDBytes+1),
		DispatchToken: DispatchToken{
			Resource:   alwaysTrue.Key,
			TaskID:     strings.Repeat("x", maxTaskIDBytes+1),
			Generation: 1,
		},
	}
	if err := registry.ValidateMessage(message); err == nil {
		t.Fatal("resource validator bypassed the generic task ID bound")
	}
	message.TaskID = "imgt_abc"
	message.DispatchToken.TaskID = message.TaskID
	if err := registry.ValidateMessage(message); err != nil {
		t.Fatalf("generic non-numeric task ID rejected: %v", err)
	}
	unknown := message
	unknown.Resource = "rag.index"
	unknown.DispatchToken.Resource = unknown.Resource
	if err := registry.ValidateMessage(unknown); err == nil {
		t.Fatal("unregistered resource accepted")
	}
}

func TestQueueNameAndStableTokenVectors(t *testing.T) {
	hash, err := TenantHash("rag.index", "user_xxx")
	if err != nil {
		t.Fatal(err)
	}
	const wantHash = "7d189b6504b81e61bf6768bbe68aa338adac1c5713bfcab5cc65361ff7a5de7f"
	if hash != wantHash || len(hash) != sha256HexLength {
		t.Fatalf("TenantHash() = %q", hash)
	}
	queue, err := TenantQueueName("rag.index", "user_xxx")
	if err != nil || queue != "bkcrab.fair.q.rag.index."+wantHash {
		t.Fatalf("TenantQueueName() = %q, %v", queue, err)
	}
	for _, character := range queue {
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') && character != '.' && character != '-' && character != '_' {
			t.Fatalf("unsafe queue character %q", character)
		}
	}
	other, err := TenantHash("image.generate", "user_xxx")
	if err != nil || other == hash {
		t.Fatalf("resource-separated hash = %q, %v", other, err)
	}
	token, err := StableReservationToken("rag.index", "12345", 7)
	if err != nil {
		t.Fatal(err)
	}
	const wantToken = "r:2807d4ed5da1c308406baa91b3dc6fb03a866d87a878222cf7261d7e856b2087"
	if token != wantToken {
		t.Fatalf("StableReservationToken() = %q", token)
	}
	if _, err := StableReservationToken("rag.index", "12345", 0); err == nil {
		t.Fatal("zero claim generation accepted")
	}
	if _, err := StableReservationToken("rag.index", "12345", uint64(math.MaxInt64)+1); err == nil {
		t.Fatal("unsigned-only claim generation accepted")
	}
}

func TestMessageHighWaterMatchesCanonicalJournalDomain(t *testing.T) {
	if err := ValidateHighWater("page 42:opaque"); err != nil {
		t.Fatalf("opaque high water rejected: %v", err)
	}
	for _, highWater := range []string{" 42", "42 ", "   ", "42\n", "页42"} {
		if err := ValidateHighWater(highWater); err == nil {
			t.Fatalf("noncanonical high water %q accepted", highWater)
		}
	}
}

const sha256HexLength = 64
