package imagegen

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestFairQueueIntegrationMySQLCapacitySurvivesMissingRedisReservations(t *testing.T) {
	first := integrationMySQLStore(t)
	second := integrationMySQLStore(t)
	writer, err := first.ReadFairQueueWriterIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fairA, err := first.BindImageFairQueueWriter(writer.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	fairB, err := second.BindImageFairQueueWriter(writer.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var batches []string
	newService := func(fair *store.ImageFairQueueStore) *BatchService {
		return NewBatchService(BatchServiceOptions{Store: fair, ProviderPlans: integrationPlanResolver{},
			IDGenerator: func(kind string, _ int) string {
				prefix := "imgt_"
				if kind == "batch" {
					prefix = "imgb_"
				}
				id := integrationID(t, prefix)
				if kind == "batch" {
					batches = append(batches, id)
				}
				return id
			}, MaxImagesPerBatch: 16, MaxImagesPerTask: 4, MaxItems: 16, PromptMaxRunes: 8000, WaitMaxSeconds: 240})
	}
	t.Cleanup(func() { cleanupIntegrationBatch(t, first, batches...) })
	a, err := newService(fairA).Create(ctx, integrationIdentity("tenant-A", "agent-a"), integrationRequest(t, `{"prompt":"A backlog","count":16,"wait_seconds":0}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := newService(fairB).Create(ctx, integrationIdentity("tenant-B", "agent-b"), integrationRequest(t, `{"prompt":"B backlog","count":1,"wait_seconds":0}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range append(append([]BatchTaskResult(nil), a.Tasks...), b.Tasks...) {
		owner := fairA
		if task.TaskID == b.Tasks[0].TaskID {
			owner = fairB
		}
		candidate, err := owner.GetDispatchableImageTaskByID(ctx, task.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := owner.MarkImageTaskDispatched(ctx, *candidate, 1); err != nil || !changed {
			t.Fatalf("mark %s changed=%v err=%v", task.TaskID, changed, err)
		}
	}
	limits := store.ImageGenerationClaimLimits{GlobalConcurrency: 4, PerUserBurstConcurrency: 4, AdvisoryLockTimeout: time.Second}
	var claims []store.ImageGenerationFence
	for _, task := range a.Tasks {
		claimed, err := fairA.ClaimImageGenerationTaskByID(ctx, task.TaskID, "tenant-A", 1, "worker-a", time.Minute, limits)
		if err != nil || claimed.Disposition != store.ImageGenerationClaimed || claimed.Claim == nil {
			t.Fatalf("A claim=%+v err=%v", claimed, err)
		}
		claims = append(claims, claimed.Claim.Fence)
	}
	blocked, err := fairB.ClaimImageGenerationTaskByID(ctx, b.Tasks[0].TaskID, "tenant-B", 1, "worker-b", time.Minute, limits)
	if err != nil || blocked.Disposition != store.ImageGenerationClaimCapacityDeferred {
		t.Fatalf("fifth claim=%+v err=%v", blocked, err)
	}
	// This decision is entirely MySQL-authoritative. No Redis reservation was
	// created by this test, which models a flush/missing coordination state.
	if changed, err := fairA.FinishImageGenerationTaskFailed(ctx, claims[0], "released"); err != nil || !changed {
		t.Fatalf("release changed=%v err=%v", changed, err)
	}
	next, err := fairB.ClaimImageGenerationTaskByID(ctx, b.Tasks[0].TaskID, "tenant-B", 1, "worker-b", time.Minute, limits)
	if err != nil || next.Disposition != store.ImageGenerationClaimed {
		t.Fatalf("B did not receive next slot: %+v err=%v", next, err)
	}
	count, err := fairB.CountValidRunningImageGenerationTasks(ctx)
	if err != nil || count > 4 {
		t.Fatalf("valid running=%d err=%v", count, err)
	}
}

func TestFairQueueIntegrationUniqueRabbitRedisTopology(t *testing.T) {
	rabbitURL := os.Getenv("BKCRAB_TEST_RABBITMQ_URL")
	redisAddr := os.Getenv("BKCRAB_TEST_REDIS_ADDR")
	if rabbitURL == "" || redisAddr == "" {
		t.Skip("BKCRAB_TEST_RABBITMQ_URL and BKCRAB_TEST_REDIS_ADDR are required")
	}
	cfg := config.DefaultImagegenBatchCfg()
	resource := ImageFairQueueResourceConfig(cfg)
	registry, err := fairqueue.NewRegistry(resource)
	if err != nil {
		t.Fatal(err)
	}
	namespace := integrationID(t, "it_")
	rabbit, err := fairqueue.NewRabbit(fairqueue.RabbitOptions{URL: rabbitURL, Exchange: namespace + ".task", DeadLetterExchange: namespace + ".dlx", OperationTimeout: 5 * time.Second}, registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rabbit.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	probe, err := rabbit.ProbeResourceTopology(ctx, store.ImageGenerationResource)
	if err != nil || probe.Resource != store.ImageGenerationResource {
		t.Fatalf("Rabbit probe=%+v err=%v", probe, err)
	}
	coordinator, err := fairqueue.NewRedis(ctx, fairqueue.RedisOptions{Addr: redisAddr, Password: os.Getenv("BKCRAB_TEST_REDIS_PASSWORD"), KeyPrefix: namespace + ":", OperationTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	topology, err := coordinator.InspectRedisTopology(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !topology.SupportsFairQueue() {
		t.Fatal(errors.New("integration Redis must be a writable standalone primary"))
	}
}
