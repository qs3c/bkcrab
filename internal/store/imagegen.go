package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type ImageGenerationBatchStatus string

const (
	ImageGenerationBatchPending   ImageGenerationBatchStatus = "PENDING"
	ImageGenerationBatchRunning   ImageGenerationBatchStatus = "RUNNING"
	ImageGenerationBatchDone      ImageGenerationBatchStatus = "DONE"
	ImageGenerationBatchPartial   ImageGenerationBatchStatus = "PARTIAL"
	ImageGenerationBatchFailed    ImageGenerationBatchStatus = "FAILED"
	ImageGenerationBatchCanceling ImageGenerationBatchStatus = "CANCELING"
	ImageGenerationBatchCanceled  ImageGenerationBatchStatus = "CANCELED"
)

type ImageGenerationTaskStatus string

const (
	ImageGenerationTaskPending  ImageGenerationTaskStatus = "PENDING"
	ImageGenerationTaskRunning  ImageGenerationTaskStatus = "RUNNING"
	ImageGenerationTaskDone     ImageGenerationTaskStatus = "DONE"
	ImageGenerationTaskFailed   ImageGenerationTaskStatus = "FAILED"
	ImageGenerationTaskCanceled ImageGenerationTaskStatus = "CANCELED"
)

var (
	imageBatchIDPattern    = regexp.MustCompile(`^imgb_[a-z0-9]{16,64}$`)
	imageTaskIDPattern     = regexp.MustCompile(`^imgt_[a-z0-9]{16,64}$`)
	lowerHex64ImagePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type ImageGenerationBatchRecord struct {
	ID                 string
	UserID             string
	ConfigUserID       string
	AgentOwnerUserID   string
	AgentID            string
	WorkspaceProjectID string
	WorkspaceSessionID string
	RequestJSON        json.RawMessage
	ProviderPlanJSON   json.RawMessage
	Status             ImageGenerationBatchStatus
	RequestedCount     int
	SucceededCount     int
	FailedCount        int
	CanceledCount      int
	CancelRequested    bool
	ErrorMessage       string
	CreatedAt          time.Time
	StartedAt          *time.Time
	FinishedAt         *time.Time
	UpdatedAt          time.Time
}

type ImageGenerationTaskRecord struct {
	ID                 string
	SequenceID         int64
	BatchID            string
	UserID             string
	ItemIndex          int
	ChunkIndex         int
	Label              string
	Prompt             string
	Size               string
	RequestedCount     int
	RequestFingerprint string
	Status             ImageGenerationTaskStatus
	RetryCount         int
	MaxRetry           int
	ClaimGeneration    int64
	DispatchGeneration int64
	LeaseOwner         string
	LeaseUntil         *time.Time
	HeartbeatAt        *time.Time
	NextRunAt          *time.Time
	DispatchedAt       *time.Time
	Provider           string
	Model              string
	ManifestKey        string
	ArtifactsJSON      json.RawMessage
	ErrorCode          string
	ErrorMessage       string
	CreatedAt          time.Time
	StartedAt          *time.Time
	FinishedAt         *time.Time
	UpdatedAt          time.Time
}

type CreateImageGenerationTaskRequest struct {
	ID                 string
	UserID             string
	ItemIndex          int
	ChunkIndex         int
	Label              string
	Prompt             string
	Size               string
	RequestedCount     int
	RequestFingerprint string
}

type CreateImageGenerationBatchRequest struct {
	BatchID            string
	UserID             string
	ConfigUserID       string
	AgentOwnerUserID   string
	AgentID            string
	WorkspaceProjectID string
	WorkspaceSessionID string
	RequestJSON        json.RawMessage
	ProviderPlanJSON   json.RawMessage
	RequestedCount     int
	MaxRetries         int
	Tasks              []CreateImageGenerationTaskRequest
}

type ImageTaskDoneResult struct {
	Provider      string
	Model         string
	ManifestKey   string
	ArtifactsJSON json.RawMessage
}

type imageBatchAggregate struct {
	Status         ImageGenerationBatchStatus
	SucceededCount int
	FailedCount    int
	CanceledCount  int
}

const imageBatchColumns = `id,user_id,config_user_id,agent_owner_user_id,agent_id,
	workspace_project_id,workspace_session_id,request_json,provider_plan_json,status,
	requested_count,succeeded_count,failed_count,canceled_count,cancel_requested,error_msg,
	created_at,started_at,finished_at,updated_at`

const imageTaskColumns = `id,sequence_id,batch_id,user_id,item_index,chunk_index,label,prompt,size,
	requested_count,request_fingerprint,status,retry_count,max_retry,claim_generation,
	dispatch_generation,lease_owner,lease_until,heartbeat_at,next_run_at,dispatched_at,
	provider,model,manifest_key,artifacts_json,error_code,error_msg,created_at,started_at,
	finished_at,updated_at`

func (d *DBStore) requireImagegenMySQL() error {
	if d == nil || d.db == nil || d.dialect != mysqlDialect {
		return errors.New("store: image generation batches require MySQL")
	}
	return nil
}

func validateCreateImageGenerationBatch(request CreateImageGenerationBatchRequest) error {
	if !imageBatchIDPattern.MatchString(request.BatchID) {
		return errors.New("store: canonical image batch ID is required")
	}
	for name, value := range map[string]string{
		"user_id": request.UserID, "config_user_id": request.ConfigUserID,
		"agent_owner_user_id": request.AgentOwnerUserID, "agent_id": request.AgentID,
	} {
		if strings.TrimSpace(value) == "" || len(value) > 120 {
			return fmt.Errorf("store: image batch %s is required and bounded", name)
		}
	}
	if !validVersionedJSONObject(request.RequestJSON) || !validVersionedJSONObject(request.ProviderPlanJSON) {
		return errors.New("store: image batch request and provider plan must be versioned JSON objects")
	}
	if imageJSONContainsSecret(request.ProviderPlanJSON) {
		return errors.New("store: image provider plan contains secret material")
	}
	if request.RequestedCount < 1 || request.RequestedCount > 16 || len(request.Tasks) < 1 || len(request.Tasks) > 16 || request.MaxRetries < 0 {
		return errors.New("store: invalid image batch count, task count, or retry limit")
	}
	ids := make(map[string]struct{}, len(request.Tasks))
	chunks := make(map[[2]int]struct{}, len(request.Tasks))
	total := 0
	for index, task := range request.Tasks {
		if !imageTaskIDPattern.MatchString(task.ID) {
			return fmt.Errorf("store: task %d has invalid canonical ID", index)
		}
		if _, exists := ids[task.ID]; exists {
			return fmt.Errorf("store: duplicate image task ID %q", task.ID)
		}
		ids[task.ID] = struct{}{}
		key := [2]int{task.ItemIndex, task.ChunkIndex}
		if task.ItemIndex < 0 || task.ChunkIndex < 0 {
			return errors.New("store: image task indices must be non-negative")
		}
		if _, exists := chunks[key]; exists {
			return fmt.Errorf("store: duplicate image task chunk %v", key)
		}
		chunks[key] = struct{}{}
		if task.UserID != request.UserID || task.RequestedCount < 1 || task.RequestedCount > 4 ||
			strings.TrimSpace(task.Prompt) == "" || len(task.Size) > 64 ||
			!lowerHex64ImagePattern.MatchString(task.RequestFingerprint) {
			return fmt.Errorf("store: invalid image task %d", index)
		}
		total += task.RequestedCount
	}
	if total != request.RequestedCount {
		return fmt.Errorf("store: requested_count %d does not equal task total %d", request.RequestedCount, total)
	}
	return nil
}

func validJSONObject(raw json.RawMessage) bool {
	if !json.Valid(raw) {
		return false
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func validVersionedJSONObject(raw json.RawMessage) bool {
	if !validJSONObject(raw) {
		return false
	}
	var envelope struct {
		Version int `json:"version"`
	}
	return json.Unmarshal(raw, &envelope) == nil && envelope.Version > 0
}

func imageJSONContainsSecret(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	return imageValueContainsSecret(value)
}

func imageValueContainsSecret(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
			switch normalized {
			case "apikey", "authorization", "secretkey", "accesstoken", "token", "password":
				return true
			}
			if imageValueContainsSecret(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if imageValueContainsSecret(nested) {
				return true
			}
		}
	case string:
		lower := strings.ToLower(typed)
		return strings.Contains(lower, "bearer ") || strings.Contains(lower, "data:image") ||
			strings.Contains(lower, "api_key=") || strings.Contains(lower, "access_token=")
	}
	return false
}

func (d *DBStore) CreateImageGenerationBatch(ctx context.Context, request CreateImageGenerationBatchRequest) (*ImageGenerationBatchRecord, []ImageGenerationTaskRecord, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, nil, err
	}
	if err := validateCreateImageGenerationBatch(request); err != nil {
		return nil, nil, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT UTC_TIMESTAMP(6)`).Scan(&now); err != nil {
		return nil, nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO image_generation_batches
		(id,user_id,config_user_id,agent_owner_user_id,agent_id,workspace_project_id,
		 workspace_session_id,request_json,provider_plan_json,status,requested_count,
		 created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, request.BatchID, request.UserID, request.ConfigUserID,
		request.AgentOwnerUserID, request.AgentID, request.WorkspaceProjectID,
		request.WorkspaceSessionID, []byte(request.RequestJSON), []byte(request.ProviderPlanJSON),
		ImageGenerationBatchPending, request.RequestedCount, now, now)
	if err != nil {
		return nil, nil, err
	}
	tasks := make([]ImageGenerationTaskRecord, 0, len(request.Tasks))
	for _, input := range request.Tasks {
		result, err := tx.ExecContext(ctx, `INSERT INTO image_generation_tasks
			(id,batch_id,user_id,item_index,chunk_index,label,prompt,size,requested_count,
			 request_fingerprint,status,max_retry,claim_generation,dispatch_generation,
			 created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, input.ID, request.BatchID, input.UserID,
			input.ItemIndex, input.ChunkIndex, input.Label, input.Prompt, input.Size,
			input.RequestedCount, input.RequestFingerprint, ImageGenerationTaskPending,
			request.MaxRetries, 0, 1, now, now)
		if err != nil {
			return nil, nil, err
		}
		sequenceID, err := result.LastInsertId()
		if err != nil {
			return nil, nil, err
		}
		tasks = append(tasks, ImageGenerationTaskRecord{
			ID: input.ID, SequenceID: sequenceID, BatchID: request.BatchID, UserID: input.UserID,
			ItemIndex: input.ItemIndex, ChunkIndex: input.ChunkIndex, Label: input.Label,
			Prompt: input.Prompt, Size: input.Size, RequestedCount: input.RequestedCount,
			RequestFingerprint: input.RequestFingerprint, Status: ImageGenerationTaskPending,
			MaxRetry: request.MaxRetries, DispatchGeneration: 1, CreatedAt: now, UpdatedAt: now,
		})
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	batch := &ImageGenerationBatchRecord{
		ID: request.BatchID, UserID: request.UserID, ConfigUserID: request.ConfigUserID,
		AgentOwnerUserID: request.AgentOwnerUserID, AgentID: request.AgentID,
		WorkspaceProjectID: request.WorkspaceProjectID, WorkspaceSessionID: request.WorkspaceSessionID,
		RequestJSON:      append(json.RawMessage(nil), request.RequestJSON...),
		ProviderPlanJSON: append(json.RawMessage(nil), request.ProviderPlanJSON...),
		Status:           ImageGenerationBatchPending, RequestedCount: request.RequestedCount,
		CreatedAt: now, UpdatedAt: now,
	}
	return batch, tasks, nil
}

type imageBatchScanner interface{ Scan(...any) error }

func scanImageBatch(scanner imageBatchScanner) (*ImageGenerationBatchRecord, error) {
	var record ImageGenerationBatchRecord
	var requestJSON, planJSON []byte
	var errorMessage sql.NullString
	var startedAt, finishedAt sql.NullTime
	err := scanner.Scan(&record.ID, &record.UserID, &record.ConfigUserID, &record.AgentOwnerUserID,
		&record.AgentID, &record.WorkspaceProjectID, &record.WorkspaceSessionID, &requestJSON,
		&planJSON, &record.Status, &record.RequestedCount, &record.SucceededCount,
		&record.FailedCount, &record.CanceledCount, &record.CancelRequested, &errorMessage,
		&record.CreatedAt, &startedAt, &finishedAt, &record.UpdatedAt)
	if err != nil {
		return nil, scanErr(err)
	}
	record.RequestJSON = append(json.RawMessage(nil), requestJSON...)
	record.ProviderPlanJSON = append(json.RawMessage(nil), planJSON...)
	if errorMessage.Valid {
		record.ErrorMessage = errorMessage.String
	}
	if startedAt.Valid {
		value := startedAt.Time
		record.StartedAt = &value
	}
	if finishedAt.Valid {
		value := finishedAt.Time
		record.FinishedAt = &value
	}
	return &record, nil
}

func (d *DBStore) GetImageGenerationBatchForPrincipal(ctx context.Context, userID, agentID, batchID string) (*ImageGenerationBatchRecord, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, err
	}
	return scanImageBatch(d.db.QueryRowContext(ctx, `SELECT `+imageBatchColumns+`
		FROM image_generation_batches WHERE id=? AND user_id=? AND agent_id=?`, batchID, userID, agentID))
}

func (d *DBStore) getImageGenerationBatch(ctx context.Context, session interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, batchID string, forUpdate bool) (*ImageGenerationBatchRecord, error) {
	query := `SELECT ` + imageBatchColumns + ` FROM image_generation_batches WHERE id=?`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanImageBatch(session.QueryRowContext(ctx, query, batchID))
}

type imageTaskScanner interface{ Scan(...any) error }

func scanImageTask(scanner imageTaskScanner) (*ImageGenerationTaskRecord, error) {
	var record ImageGenerationTaskRecord
	var leaseOwner sql.NullString
	var provider, model, manifestKey, errorCode string
	var leaseUntil, heartbeatAt, nextRunAt, dispatchedAt, startedAt, finishedAt sql.NullTime
	var artifacts []byte
	var errorMessage sql.NullString
	err := scanner.Scan(&record.ID, &record.SequenceID, &record.BatchID, &record.UserID,
		&record.ItemIndex, &record.ChunkIndex, &record.Label, &record.Prompt, &record.Size,
		&record.RequestedCount, &record.RequestFingerprint, &record.Status, &record.RetryCount,
		&record.MaxRetry, &record.ClaimGeneration, &record.DispatchGeneration, &leaseOwner,
		&leaseUntil, &heartbeatAt, &nextRunAt, &dispatchedAt, &provider, &model, &manifestKey,
		&artifacts, &errorCode, &errorMessage, &record.CreatedAt, &startedAt, &finishedAt,
		&record.UpdatedAt)
	if err != nil {
		return nil, scanErr(err)
	}
	if leaseOwner.Valid {
		record.LeaseOwner = leaseOwner.String
	}
	record.Provider, record.Model, record.ManifestKey, record.ErrorCode = provider, model, manifestKey, errorCode
	if leaseUntil.Valid {
		value := leaseUntil.Time
		record.LeaseUntil = &value
	}
	if heartbeatAt.Valid {
		value := heartbeatAt.Time
		record.HeartbeatAt = &value
	}
	if nextRunAt.Valid {
		value := nextRunAt.Time
		record.NextRunAt = &value
	}
	if dispatchedAt.Valid {
		value := dispatchedAt.Time
		record.DispatchedAt = &value
	}
	if startedAt.Valid {
		value := startedAt.Time
		record.StartedAt = &value
	}
	if finishedAt.Valid {
		value := finishedAt.Time
		record.FinishedAt = &value
	}
	if artifacts != nil {
		record.ArtifactsJSON = append(json.RawMessage(nil), artifacts...)
	}
	if errorMessage.Valid {
		record.ErrorMessage = errorMessage.String
	}
	return &record, nil
}

func scanImageTaskRows(rows *sql.Rows) ([]ImageGenerationTaskRecord, error) {
	defer rows.Close()
	var records []ImageGenerationTaskRecord
	for rows.Next() {
		record, err := scanImageTask(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}

func (d *DBStore) ListImageGenerationTasks(ctx context.Context, batchID string) ([]ImageGenerationTaskRecord, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks
		WHERE batch_id=? ORDER BY item_index,chunk_index,id`, batchID)
	if err != nil {
		return nil, err
	}
	return scanImageTaskRows(rows)
}

func (d *DBStore) GetImageGenerationTask(ctx context.Context, taskID string) (*ImageGenerationTaskRecord, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, err
	}
	return scanImageTask(d.db.QueryRowContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks WHERE id=?`, taskID))
}

func computeImageBatchAggregate(cancelRequested bool, tasks []ImageGenerationTaskRecord) imageBatchAggregate {
	result := imageBatchAggregate{Status: ImageGenerationBatchPending}
	allTerminal := len(tasks) > 0
	hasRunning := false
	hasProgress := false
	for _, task := range tasks {
		switch task.Status {
		case ImageGenerationTaskDone:
			hasProgress = true
			result.SucceededCount += task.RequestedCount
		case ImageGenerationTaskFailed:
			hasProgress = true
			result.FailedCount += task.RequestedCount
		case ImageGenerationTaskCanceled:
			hasProgress = true
			result.CanceledCount += task.RequestedCount
		case ImageGenerationTaskRunning:
			hasRunning, hasProgress, allTerminal = true, true, false
		default:
			allTerminal = false
		}
	}
	if cancelRequested {
		if hasRunning {
			result.Status = ImageGenerationBatchCanceling
		} else {
			result.Status = ImageGenerationBatchCanceled
		}
		return result
	}
	if allTerminal {
		switch {
		case result.SucceededCount > 0 && result.FailedCount == 0 && result.CanceledCount == 0:
			result.Status = ImageGenerationBatchDone
		case result.SucceededCount > 0:
			result.Status = ImageGenerationBatchPartial
		case result.FailedCount > 0:
			result.Status = ImageGenerationBatchFailed
		default:
			result.Status = ImageGenerationBatchCanceled
		}
	} else if hasRunning || hasProgress {
		result.Status = ImageGenerationBatchRunning
	}
	return result
}

func imageBatchTerminal(status ImageGenerationBatchStatus) bool {
	return status == ImageGenerationBatchDone || status == ImageGenerationBatchPartial || status == ImageGenerationBatchFailed || status == ImageGenerationBatchCanceled
}

func imageTaskTerminal(status ImageGenerationTaskStatus) bool {
	return status == ImageGenerationTaskDone || status == ImageGenerationTaskFailed || status == ImageGenerationTaskCanceled
}

func (d *DBStore) MarkImageGenerationBatchStarted(ctx context.Context, batchID string) (*ImageGenerationBatchRecord, bool, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, false, err
	}
	result, err := d.db.ExecContext(ctx, `UPDATE image_generation_batches SET status='RUNNING',started_at=COALESCE(started_at,UTC_TIMESTAMP(6)),updated_at=UTC_TIMESTAMP(6) WHERE id=? AND status='PENDING'`, batchID)
	if err != nil {
		return nil, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	batch, err := d.getImageGenerationBatch(ctx, d.db, batchID, false)
	return batch, count == 1, err
}

func (d *DBStore) recomputeImageBatchOn(ctx context.Context, tx *sql.Tx, batch *ImageGenerationBatchRecord) (*ImageGenerationBatchRecord, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks WHERE batch_id=? ORDER BY item_index,chunk_index,id`, batch.ID)
	if err != nil {
		return nil, err
	}
	tasks, err := scanImageTaskRows(rows)
	if err != nil {
		return nil, err
	}
	aggregate := computeImageBatchAggregate(batch.CancelRequested, tasks)
	terminal := imageBatchTerminal(aggregate.Status)
	_, err = tx.ExecContext(ctx, `UPDATE image_generation_batches SET status=?,succeeded_count=?,failed_count=?,canceled_count=?,
		finished_at=CASE WHEN ? THEN COALESCE(finished_at,UTC_TIMESTAMP(6)) ELSE finished_at END,
		updated_at=UTC_TIMESTAMP(6) WHERE id=?`, aggregate.Status, aggregate.SucceededCount,
		aggregate.FailedCount, aggregate.CanceledCount, terminal, batch.ID)
	if err != nil {
		return nil, err
	}
	return d.getImageGenerationBatch(ctx, tx, batch.ID, false)
}

type imageTaskFinalizeKind int

const (
	imageFinalizeDone imageTaskFinalizeKind = iota
	imageFinalizeFailed
	imageFinalizeCanceled
)

func (d *DBStore) finalizeImageTask(ctx context.Context, taskID string, kind imageTaskFinalizeKind, done ImageTaskDoneResult, errorCode, errorMessage string) (*ImageGenerationBatchRecord, bool, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, false, err
	}
	if kind == imageFinalizeDone && (!validJSONValue(done.ArtifactsJSON) || strings.TrimSpace(done.ManifestKey) == "") {
		return nil, false, errors.New("store: DONE image task requires artifacts JSON and manifest key")
	}
	var batchID string
	if err := d.db.QueryRowContext(ctx, `SELECT batch_id FROM image_generation_tasks WHERE id=?`, taskID).Scan(&batchID); err != nil {
		return nil, false, scanErr(err)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	batch, err := d.getImageGenerationBatch(ctx, tx, batchID, true)
	if err != nil {
		return nil, false, err
	}
	task, err := scanImageTask(tx.QueryRowContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks WHERE id=? FOR UPDATE`, taskID))
	if err != nil {
		return nil, false, err
	}
	if kind == imageFinalizeDone {
		artifactCount, valid := imageArtifactCount(done.ArtifactsJSON)
		if !valid || artifactCount != task.RequestedCount {
			return nil, false, fmt.Errorf("store: DONE image task requires exactly %d artifacts, got %d", task.RequestedCount, artifactCount)
		}
	}
	changed := !imageTaskTerminal(task.Status)
	if changed {
		switch kind {
		case imageFinalizeDone:
			_, err = tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='DONE',provider=?,model=?,manifest_key=?,artifacts_json=?,error_code='',error_msg=NULL,lease_owner=NULL,lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,finished_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=?`, done.Provider, done.Model, done.ManifestKey, []byte(done.ArtifactsJSON), taskID)
		case imageFinalizeFailed:
			_, err = tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='FAILED',error_code=?,error_msg=?,lease_owner=NULL,lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,finished_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=?`, boundedImageError(errorCode, 64), boundedImageError(errorMessage, 2048), taskID)
		case imageFinalizeCanceled:
			_, err = tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='CANCELED',dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1,dispatched_at=NULL,lease_owner=NULL,lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,finished_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=?`, taskID)
		}
		if err != nil {
			return nil, false, err
		}
	}
	batch, err = d.recomputeImageBatchOn(ctx, tx, batch)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return batch, changed, nil
}

func validJSONValue(raw json.RawMessage) bool {
	return len(raw) > 0 && json.Valid(raw) && !strings.Contains(strings.ToLower(string(raw)), "data:image")
}

func imageArtifactCount(raw json.RawMessage) (int, bool) {
	if !validJSONValue(raw) {
		return 0, false
	}
	var artifacts []json.RawMessage
	if err := json.Unmarshal(raw, &artifacts); err != nil || artifacts == nil {
		return 0, false
	}
	return len(artifacts), true
}

func boundedImageError(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 {
		return ""
	}
	if len(value) > max {
		cut := max
		for cut > 0 && !utf8.ValidString(value[:cut]) {
			cut--
		}
		value = value[:cut]
	}
	return value
}

func (d *DBStore) FinalizeImageTaskDone(ctx context.Context, taskID string, result ImageTaskDoneResult) (*ImageGenerationBatchRecord, bool, error) {
	return d.finalizeImageTask(ctx, taskID, imageFinalizeDone, result, "", "")
}

func (d *DBStore) FinalizeImageTaskFailed(ctx context.Context, taskID, errorCode, errorMessage string) (*ImageGenerationBatchRecord, bool, error) {
	return d.finalizeImageTask(ctx, taskID, imageFinalizeFailed, ImageTaskDoneResult{}, errorCode, errorMessage)
}

func (d *DBStore) FinalizeImageTaskCanceled(ctx context.Context, taskID string) (*ImageGenerationBatchRecord, bool, error) {
	return d.finalizeImageTask(ctx, taskID, imageFinalizeCanceled, ImageTaskDoneResult{}, "", "")
}

func (d *DBStore) RequestImageBatchCancel(ctx context.Context, userID, agentID, batchID string) (*ImageGenerationBatchRecord, []ImageGenerationTaskRecord, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, nil, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	batch, err := scanImageBatch(tx.QueryRowContext(ctx, `SELECT `+imageBatchColumns+` FROM image_generation_batches WHERE id=? AND user_id=? AND agent_id=? FOR UPDATE`, batchID, userID, agentID))
	if err != nil {
		return nil, nil, err
	}
	if !imageBatchTerminal(batch.Status) {
		if _, err := tx.ExecContext(ctx, `UPDATE image_generation_batches SET cancel_requested=TRUE,updated_at=UTC_TIMESTAMP(6) WHERE id=?`, batchID); err != nil {
			return nil, nil, err
		}
		batch.CancelRequested = true
		if _, err := tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='CANCELED',dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1,dispatched_at=NULL,lease_owner=NULL,lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,finished_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE batch_id=? AND status='PENDING'`, batchID); err != nil {
			return nil, nil, err
		}
		batch, err = d.recomputeImageBatchOn(ctx, tx, batch)
		if err != nil {
			return nil, nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks WHERE batch_id=? ORDER BY item_index,chunk_index,id`, batchID)
	if err != nil {
		return nil, nil, err
	}
	tasks, err := scanImageTaskRows(rows)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return batch, tasks, nil
}
