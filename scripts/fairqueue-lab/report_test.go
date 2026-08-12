package main

import (
	"testing"
	"time"
)

func TestEvaluateReportPassesWithDirectMultiTenantEvidence(t *testing.T) {
	now := time.Now().UTC()
	health := fairQueueHealth{Enabled: true, Mode: "fair", GateOpen: true, Status: "healthy"}
	health.Redis.GlobalInflight = 2
	report := &labReport{
		Config: labReportConfig{Users: 2, DocumentsPerUser: 1, ExpectedGlobal: 4, ExpectedPerUser: 4},
		Users:  []labUserReport{{ID: "u1", TenantHash: "h1"}, {ID: "u2", TenantHash: "h2"}},
		Documents: []documentObservation{
			{ID: "d1", UserID: "u1", LastStatus: "DONE", FirstActiveAt: timePtr(now)},
			{ID: "d2", UserID: "u2", LastStatus: "DONE", FirstActiveAt: timePtr(now.Add(time.Millisecond))},
		},
		Samples: []observationSample{{
			Health: &health,
			Redis:  &redisObservation{GlobalInflight: 2, TenantInflight: map[string]int64{"h1": 1, "h2": 1}},
			Rabbit: []rabbitQueueObservation{{TenantHash: "h1", PublishTotal: 1}, {TenantHash: "h2", PublishTotal: 1}},
		}},
	}

	checks, verdict := evaluateReport(report)
	if verdict != verdictPass {
		t.Fatalf("verdict=%s checks=%+v", verdict, checks)
	}
}

func TestEvaluateReportIsInconclusiveWithoutContentionOrDirectObservers(t *testing.T) {
	now := time.Now().UTC()
	report := &labReport{
		Config:    labReportConfig{Users: 2, DocumentsPerUser: 1, ExpectedGlobal: 4, ExpectedPerUser: 4},
		Users:     []labUserReport{{ID: "u1", TenantHash: "h1"}, {ID: "u2", TenantHash: "h2"}},
		Documents: []documentObservation{{ID: "d1", UserID: "u1", LastStatus: "DONE", FirstActiveAt: timePtr(now)}, {ID: "d2", UserID: "u2", LastStatus: "DONE", FirstActiveAt: timePtr(now)}},
		Samples:   []observationSample{{Health: &fairQueueHealth{Enabled: true, Mode: "fair", GateOpen: true, Status: "healthy"}}},
	}

	_, verdict := evaluateReport(report)
	if verdict != verdictInconclusive {
		t.Fatalf("verdict=%s, want %s", verdict, verdictInconclusive)
	}
}

func TestEvaluateReportAllowsTransientGatePauseWhenFinalHealthRecovers(t *testing.T) {
	now := time.Now().UTC()
	paused := fairQueueHealth{Enabled: true, Mode: "fair", GateOpen: false, Status: "degraded"}
	healthy := fairQueueHealth{Enabled: true, Mode: "fair", GateOpen: true, Status: "healthy"}
	healthy.Redis.GlobalInflight = 2
	report := &labReport{
		Config: labReportConfig{Users: 2, DocumentsPerUser: 1, ExpectedGlobal: 4, ExpectedPerUser: 4},
		Users:  []labUserReport{{ID: "u1", TenantHash: "h1"}, {ID: "u2", TenantHash: "h2"}},
		Documents: []documentObservation{
			{ID: "d1", UserID: "u1", LastStatus: "DONE", FirstActiveAt: timePtr(now)},
			{ID: "d2", UserID: "u2", LastStatus: "DONE", FirstActiveAt: timePtr(now.Add(time.Millisecond))},
		},
		Samples: []observationSample{
			{Health: &paused, Redis: &redisObservation{TenantInflight: map[string]int64{"h1": 1, "h2": 1}}},
			{Health: &healthy, Redis: &redisObservation{GlobalInflight: 2, TenantInflight: map[string]int64{"h1": 1, "h2": 1}}},
		},
	}

	checks, verdict := evaluateReport(report)
	if verdict != verdictPass {
		t.Fatalf("verdict=%s checks=%+v", verdict, checks)
	}
}

func TestEvaluateReportRequiresObservedArtifactsToBeCleaned(t *testing.T) {
	now := time.Now().UTC()
	health := fairQueueHealth{Enabled: true, Mode: "fair", GateOpen: true, Status: "healthy"}
	health.Redis.GlobalInflight = 2
	report := &labReport{
		Config: labReportConfig{
			Users: 2, DocumentsPerUser: 1, ExpectedGlobal: 4, ExpectedPerUser: 4,
			RedisObserved: true, RabbitObserved: true, CleanupEnabled: true,
		},
		Users: []labUserReport{
			{ID: "u1", TenantHash: "h1", Cleaned: true, RedisCleaned: true, RabbitCleaned: true},
			{ID: "u2", TenantHash: "h2", Cleaned: true, RedisCleaned: false, RabbitCleaned: true},
		},
		Documents: []documentObservation{
			{ID: "d1", UserID: "u1", LastStatus: "DONE", FirstActiveAt: timePtr(now)},
			{ID: "d2", UserID: "u2", LastStatus: "DONE", FirstActiveAt: timePtr(now.Add(time.Millisecond))},
		},
		Samples: []observationSample{{
			Health: &health,
			Redis:  &redisObservation{GlobalInflight: 2, TenantInflight: map[string]int64{"h1": 1, "h2": 1}},
			Rabbit: []rabbitQueueObservation{{TenantHash: "h1", PublishTotal: 1}, {TenantHash: "h2", PublishTotal: 1}},
		}},
	}

	checks, verdict := evaluateReport(report)
	if verdict != verdictFail {
		t.Fatalf("verdict=%s checks=%+v", verdict, checks)
	}
	for _, check := range checks {
		if check.Name == "cleanup_complete" && check.Status == verdictFail {
			return
		}
	}
	t.Fatalf("missing failed cleanup_complete check: %+v", checks)
}

func timePtr(value time.Time) *time.Time { return &value }
