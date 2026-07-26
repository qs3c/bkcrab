package rag

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
)

var errRAGKBProvisionFenceLost = errors.New("RAG knowledge-base provisioning fence lost")

const kbProvisioningFinalizeTimeout = 30 * time.Second

// ensureProvisionedKBCollection keeps the durable SQL lease alive for the
// entire external collection operation. A tombstone makes heartbeat fail,
// which cancels the Milvus request before cleanup is allowed to drop data.
func (s *Service) ensureProvisionedKBCollection(
	ctx context.Context,
	kb *store.RAGKBRecord,
	fence store.RAGKBProvisionFence,
) error {
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	done := make(chan struct{})
	leaseLost := make(chan struct{}, 1)
	stop := make(chan struct{})
	go func() {
		defer close(done)
		interval := s.heartbeatInterval
		if interval <= 0 || interval >= s.leaseDuration {
			interval = s.leaseDuration / 3
		}
		if interval <= 0 {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				cancelWork()
				return
			case <-ticker.C:
				heartbeatCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interval)
				ok, err := s.st.HeartbeatRAGKBProvisioning(heartbeatCtx, fence, s.leaseDuration)
				cancel()
				if err == nil && ok {
					continue
				}
				select {
				case leaseLost <- struct{}{}:
				default:
				}
				cancelWork()
				return
			}
		}
	}()

	err := s.vec.EnsureCollection(workCtx, kb.ID, kb.EmbedDims)
	close(stop)
	<-done
	select {
	case <-leaseLost:
		if err == nil {
			err = errRAGKBProvisionFenceLost
		}
	default:
	}
	return err
}

// abandonKBProvisioning is intentionally detached from the request
// cancellation. Once external provisioning has started, a bounded finalizer
// must either persist DELETING or leave the lease for crash recovery.
func (s *Service) abandonKBProvisioning(
	ctx context.Context,
	fence store.RAGKBProvisionFence,
) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kbProvisioningFinalizeTimeout)
	defer cancel()
	marked, aborted, err := s.st.AbortRAGKBProvisioning(cleanupCtx, fence)
	if err != nil {
		return err
	}
	if !aborted || marked == nil {
		return nil
	}
	return s.cleanupDeletingKB(cleanupCtx, marked)
}

func mapKBProvisioningStoreError(err error, maxKBs int) error {
	switch {
	case errors.Is(err, store.ErrRAGKBQuotaExceeded):
		return fmt.Errorf("%w: 每用户最多 %d 个知识库", ErrQuota, maxKBs)
	case errors.Is(err, store.ErrRAGLifecycleInactive):
		return ErrForbidden
	default:
		return err
	}
}
