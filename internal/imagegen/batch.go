package imagegen

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/store"
)

type BatchStatus string

const (
	BatchStatusPending   BatchStatus = "PENDING"
	BatchStatusRunning   BatchStatus = "RUNNING"
	BatchStatusDone      BatchStatus = "DONE"
	BatchStatusPartial   BatchStatus = "PARTIAL"
	BatchStatusFailed    BatchStatus = "FAILED"
	BatchStatusCanceling BatchStatus = "CANCELING"
	BatchStatusCanceled  BatchStatus = "CANCELED"
)

type BatchStore interface {
	CreateImageGenerationBatch(ctx context.Context, request store.CreateImageGenerationBatchRequest) (*store.ImageGenerationBatchRecord, []store.ImageGenerationTaskRecord, error)
	GetImageGenerationBatchForPrincipal(ctx context.Context, userID, agentID, batchID string) (*store.ImageGenerationBatchRecord, error)
	ListImageGenerationTasks(ctx context.Context, batchID string) ([]store.ImageGenerationTaskRecord, error)
	RequestImageBatchCancel(ctx context.Context, userID, agentID, batchID string) (*store.ImageGenerationBatchRecord, []store.ImageGenerationTaskRecord, error)
}

type DispatchNotifier interface {
	TryDispatch(ctx context.Context, batchID string) error
}

type BatchClock interface {
	Now() time.Time
}

type WaitWakeup interface {
	Wait(ctx context.Context, batchID string, delay time.Duration) error
}

type ArtifactURLResolver interface {
	ResolveArtifactURL(ctx context.Context, scope ArtifactScope, key string) (string, error)
}

type BatchIDGenerator func(kind string, sequence int) string

type BatchServiceOptions struct {
	Store             BatchStore
	ProviderPlans     ProviderPlanResolver
	Dispatcher        DispatchNotifier
	Clock             BatchClock
	Wakeup            WaitWakeup
	ArtifactURLs      ArtifactURLResolver
	IDGenerator       BatchIDGenerator
	Jitter            func(time.Duration) time.Duration
	MaxImagesPerBatch int
	MaxImagesPerTask  int
	MaxItems          int
	PromptMaxRunes    int
	WaitMaxSeconds    int
	MaxRetries        int
	PollInterval      time.Duration
}

type BatchService struct {
	options BatchServiceOptions
}

type BatchArtifactResult struct {
	BatchID    string        `json:"batch_id"`
	TaskID     string        `json:"task_id"`
	ItemIndex  int           `json:"item_index"`
	ChunkIndex int           `json:"chunk_index"`
	Label      string        `json:"label"`
	Index      int           `json:"index"`
	Path       string        `json:"path"`
	URL        string        `json:"url,omitempty"`
	MIMEType   string        `json:"mime_type"`
	Size       int64         `json:"size"`
	Width      int           `json:"width"`
	Height     int           `json:"height"`
	SHA256     string        `json:"sha256"`
	Origin     ArtifactScope `json:"origin"`
}

type BatchTaskResult struct {
	TaskID         string      `json:"task_id"`
	ItemIndex      int         `json:"item_index"`
	ChunkIndex     int         `json:"chunk_index"`
	Label          string      `json:"label"`
	Status         BatchStatus `json:"status"`
	RequestedCount int         `json:"requested_count"`
	RetryCount     int         `json:"retry_count"`
	ErrorCode      string      `json:"error_code,omitempty"`
	ErrorMessage   string      `json:"error_message,omitempty"`
}

type BatchResult struct {
	BatchID         string                `json:"batch_id"`
	Status          BatchStatus           `json:"status"`
	RequestedCount  int                   `json:"requested_count"`
	SucceededCount  int                   `json:"succeeded_count"`
	FailedCount     int                   `json:"failed_count"`
	CanceledCount   int                   `json:"canceled_count"`
	CancelRequested bool                  `json:"cancel_requested"`
	CreatedAt       time.Time             `json:"created_at"`
	StartedAt       *time.Time            `json:"started_at,omitempty"`
	FinishedAt      *time.Time            `json:"finished_at,omitempty"`
	UpdatedAt       time.Time             `json:"updated_at"`
	Tasks           []BatchTaskResult     `json:"tasks,omitempty"`
	Artifacts       []BatchArtifactResult `json:"artifacts,omitempty"`
	StatusCall      string                `json:"status_call,omitempty"`
}

type realBatchClock struct{}

func (realBatchClock) Now() time.Time { return time.Now() }

type timerBatchWakeup struct{}

func (timerBatchWakeup) Wait(ctx context.Context, _ string, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var (
	canonicalTaskID     = regexp.MustCompile(`^imgt_[a-z0-9]{16,64}$`)
	statusURLPattern    = regexp.MustCompile(`https?://[^\s]+`)
	statusBearerPattern = regexp.MustCompile(`(?i)bearer\s+[^\s]+`)
	statusSecretPattern = regexp.MustCompile(`(?i)sk-[a-z0-9_-]+`)
)

func NewBatchService(options BatchServiceOptions) *BatchService {
	if options.Clock == nil {
		options.Clock = realBatchClock{}
	}
	if options.Wakeup == nil {
		options.Wakeup = timerBatchWakeup{}
	}
	if options.IDGenerator == nil {
		options.IDGenerator = randomBatchID
	}
	if options.Jitter == nil {
		options.Jitter = func(delay time.Duration) time.Duration { return delay }
	}
	if options.MaxImagesPerBatch <= 0 {
		options.MaxImagesPerBatch = 16
	}
	if options.MaxImagesPerTask <= 0 {
		options.MaxImagesPerTask = 4
	}
	if options.MaxItems <= 0 {
		options.MaxItems = 16
	}
	if options.PromptMaxRunes <= 0 {
		options.PromptMaxRunes = 8000
	}
	if options.WaitMaxSeconds <= 0 {
		options.WaitMaxSeconds = 240
	}
	if options.MaxRetries <= 0 {
		options.MaxRetries = 3
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	return &BatchService{options: options}
}

func (s *BatchService) Create(ctx context.Context, identity ExecutionIdentity, request NormalizedRequest) (BatchResult, error) {
	if err := s.validateCreate(identity, request); err != nil {
		return BatchResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return BatchResult{}, err
	}
	planned, err := PlanTasks(request, s.options.MaxImagesPerTask)
	if err != nil {
		return BatchResult{}, err
	}
	if s.options.ProviderPlans == nil {
		return BatchResult{}, errors.New("imagegen: provider plan resolver is required")
	}
	plan, err := s.options.ProviderPlans.Snapshot(ctx, identity)
	if err != nil {
		return BatchResult{}, err
	}
	if err := plan.Validate(); err != nil || !plan.MatchesIdentity(identity) {
		if err == nil {
			err = errors.New("imagegen: provider plan identity mismatch")
		}
		return BatchResult{}, err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return BatchResult{}, err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil || unsafeDurableProviderPlan(planJSON) {
		return BatchResult{}, errors.New("imagegen: provider plan is not safe to persist")
	}
	if s.options.Store == nil {
		return BatchResult{}, errors.New("imagegen: batch store is required")
	}
	batchID := s.options.IDGenerator("batch", 0)
	if !canonicalBatchID.MatchString(batchID) {
		return BatchResult{}, errors.New("imagegen: ID generator returned invalid batch ID")
	}
	storeRequest := store.CreateImageGenerationBatchRequest{
		BatchID: batchID, UserID: identity.UserID, ConfigUserID: identity.ConfigUserID,
		AgentOwnerUserID: identity.AgentOwnerUserID, AgentID: identity.AgentID,
		WorkspaceProjectID: identity.WorkspaceProjectID, WorkspaceSessionID: identity.WorkspaceSessionID,
		RequestJSON: requestJSON, ProviderPlanJSON: planJSON, RequestedCount: normalizedImageCount(request),
		MaxRetries: s.options.MaxRetries, Tasks: make([]store.CreateImageGenerationTaskRequest, 0, len(planned)),
	}
	for index, task := range planned {
		taskID := s.options.IDGenerator("task", index)
		if !canonicalTaskID.MatchString(taskID) {
			return BatchResult{}, errors.New("imagegen: ID generator returned invalid task ID")
		}
		storeRequest.Tasks = append(storeRequest.Tasks, store.CreateImageGenerationTaskRequest{
			ID: taskID, UserID: identity.UserID, ItemIndex: task.ItemIndex, ChunkIndex: task.ChunkIndex,
			Label: task.Label, Prompt: task.Prompt, Size: task.Size, RequestedCount: task.RequestedCount,
			RequestFingerprint: task.RequestFingerprint,
		})
	}
	batch, tasks, err := s.options.Store.CreateImageGenerationBatch(ctx, storeRequest)
	if err != nil {
		return BatchResult{}, err
	}
	result, err := s.resultFromRecords(ctx, batch, tasks)
	if err != nil {
		return BatchResult{}, err
	}
	if s.options.Dispatcher != nil {
		_ = s.options.Dispatcher.TryDispatch(ctx, batchID)
	}
	if request.WaitSeconds == 0 || ctx.Err() != nil || batchTerminal(result.Status) {
		return result, nil
	}
	return s.wait(ctx, identity, result, time.Duration(request.WaitSeconds)*time.Second)
}

func (s *BatchService) Status(ctx context.Context, identity ExecutionIdentity, batchID string) (BatchResult, error) {
	if err := identity.Validate(); err != nil {
		return BatchResult{}, err
	}
	if !canonicalBatchID.MatchString(batchID) || s == nil || s.options.Store == nil {
		return BatchResult{}, errors.New("imagegen: canonical batch ID and store are required")
	}
	batch, err := s.options.Store.GetImageGenerationBatchForPrincipal(ctx, identity.UserID, identity.AgentID, batchID)
	if err != nil {
		return BatchResult{}, err
	}
	tasks, err := s.options.Store.ListImageGenerationTasks(ctx, batchID)
	if err != nil {
		return BatchResult{}, err
	}
	return s.resultFromRecords(ctx, batch, tasks)
}

func (s *BatchService) Cancel(ctx context.Context, identity ExecutionIdentity, batchID string) (BatchResult, error) {
	if err := identity.Validate(); err != nil {
		return BatchResult{}, err
	}
	if !canonicalBatchID.MatchString(batchID) || s == nil || s.options.Store == nil {
		return BatchResult{}, errors.New("imagegen: canonical batch ID and store are required")
	}
	batch, tasks, err := s.options.Store.RequestImageBatchCancel(ctx, identity.UserID, identity.AgentID, batchID)
	if err != nil {
		return BatchResult{}, err
	}
	return s.resultFromRecords(ctx, batch, tasks)
}

func (s *BatchService) validateCreate(identity ExecutionIdentity, request NormalizedRequest) error {
	if s == nil {
		return errors.New("imagegen: batch service is required")
	}
	if err := identity.Validate(); err != nil {
		return err
	}
	if request.Version != RequestSchemaVersion || request.Action != ActionCreate || len(request.Items) < 1 || len(request.Items) > s.options.MaxItems ||
		request.WaitSeconds < 0 || request.WaitSeconds > s.options.WaitMaxSeconds {
		return errors.New("imagegen: invalid normalized create request")
	}
	total := 0
	labels := make(map[string]struct{}, len(request.Items))
	for index, item := range request.Items {
		if item.Index != index || strings.TrimSpace(item.Label) == "" || strings.TrimSpace(item.Prompt) == "" ||
			!utf8.ValidString(item.Prompt) || utf8.RuneCountInString(item.Prompt) > s.options.PromptMaxRunes || item.Count < 1 ||
			(item.Size != SizeSquare && item.Size != SizeLandscape && item.Size != SizePortrait) {
			return errors.New("imagegen: invalid normalized image item")
		}
		if _, exists := labels[item.Label]; exists {
			return errors.New("imagegen: duplicate normalized image label")
		}
		labels[item.Label] = struct{}{}
		total += item.Count
		if total > s.options.MaxImagesPerBatch {
			return errors.New("imagegen: image batch count exceeds configured maximum")
		}
	}
	if total < 1 {
		return errors.New("imagegen: image batch count is required")
	}
	return nil
}

func (s *BatchService) wait(ctx context.Context, identity ExecutionIdentity, current BatchResult, duration time.Duration) (BatchResult, error) {
	deadline := s.options.Clock.Now().Add(duration)
	for !batchTerminal(current.Status) && s.options.Clock.Now().Before(deadline) {
		if ctx.Err() != nil {
			return current, nil
		}
		delay := s.options.Jitter(s.options.PollInterval)
		remaining := deadline.Sub(s.options.Clock.Now())
		if delay <= 0 || delay > remaining {
			delay = remaining
		}
		waitErr := s.options.Wakeup.Wait(ctx, current.BatchID, delay)
		if ctx.Err() != nil {
			return current, nil
		}
		latest, err := s.Status(ctx, identity, current.BatchID)
		if err != nil {
			if waitErr != nil {
				return current, waitErr
			}
			return current, err
		}
		current = latest
	}
	return current, nil
}

func (s *BatchService) resultFromRecords(ctx context.Context, batch *store.ImageGenerationBatchRecord, tasks []store.ImageGenerationTaskRecord) (BatchResult, error) {
	if batch == nil {
		return BatchResult{}, errors.New("imagegen: batch record is required")
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].ItemIndex != tasks[j].ItemIndex {
			return tasks[i].ItemIndex < tasks[j].ItemIndex
		}
		if tasks[i].ChunkIndex != tasks[j].ChunkIndex {
			return tasks[i].ChunkIndex < tasks[j].ChunkIndex
		}
		return tasks[i].ID < tasks[j].ID
	})
	result := BatchResult{
		BatchID: batch.ID, Status: batchStatus(batch.Status), RequestedCount: batch.RequestedCount,
		SucceededCount: batch.SucceededCount, FailedCount: batch.FailedCount, CanceledCount: batch.CanceledCount,
		CancelRequested: batch.CancelRequested, CreatedAt: batch.CreatedAt, StartedAt: batch.StartedAt,
		FinishedAt: batch.FinishedAt, UpdatedAt: batch.UpdatedAt,
		StatusCall: fmt.Sprintf(`{"action":"status","batch_id":%q}`, batch.ID),
	}
	origin := ArtifactScope{AgentID: batch.AgentID, ProjectID: batch.WorkspaceProjectID, SessionID: batch.WorkspaceSessionID}
	for _, task := range tasks {
		result.Tasks = append(result.Tasks, BatchTaskResult{
			TaskID: task.ID, ItemIndex: task.ItemIndex, ChunkIndex: task.ChunkIndex, Label: task.Label,
			Status: taskStatus(task.Status), RequestedCount: task.RequestedCount, RetryCount: task.RetryCount,
			ErrorCode: boundedStatusError(task.ErrorCode), ErrorMessage: boundedStatusError(task.ErrorMessage),
		})
		if task.Status != store.ImageGenerationTaskDone || len(task.ArtifactsJSON) == 0 {
			continue
		}
		var artifacts []ImageArtifact
		if err := json.Unmarshal(task.ArtifactsJSON, &artifacts); err != nil || len(artifacts) != task.RequestedCount {
			return BatchResult{}, errors.New("imagegen: stored artifact metadata is invalid")
		}
		claimPrefix := pathPrefixForTask(batch.ID, task.ID)
		for _, artifact := range artifacts {
			if len(result.Artifacts) >= 16 || artifact.Index < 0 || artifact.Index >= task.RequestedCount ||
				!strings.HasPrefix(artifact.Key, claimPrefix) || extensionForMIME(artifact.MIMEType) == "" || artifact.Width < 1 || artifact.Height < 1 || artifact.Size < 1 || !artifactFingerprint.MatchString(artifact.SHA256) {
				return BatchResult{}, errors.New("imagegen: stored artifact metadata failed validation")
			}
			item := BatchArtifactResult{
				BatchID: batch.ID, TaskID: task.ID, ItemIndex: task.ItemIndex, ChunkIndex: task.ChunkIndex,
				Label: task.Label, Index: artifact.Index, Path: artifact.Key, MIMEType: artifact.MIMEType,
				Size: artifact.Size, Width: artifact.Width, Height: artifact.Height, SHA256: artifact.SHA256, Origin: origin,
			}
			if s.options.ArtifactURLs != nil {
				item.URL, _ = s.options.ArtifactURLs.ResolveArtifactURL(ctx, origin, artifact.Key)
			}
			result.Artifacts = append(result.Artifacts, item)
		}
	}
	return result, nil
}

func randomBatchID(kind string, _ int) string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return ""
	}
	prefix := "imgt_"
	if kind == "batch" {
		prefix = "imgb_"
	}
	return prefix + hex.EncodeToString(data)
}

func normalizedImageCount(request NormalizedRequest) int {
	total := 0
	for _, item := range request.Items {
		total += item.Count
	}
	return total
}

func unsafeDurableProviderPlan(encoded []byte) bool {
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"apikey", "authorization", "bearer ", "secretkey", "access token", "sk-"} {
		if strings.Contains(lower, forbidden) {
			return true
		}
	}
	return false
}

func batchStatus(status store.ImageGenerationBatchStatus) BatchStatus { return BatchStatus(status) }
func taskStatus(status store.ImageGenerationTaskStatus) BatchStatus   { return BatchStatus(status) }

func batchTerminal(status BatchStatus) bool {
	return status == BatchStatusDone || status == BatchStatusPartial || status == BatchStatusFailed || status == BatchStatusCanceled
}

func pathPrefixForTask(batchID, taskID string) string {
	return "imagegen/" + batchID + "/" + taskID + "/claims/"
}

func boundedStatusError(value string) string {
	value = statusBearerPattern.ReplaceAllString(strings.TrimSpace(value), "[redacted]")
	value = statusSecretPattern.ReplaceAllString(value, "[redacted]")
	value = statusURLPattern.ReplaceAllStringFunc(value, func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "[url]"
		}
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	})
	if len(value) <= 256 {
		return value
	}
	cut := 256
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut]
}
