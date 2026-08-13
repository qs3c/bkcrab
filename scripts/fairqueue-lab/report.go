package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	verdictPass         = "PASS"
	verdictFail         = "FAIL"
	verdictInconclusive = "INCONCLUSIVE"
)

type labReport struct {
	Version    int                   `json:"version"`
	StartedAt  time.Time             `json:"startedAt"`
	FinishedAt time.Time             `json:"finishedAt"`
	Config     labReportConfig       `json:"config"`
	Users      []labUserReport       `json:"users"`
	Documents  []documentObservation `json:"documents"`
	Samples    []observationSample   `json:"samples"`
	Checks     []verificationCheck   `json:"checks"`
	Verdict    string                `json:"verdict"`
	Errors     []string              `json:"errors,omitempty"`
	mu         sync.Mutex            `json:"-"`
}

type labReportConfig struct {
	BaseURL          string        `json:"baseUrl"`
	Users            int           `json:"users"`
	DocumentsPerUser int           `json:"documentsPerUser"`
	DocumentBytes    int           `json:"documentBytes"`
	PollInterval     time.Duration `json:"pollInterval"`
	Timeout          time.Duration `json:"timeout"`
	ExpectedGlobal   int64         `json:"expectedGlobal"`
	ExpectedPerUser  int64         `json:"expectedPerUser"`
	RedisObserved    bool          `json:"redisObserved"`
	RabbitObserved   bool          `json:"rabbitObserved"`
	CleanupEnabled   bool          `json:"cleanupEnabled"`
}

type labUserReport struct {
	Index         int    `json:"index"`
	ID            string `json:"id"`
	Username      string `json:"username"`
	TenantHash    string `json:"tenantHash"`
	KBID          string `json:"kbId"`
	Cleaned       bool   `json:"cleaned"`
	RedisCleaned  bool   `json:"redisCleaned"`
	RabbitCleaned bool   `json:"rabbitCleaned"`
}

type documentTransition struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"`
	Stage  string    `json:"stage"`
}

type documentObservation struct {
	ID            string               `json:"id"`
	UserID        string               `json:"userId"`
	Username      string               `json:"username"`
	FileName      string               `json:"fileName"`
	SubmittedAt   time.Time            `json:"submittedAt"`
	FirstActiveAt *time.Time           `json:"firstActiveAt,omitempty"`
	FinishedAt    *time.Time           `json:"finishedAt,omitempty"`
	LastStatus    string               `json:"lastStatus"`
	LastStage     string               `json:"lastStage"`
	Transitions   []documentTransition `json:"transitions"`
}

type verificationCheck struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

func newReport(cfg labConfig, started time.Time) *labReport {
	return &labReport{
		Version: 1, StartedAt: started, Verdict: verdictInconclusive,
		Config: labReportConfig{
			BaseURL: cfg.BaseURL, Users: cfg.Users, DocumentsPerUser: cfg.DocumentsPerUser,
			DocumentBytes: cfg.DocumentBytes, PollInterval: cfg.PollInterval, Timeout: cfg.Timeout,
			ExpectedGlobal: cfg.ExpectedGlobal, ExpectedPerUser: cfg.ExpectedPerUser,
			RedisObserved: cfg.RedisAddr != "", RabbitObserved: cfg.RabbitURL != "", CleanupEnabled: !cfg.Keep,
		},
	}
}

func (r *labReport) addDocument(document documentObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	document.Transitions = append(document.Transitions, documentTransition{At: document.SubmittedAt, Status: document.LastStatus, Stage: document.LastStage})
	if !pendingDocumentStatus(document.LastStatus, document.LastStage) {
		value := document.SubmittedAt
		document.FirstActiveAt = &value
	}
	if terminalDocumentStatus(document.LastStatus) {
		value := document.SubmittedAt
		document.FinishedAt = &value
	}
	r.Documents = append(r.Documents, document)
}

func (r *labReport) observeDocument(id, status, stage string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.Documents {
		document := &r.Documents[index]
		if document.ID != id {
			continue
		}
		if document.LastStatus == status && document.LastStage == stage {
			return
		}
		document.LastStatus, document.LastStage = status, stage
		document.Transitions = append(document.Transitions, documentTransition{At: at, Status: status, Stage: stage})
		if document.FirstActiveAt == nil && !pendingDocumentStatus(status, stage) {
			value := at
			document.FirstActiveAt = &value
		}
		if terminalDocumentStatus(status) {
			value := at
			document.FinishedAt = &value
		}
		return
	}
}

func (r *labReport) addSample(sample observationSample) {
	r.mu.Lock()
	r.Samples = append(r.Samples, sample)
	r.mu.Unlock()
}

func (r *labReport) addError(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Errors) == 0 || r.Errors[len(r.Errors)-1] != message {
		r.Errors = append(r.Errors, message)
	}
}

func (r *labReport) markCleaned(userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.Users {
		if r.Users[index].ID == userID {
			r.Users[index].Cleaned = true
			return
		}
	}
}

func (r *labReport) userCleaned(userID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.Users {
		if r.Users[index].ID == userID {
			return r.Users[index].Cleaned
		}
	}
	return false
}

func (r *labReport) markRedisCleaned(userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.Users {
		if r.Users[index].ID == userID {
			r.Users[index].RedisCleaned = true
			return
		}
	}
}

func (r *labReport) markRabbitCleaned(userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.Users {
		if r.Users[index].ID == userID {
			r.Users[index].RabbitCleaned = true
			return
		}
	}
}

func (r *labReport) finish(finished time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.FinishedAt = finished
	r.Checks, r.Verdict = evaluateReport(r)
}

func (r *labReport) marshal() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return json.MarshalIndent(r, "", "  ")
}

func evaluateReport(report *labReport) ([]verificationCheck, string) {
	checks := make([]verificationCheck, 0, 9)
	add := func(name, status, evidence string) {
		checks = append(checks, verificationCheck{Name: name, Status: status, Evidence: evidence})
	}

	healthSamples := 0
	fairConfigurationValid := true
	fairNeverFailed := true
	var finalHealth *fairQueueHealth
	maxGlobal, maxReady, maxDLQ := int64(0), int64(0), int64(0)
	maxTenant := make(map[string]int64)
	seenTenant := make(map[string]bool)
	directRedis := false
	directRabbit := false
	multiTenantConcurrent := false
	for _, sample := range report.Samples {
		if sample.Health != nil {
			healthSamples++
			health := sample.Health
			fairConfigurationValid = fairConfigurationValid && health.Enabled && health.Mode == "fair"
			fairNeverFailed = fairNeverFailed && health.Status != "failed"
			finalHealth = health
			maxGlobal = max(maxGlobal, health.Redis.GlobalInflight)
			maxReady = max(maxReady, health.Rabbit.ReadyDepth)
			maxDLQ = max(maxDLQ, health.Rabbit.DLQDepth)
		}
		if sample.Redis != nil {
			directRedis = true
			maxGlobal = max(maxGlobal, sample.Redis.GlobalInflight)
			concurrentTenants := 0
			for hash, value := range sample.Redis.TenantInflight {
				maxTenant[hash] = max(maxTenant[hash], value)
				seenTenant[hash] = seenTenant[hash] || value > 0
				if value > 0 {
					concurrentTenants++
				}
			}
			multiTenantConcurrent = multiTenantConcurrent || concurrentTenants >= 2
		}
		if sample.Rabbit != nil {
			directRabbit = true
			for _, queue := range sample.Rabbit {
				maxReady = max(maxReady, queue.Ready)
				if queue.Name == "bkcrab.fair.dlq.rag.index" {
					maxDLQ = max(maxDLQ, queue.Messages)
				}
				if queue.TenantHash != "" && (queue.Messages > 0 || queue.PublishTotal > 0 || queue.DeliverTotal > 0) {
					seenTenant[queue.TenantHash] = true
				}
			}
		}
	}
	if healthSamples == 0 {
		add("fair_mode_healthy", verdictInconclusive, "no admin health samples")
	} else if fairConfigurationValid && fairNeverFailed && finalHealth != nil && finalHealth.GateOpen && finalHealth.Status == "healthy" {
		add("fair_mode_healthy", verdictPass, "fair mode never failed and the final sample was healthy with an open gate")
	} else {
		add("fair_mode_healthy", verdictFail, "fair mode was disabled, failed, or did not recover to a healthy open gate")
	}

	expectedDocuments := report.Config.Users * report.Config.DocumentsPerUser
	if len(report.Documents) == expectedDocuments {
		add("load_created", verdictPass, "all requested documents were accepted")
	} else {
		add("load_created", verdictFail, "accepted document count differs from requested load")
	}

	if maxReady > 0 || maxGlobal >= min(report.Config.ExpectedGlobal, 2) {
		add("contention_observed", verdictPass, "queue or inflight concurrency showed overlapping work")
	} else {
		add("contention_observed", verdictInconclusive, "work completed too quickly to expose queue contention; increase document size/count")
	}

	if maxGlobal > report.Config.ExpectedGlobal {
		add("global_concurrency_ceiling", verdictFail, "observed global inflight exceeded the configured expectation")
	} else if healthSamples > 0 || directRedis {
		add("global_concurrency_ceiling", verdictPass, "observed global inflight stayed within the expected ceiling")
	} else {
		add("global_concurrency_ceiling", verdictInconclusive, "no Redis/global inflight samples")
	}

	perUserExceeded := false
	for _, value := range maxTenant {
		perUserExceeded = perUserExceeded || value > report.Config.ExpectedPerUser
	}
	if perUserExceeded {
		add("per_user_concurrency_ceiling", verdictFail, "at least one tenant exceeded the expected burst ceiling")
	} else if directRedis {
		add("per_user_concurrency_ceiling", verdictPass, "all directly sampled tenant reservations stayed within the expected ceiling")
	} else {
		add("per_user_concurrency_ceiling", verdictInconclusive, "enable -redis-addr for per-tenant reservation evidence")
	}

	participants := 0
	for _, user := range report.Users {
		if seenTenant[user.TenantHash] {
			participants++
		}
	}
	if participants == len(report.Users) && len(report.Users) > 0 {
		add("tenant_participation", verdictPass, "every temporary tenant appeared in Redis or RabbitMQ activity")
	} else if directRedis || directRabbit {
		add("tenant_participation", verdictFail, "not every temporary tenant appeared in direct queue observations")
	} else {
		add("tenant_participation", verdictInconclusive, "enable Redis or RabbitMQ observation to prove every tenant participated")
	}

	active := append([]documentObservation(nil), report.Documents...)
	sort.Slice(active, func(i, j int) bool {
		if active[i].FirstActiveAt == nil {
			return false
		}
		if active[j].FirstActiveAt == nil {
			return true
		}
		return active[i].FirstActiveAt.Before(*active[j].FirstActiveAt)
	})
	window := min(len(active), int(report.Config.ExpectedGlobal))
	firstUsers := make(map[string]struct{})
	for _, document := range active[:window] {
		if document.FirstActiveAt != nil {
			firstUsers[document.UserID] = struct{}{}
		}
	}
	if multiTenantConcurrent {
		add("early_interleaving", verdictPass, "Redis directly observed multiple tenants holding reservations at the same time")
	} else if window >= 2 && len(firstUsers) >= 2 {
		add("early_interleaving", verdictPass, "the first observed execution window contained multiple tenants")
	} else {
		add("early_interleaving", verdictInconclusive, "polling did not capture multiple tenants in the first execution window")
	}

	done, failed := 0, 0
	for _, document := range report.Documents {
		if strings.EqualFold(document.LastStatus, "DONE") {
			done++
		}
		if strings.EqualFold(document.LastStatus, "FAILED") {
			failed++
		}
	}
	if done == expectedDocuments {
		add("documents_completed", verdictPass, "all documents reached DONE")
	} else {
		add("documents_completed", verdictFail, "not all documents completed successfully")
	}
	if maxDLQ == 0 && failed == 0 {
		add("no_dlq_or_failures", verdictPass, "no DLQ depth or failed document was observed")
	} else {
		add("no_dlq_or_failures", verdictFail, "DLQ messages or failed documents were observed")
	}

	if report.Config.CleanupEnabled {
		usersCleaned, redisCleaned, rabbitCleaned := 0, 0, 0
		for _, user := range report.Users {
			if user.Cleaned {
				usersCleaned++
			}
			if user.RedisCleaned {
				redisCleaned++
			}
			if user.RabbitCleaned {
				rabbitCleaned++
			}
		}
		cleanupComplete := usersCleaned == len(report.Users)
		if report.Config.RedisObserved {
			cleanupComplete = cleanupComplete && redisCleaned == len(report.Users)
		}
		if report.Config.RabbitObserved {
			cleanupComplete = cleanupComplete && rabbitCleaned == len(report.Users)
		}
		evidence := "temporary users and directly observed Redis/RabbitMQ tenant artifacts were removed"
		if cleanupComplete && len(report.Users) > 0 {
			add("cleanup_complete", verdictPass, evidence)
		} else {
			add("cleanup_complete", verdictFail, "one or more temporary users or directly observed tenant artifacts remain")
		}
	}

	verdict := verdictPass
	for _, check := range checks {
		if check.Status == verdictFail {
			return checks, verdictFail
		}
		if check.Status == verdictInconclusive {
			verdict = verdictInconclusive
		}
	}
	return checks, verdict
}

func pendingDocumentStatus(status, stage string) bool {
	return strings.EqualFold(status, "PENDING") || strings.EqualFold(stage, "queued") || strings.EqualFold(stage, "pending")
}

func terminalDocumentStatus(status string) bool {
	return strings.EqualFold(status, "DONE") || strings.EqualFold(status, "FAILED") || strings.EqualFold(status, "DELETING")
}
