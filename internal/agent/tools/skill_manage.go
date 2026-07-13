package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

// SkillLedgerUpserter / SkillLedgerDeleter 是 skill_manage 写路径同步的
// 生命周期账本子集。拆成单方法接口,让只做 create/update 的提取循环不必
// 伪造 Delete。store.Store 同时满足两者。
type SkillLedgerUpserter interface {
	UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error
}

type SkillLedgerDeleter interface {
	DeleteSkillUsage(ctx context.Context, agentID, slug string) error
}

// SkillUsageLister is the agent-global lifecycle view used for cross-Pod
// create quotas, exact-content deduplication, and update CAS checks. Keeping
// it separate from SkillManageLedger preserves lightweight/local-only test and
// deployment implementations while allowing store.DBStore to provide the
// authoritative view when configured.
type SkillUsageLister interface {
	ListSkillUsage(ctx context.Context, agentID string) ([]store.SkillUsageRow, error)
}

// SkillMutationReceiptStore is the durable outbox used only by background
// cadence jobs. Foreground updates intentionally have no job receipt.
type SkillMutationReceiptStore interface {
	PrepareSkillExtractionMutation(ctx context.Context, jobID, workerID string, intent store.SkillExtractionMutationIntent) (*store.SkillExtractionMutationReceipt, error)
	GetSkillExtractionMutation(ctx context.Context, jobID string) (*store.SkillExtractionMutationReceipt, error)
	CommitSkillExtractionMutation(ctx context.Context, jobID, workerID string) (*store.SkillExtractionMutationReceipt, error)
	ConflictSkillExtractionMutation(ctx context.Context, jobID, workerID, reason string) (*store.SkillExtractionMutationReceipt, error)
}

var (
	// ErrSkillMutationPending means a durable prepared intent exists but its
	// physical asset or relational receipt still needs reconciliation. The
	// learner loop must stop immediately and retry the job without asking the
	// model to choose another intent.
	ErrSkillMutationPending = errors.New("learner skill mutation is pending reconciliation")
	// ErrSkillMutationConflict is terminal for the current cadence job: a
	// prepared intent encountered a divergent authoritative asset and was
	// deliberately not allowed to overwrite it.
	ErrSkillMutationConflict = errors.New("learner skill mutation reconciliation conflict")
)

// SkillManageLedger 是 Registry 装配用的账本全集(写 + 删)。
type SkillManageLedger interface {
	SkillLedgerUpserter
	SkillLedgerDeleter
}

type SkillMutationLeaser = skills.MutationLeaser

type skillAgentGuard interface {
	GetAgent(ctx context.Context, agentID string) (*store.AgentRecord, error)
}

type skillAgentDeletionGuard interface {
	IsAgentDeleting(ctx context.Context, agentID string) (bool, error)
}

// SkillManageDeps 打包 skill_manage 动作执行的依赖。
type SkillManageDeps struct {
	Manager  *skills.Manager
	Upserter SkillLedgerUpserter // 可为 nil(无 store 装配):跳过记账
	Deleter  SkillLedgerDeleter  // 可为 nil:delete 只删目录
	AgentID  string
	// AssetMax caps the number of learner-generated skills owned by this
	// agent. Non-positive values preserve the unbounded legacy behaviour.
	AssetMax int
	// CreateDisabledReason fails closed when a remote learner namespace was
	// configured but could not be hydrated. Update remains available through
	// its expected-hash CAS; create cannot safely enforce global uniqueness or
	// capacity from an unhealthy local snapshot.
	CreateDisabledReason string
	// CreateHealthCheck refreshes/verifies the remote learner view while the
	// agent-wide lease is held, before taking the per-slug local lock. A
	// successful check permits recovery from an earlier hydration failure.
	CreateHealthCheck func(ctx context.Context) error
	// Workspace mirrors learner CRUD into <agent>/learner-skills. A nil store
	// keeps the single-node/local-only behaviour.
	Workspace workspace.Store
	// MutationBudget limits successful writes for one extraction job. Nil means
	// no per-executor budget (foreground owner turns rely on action capability).
	MutationBudget *SkillMutationBudget
	// BeforeDelete revalidates an internal lifecycle deletion after the
	// agent-wide mutation lease and local lock are held. Foreground/cadence
	// capabilities never expose delete and leave it nil.
	BeforeDelete func(ctx context.Context, slug string) error
	// AllowMissingLocalDelete is reserved for lifecycle reconciliation. It lets
	// the internal delete path reap a remote/ledger orphan while still taking
	// the same agent lease, local operation lock, tombstone guard, and delete
	// revalidation as an ordinary asset deletion.
	AllowMissingLocalDelete bool
	// JobID and WorkerID activate durable cadence idempotency. Both are empty
	// for foreground and lifecycle calls. ReceiptStore must be configured when
	// either value is present.
	JobID        string
	WorkerID     string
	ReceiptStore SkillMutationReceiptStore
}

type SkillManageActions uint8

const (
	SkillManageList SkillManageActions = 1 << iota
	SkillManageRead
	SkillManageCreate
	SkillManageUpdate
	SkillManageDelete
)

const (
	SkillManageForeground = SkillManageList | SkillManageRead | SkillManageUpdate
	SkillManageCadence    = SkillManageList | SkillManageRead | SkillManageCreate | SkillManageUpdate
	SkillManageLifecycle  = SkillManageDelete
	SkillManageAll        = SkillManageForeground | SkillManageCreate | SkillManageDelete
)

func (a SkillManageActions) Allows(action string) bool {
	var bit SkillManageActions
	switch action {
	case "list":
		bit = SkillManageList
	case "read":
		bit = SkillManageRead
	case "create":
		bit = SkillManageCreate
	case "update":
		bit = SkillManageUpdate
	case "delete":
		bit = SkillManageDelete
	default:
		return false
	}
	return a&bit != 0
}

func (a SkillManageActions) names() []string {
	ordered := []string{"list", "read", "create", "update", "delete"}
	out := make([]string, 0, len(ordered))
	for _, action := range ordered {
		if a.Allows(action) {
			out = append(out, action)
		}
	}
	return out
}

type SkillMutationBudget struct {
	mu       sync.Mutex
	limit    int
	reserved int
	used     int
}

func NewSkillMutationBudget(limit int) *SkillMutationBudget {
	return &SkillMutationBudget{limit: limit}
}

func (b *SkillMutationBudget) reserve() (func(bool), bool) {
	if b == nil {
		return func(bool) {}, true
	}
	b.mu.Lock()
	if b.limit <= 0 || b.used+b.reserved >= b.limit {
		b.mu.Unlock()
		return nil, false
	}
	b.reserved++
	b.mu.Unlock()
	return func(success bool) {
		b.mu.Lock()
		b.reserved--
		if success {
			b.used++
		}
		b.mu.Unlock()
	}, true
}

type skillManageArgs struct {
	Action       string `json:"action"`
	Slug         string `json:"slug,omitempty"`
	Content      string `json:"content,omitempty"`
	ExpectedHash string `json:"expected_hash,omitempty"`
}

type skillManageReadResult struct {
	Slug        string `json:"slug"`
	ContentHash string `json:"content_hash"`
	Content     string `json:"content"`
}

// SkillManageMutationResult is the structured create/update response. Learner
// accounting must use Applied rather than inferring success from the requested
// action: a valid update can be a deliberate no-op.
type SkillManageMutationResult struct {
	Action  string `json:"action"`
	Slug    string `json:"slug"`
	Applied bool   `json:"applied"`
	NoOp    bool   `json:"no_op,omitempty"`
	Message string `json:"message,omitempty"`
}

func ParseSkillManageMutationResult(output string) (SkillManageMutationResult, bool) {
	var result SkillManageMutationResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return result, false
	}
	if result.Action != "create" && result.Action != "update" {
		return result, false
	}
	return result, true
}

func encodeSkillManageMutationResult(result SkillManageMutationResult) (string, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode skill mutation result: %w", err)
	}
	return string(data), nil
}

func skillManageDescription(actions SkillManageActions) string {
	if actions == SkillManageForeground {
		return "Inspect and update only this agent's learner-generated skills. Use list/read to identify an existing learned workflow and update only when there is concrete evidence it is wrong, incomplete, or inefficient. New skill creation and deletion are unavailable here."
	}
	if actions == SkillManageCadence {
		return "Create or improve reusable workflows in this agent's isolated learner-generated skill library. Prefer read then update when a related skill already exists; create only for a distinct valuable workflow. Deletion is unavailable."
	}
	return "Manage only this agent's isolated learner-generated skill library using the explicitly allowed actions. Installed, manual, and personal skills are outside this tool's scope."
}

func skillManageSchema(actions SkillManageActions) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        actions.names(),
				"description": "Choose one action exposed by this context. Hidden actions are also rejected by the executor.",
			},
			"slug": map[string]any{
				"type":        "string",
				"description": "Kebab-case skill identifier (e.g. \"deploy-go-service\"). Required for create/update/read/delete.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Full SKILL.md content: YAML frontmatter with non-empty name and description, then step-by-step markdown instructions. Required for create/update.",
			},
			"expected_hash": map[string]any{
				"type":        "string",
				"description": "Required for update. Copy content_hash exactly from the latest successful read of this slug. If the skill changed meanwhile, update is rejected so you can read again and merge.",
				"pattern":     "^[0-9a-fA-F]{64}$",
			},
		},
		"required": []string{"action"},
		"allOf": []any{
			map[string]any{
				"if": map[string]any{
					"properties": map[string]any{
						"action": map[string]any{"const": "update"},
					},
				},
				"then": map[string]any{"required": []string{"slug", "content", "expected_hash"}},
			},
		},
	}
}

// SkillManageToolDef returns the same provider tool schema to the background
// extraction loop. The model still acts through a structured skill_manage
// ToolCall; only the chat-specific Registry authorization wrapper is absent.
func SkillManageToolDef(actions SkillManageActions) provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "skill_manage",
			Description: skillManageDescription(actions),
			Parameters:  skillManageSchema(actions),
		},
	}
}

// SkillManageExec returns the structured skill_manage tool executor used by
// the background learner to dispatch model ToolCalls. This is still the tool
// path (schema -> ToolCall -> executor), not a direct Manager call. Background
// owner authorization happens before the learner loop starts; the main-chat
// path additionally enforces its per-turn gate in registerSkillManage.
func SkillManageExec(deps SkillManageDeps, actions SkillManageActions) ToolFunc {
	return makeSkillManage(func() SkillManageDeps { return deps }, func() SkillManageActions { return actions }, nil)
}

// makeSkillManage 构造 skill_manage 的 ToolFunc。deps 以函数注入按调用时
// 求值——builtin 注册发生在 SetSkillManage 之前,晚装配的管理器/账本因此
// 无需重新注册即可生效。gate 非 nil 时所有动作先过门控。
func makeSkillManage(deps func() SkillManageDeps, actions func() SkillManageActions, gate func(action string) error) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args skillManageArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		action := strings.ToLower(strings.TrimSpace(args.Action))
		if gate != nil {
			if err := gate(action); err != nil {
				return "", err
			}
		}
		allowed := actions()
		if !allowed.Allows(action) {
			return "", fmt.Errorf("skill_manage action %q is not available in this context; allowed actions: %s", action, strings.Join(allowed.names(), ", "))
		}
		resolved := deps()
		var finishBudget func(bool)
		if action == "create" || action == "update" || action == "delete" {
			var ok bool
			finishBudget, ok = resolved.MutationBudget.reserve()
			if !ok {
				return "", fmt.Errorf("skill_manage mutation budget exhausted for this extraction; only one successful write is allowed")
			}
		}
		out, err := applySkillManage(ctx, resolved, action, strings.TrimSpace(args.Slug), args.Content, strings.TrimSpace(args.ExpectedHash))
		if finishBudget != nil {
			consume := err == nil
			if mutation, ok := ParseSkillManageMutationResult(out); ok && !mutation.Applied {
				consume = false
			}
			finishBudget(consume || errors.Is(err, ErrSkillMutationPending) || errors.Is(err, ErrSkillMutationConflict))
		}
		return out, err
	}
}

func applySkillManage(ctx context.Context, deps SkillManageDeps, action, slug, content, expectedHash string) (string, error) {
	if deps.Manager == nil {
		return "", fmt.Errorf("skill management is not configured on this agent")
	}
	if !skills.IsLearnerSkillsRoot(deps.Manager.RootDir()) {
		return "", fmt.Errorf("skill management requires the dedicated learner-skills manager")
	}
	if action == "create" || action == "update" {
		if slug == "" || strings.TrimSpace(content) == "" {
			return "", fmt.Errorf("%s requires slug and content", action)
		}
		var err error
		content, err = deps.Manager.ValidateWrite(slug, content)
		if err != nil {
			return "", err
		}
	}
	var unlock func()
	if action == "create" || action == "update" || action == "delete" {
		if slug == "" {
			return "", fmt.Errorf("%s requires slug", action)
		}
		leaseCtx, releaseLease, err := acquireSkillMutationLease(ctx, deps, slug)
		if err != nil {
			return "", err
		}
		defer releaseLease()
		ctx = leaseCtx
		if err := ensureSkillAgentActive(ctx, deps); err != nil {
			return "", err
		}
		if action == "create" {
			if err := ensureLearnerCreateViewHealthy(ctx, deps); err != nil {
				return "", err
			}
		}
		unlock, err = skills.LockLearnerSkillOperation(deps.Manager.RootDir(), slug)
		if err != nil {
			return "", err
		}
		defer unlock()
		if err := ensureSkillAgentActive(ctx, deps); err != nil {
			return "", err
		}
		if action == "create" {
			if err := validateLearnerSkillCreate(ctx, deps, content); err != nil {
				return "", err
			}
		}
	}
	switch action {
	case "create":
		if _, exists := deps.Manager.Read(slug); exists {
			return "", fmt.Errorf("skill %q already exists", slug)
		}
		receipt, err := prepareSkillMutationReceipt(ctx, deps, store.SkillExtractionMutationIntent{
			Action: "create", Slug: slug, AfterHash: store.HashSkillContent(content), DesiredContent: content,
		})
		if err != nil {
			return "", err
		}
		if receipt != nil {
			return applyPreparedCreate(ctx, deps, receipt, content)
		}
		if err := ensureSkillMutationLeaseActive(ctx); err != nil {
			return "", err
		}
		if err := deps.Manager.Create(slug, content); err != nil {
			return "", err
		}
		if err := ensureSkillMutationLeaseActive(ctx); err != nil {
			localRollback := deps.Manager.Delete(slug)
			return "", fmt.Errorf("create skill %q after mutation lease loss: %w", slug, errors.Join(err, localRollback))
		}
		if err := syncLearnerSkill(ctx, deps, slug, content); err != nil {
			localRollback := deps.Manager.Delete(slug)
			remoteRollback := deleteLearnerSkill(ctx, deps, slug)
			return "", fmt.Errorf("persist created skill %q: %w", slug, errors.Join(err, localRollback, remoteRollback))
		}
		if err := ensureSkillMutationLeaseActive(ctx); err != nil {
			localRollback := deps.Manager.Delete(slug)
			return "", fmt.Errorf("record created skill %q after mutation lease loss: %w", slug, errors.Join(err, localRollback))
		}
		if err := upsertSkillLedger(ctx, deps, slug, content, true); err != nil {
			localRollback := deps.Manager.Delete(slug)
			remoteRollback := deleteLearnerSkill(ctx, deps, slug)
			return "", fmt.Errorf("record created skill %q: %w", slug, errors.Join(err, localRollback, remoteRollback))
		}
		return encodeSkillManageMutationResult(SkillManageMutationResult{
			Action: "create", Slug: slug, Applied: true, Message: fmt.Sprintf("created skill %q", slug),
		})
	case "update":
		if expectedHash == "" {
			return "", fmt.Errorf("update requires expected_hash from the latest skill_manage read; read %q, merge against that content, then retry", slug)
		}
		previous, ok := deps.Manager.Read(slug)
		if !ok {
			return "", fmt.Errorf("skill %q does not exist", slug)
		}
		if err := validateSkillUpdateCAS(ctx, deps, slug, previous, expectedHash); err != nil {
			return "", err
		}
		beforeHash := store.HashSkillContent(previous)
		afterHash := store.HashSkillContent(content)
		if beforeHash == afterHash {
			return encodeSkillManageMutationResult(SkillManageMutationResult{
				Action: "update", Slug: slug, NoOp: true,
				Message: fmt.Sprintf("skill %q already has identical normalized content; no update applied", slug),
			})
		}
		receipt, err := prepareSkillMutationReceipt(ctx, deps, store.SkillExtractionMutationIntent{
			Action: "update", Slug: slug, BeforeHash: beforeHash, AfterHash: afterHash, DesiredContent: content,
		})
		if err != nil {
			return "", err
		}
		if receipt != nil {
			return applyPreparedUpdate(ctx, deps, receipt, previous, content)
		}
		if err := ensureSkillMutationLeaseActive(ctx); err != nil {
			return "", err
		}
		if err := deps.Manager.Update(slug, content); err != nil {
			return "", err
		}
		if err := ensureSkillMutationLeaseActive(ctx); err != nil {
			localRollback := deps.Manager.Update(slug, previous)
			return "", fmt.Errorf("update skill %q after mutation lease loss: %w", slug, errors.Join(err, localRollback))
		}
		if err := syncLearnerSkill(ctx, deps, slug, content); err != nil {
			localRollback := deps.Manager.Update(slug, previous)
			remoteRollback := syncLearnerSkill(ctx, deps, slug, previous)
			return "", fmt.Errorf("persist updated skill %q: %w", slug, errors.Join(err, localRollback, remoteRollback))
		}
		if err := ensureSkillMutationLeaseActive(ctx); err != nil {
			localRollback := deps.Manager.Update(slug, previous)
			return "", fmt.Errorf("record updated skill %q after mutation lease loss: %w", slug, errors.Join(err, localRollback))
		}
		if err := upsertSkillLedger(ctx, deps, slug, content, false); err != nil {
			localRollback := deps.Manager.Update(slug, previous)
			remoteRollback := syncLearnerSkill(ctx, deps, slug, previous)
			return "", fmt.Errorf("record updated skill %q: %w", slug, errors.Join(err, localRollback, remoteRollback))
		}
		return encodeSkillManageMutationResult(SkillManageMutationResult{
			Action: "update", Slug: slug, Applied: true, Message: fmt.Sprintf("updated skill %q", slug),
		})
	case "read":
		if slug == "" {
			return "", fmt.Errorf("read requires slug")
		}
		if deps.Workspace != nil {
			leaseCtx, releaseLease, err := acquireSkillMutationLease(ctx, deps, slug)
			if err != nil {
				return "", err
			}
			defer releaseLease()
			ctx = leaseCtx
			if err := ensureSkillAgentActive(ctx, deps); err != nil {
				return "", err
			}
			if deps.AgentID == "" {
				return "", fmt.Errorf("agent id is required to refresh the learner skill view")
			}
			if err := skills.HydrateLearnerSkillsDown(ctx, deps.Workspace, deps.AgentID, deps.Manager.RootDir()); err != nil {
				return "", fmt.Errorf("refresh learner skill %q before read: %w", slug, err)
			}
			if err := ensureSkillMutationLeaseActive(ctx); err != nil {
				return "", err
			}
		}
		got, ok := deps.Manager.Read(slug)
		if !ok {
			return "", fmt.Errorf("skill %q not found", slug)
		}
		encoded, err := json.Marshal(skillManageReadResult{
			Slug:        slug,
			ContentHash: store.HashSkillContent(got),
			Content:     got,
		})
		if err != nil {
			return "", fmt.Errorf("encode skill %q read result: %w", slug, err)
		}
		return string(encoded), nil
	case "delete":
		if slug == "" {
			return "", fmt.Errorf("delete requires slug")
		}
		previous, ok := deps.Manager.Read(slug)
		if !ok && !deps.AllowMissingLocalDelete {
			return "", fmt.Errorf("skill %q does not exist", slug)
		}
		if deps.BeforeDelete != nil {
			if err := deps.BeforeDelete(ctx, slug); err != nil {
				return "", fmt.Errorf("delete skill %q revalidation failed: %w", slug, err)
			}
		}
		if err := ensureSkillMutationLeaseActive(ctx); err != nil {
			return "", err
		}
		if err := deleteLearnerSkill(ctx, deps, slug); err != nil {
			return "", fmt.Errorf("delete remote skill %q: %w", slug, err)
		}
		if ok {
			if err := ensureSkillMutationLeaseActive(ctx); err != nil {
				return "", err
			}
			if err := deps.Manager.Delete(slug); err != nil {
				// The local copy still exists, so best-effort restore the remote copy
				// before surfacing the error. Hydration must not turn a local failure
				// into an implicit successful delete on the next turn.
				remoteRollback := syncLearnerSkill(ctx, deps, slug, previous)
				return "", fmt.Errorf("delete local skill %q: %w", slug, errors.Join(err, remoteRollback))
			}
		}
		if deps.Deleter != nil && deps.AgentID != "" {
			if err := ensureSkillMutationLeaseActive(ctx); err != nil {
				return "", err
			}
			if err := deps.Deleter.DeleteSkillUsage(ctx, deps.AgentID, slug); err != nil {
				var localRollback, remoteRollback error
				if ok {
					localRollback = deps.Manager.Create(slug, previous)
					remoteRollback = syncLearnerSkill(ctx, deps, slug, previous)
				}
				return "", fmt.Errorf("delete skill ledger %q: %w", slug, errors.Join(err, localRollback, remoteRollback))
			}
		}
		return fmt.Sprintf("deleted skill %q", slug), nil
	case "list":
		items := deps.Manager.List()
		if len(items) == 0 {
			return "(no skills)", nil
		}
		var sb strings.Builder
		for _, it := range items {
			fmt.Fprintf(&sb, "- %s — %s\n", it.Slug, it.Description)
		}
		return sb.String(), nil
	default:
		return "", fmt.Errorf("unknown action %q: use create, update, read, delete, or list", action)
	}
}

func skillMutationReceiptStore(deps SkillManageDeps) SkillMutationReceiptStore {
	if deps.ReceiptStore != nil {
		return deps.ReceiptStore
	}
	if candidate, ok := deps.Upserter.(SkillMutationReceiptStore); ok {
		return candidate
	}
	if candidate, ok := deps.Deleter.(SkillMutationReceiptStore); ok {
		return candidate
	}
	return nil
}

func pendingSkillMutationError(operation string, err error) error {
	if err == nil {
		err = errors.New("unknown mutation failure")
	}
	return fmt.Errorf("%w: %s: %v", ErrSkillMutationPending, operation, err)
}

func prepareSkillMutationReceipt(ctx context.Context, deps SkillManageDeps, intent store.SkillExtractionMutationIntent) (*store.SkillExtractionMutationReceipt, error) {
	jobID := strings.TrimSpace(deps.JobID)
	workerID := strings.TrimSpace(deps.WorkerID)
	if jobID == "" && workerID == "" {
		return nil, nil
	}
	if jobID == "" || workerID == "" {
		return nil, pendingSkillMutationError("prepare durable intent", errors.New("cadence job_id and worker_id must both be configured"))
	}
	receipts := skillMutationReceiptStore(deps)
	if receipts == nil {
		return nil, pendingSkillMutationError("prepare durable intent", errors.New("mutation receipt store is not configured"))
	}
	receipt, err := receipts.PrepareSkillExtractionMutation(ctx, jobID, workerID, intent)
	if err != nil {
		return nil, pendingSkillMutationError("prepare durable intent", err)
	}
	if receipt.Status == store.SkillExtractionMutationConflict {
		return nil, fmt.Errorf("%w: %s", ErrSkillMutationConflict, receipt.LastError)
	}
	return receipt, nil
}

func committedMutationResult(receipt *store.SkillExtractionMutationReceipt) (string, error) {
	if receipt == nil {
		return "", errors.New("nil skill mutation receipt")
	}
	return encodeSkillManageMutationResult(SkillManageMutationResult{
		Action: receipt.Action, Slug: receipt.Slug, Applied: true,
		Message: fmt.Sprintf("%s skill %q", map[string]string{"create": "created", "update": "updated"}[receipt.Action], receipt.Slug),
	})
}

func commitPreparedMutation(ctx context.Context, deps SkillManageDeps, receipt *store.SkillExtractionMutationReceipt) (string, error) {
	if receipt.Status == store.SkillExtractionMutationApplied {
		return committedMutationResult(receipt)
	}
	if receipt.Status != store.SkillExtractionMutationPrepared {
		return "", fmt.Errorf("%w: receipt %q has status %q", ErrSkillMutationConflict, receipt.JobID, receipt.Status)
	}
	if err := ensureSkillMutationLeaseActive(ctx); err != nil {
		return "", pendingSkillMutationError("commit durable mutation after lease loss", err)
	}
	receipts := skillMutationReceiptStore(deps)
	applied, err := receipts.CommitSkillExtractionMutation(ctx, deps.JobID, deps.WorkerID)
	if err != nil {
		// A database Commit error is ambiguous: the server may have committed even
		// though the client lost its acknowledgement. Never roll the asset back;
		// recovery resolves the receipt with a fresh read.
		return "", pendingSkillMutationError("commit mutation receipt and lifecycle ledger", err)
	}
	return committedMutationResult(applied)
}

func applyPreparedCreate(ctx context.Context, deps SkillManageDeps, receipt *store.SkillExtractionMutationReceipt, content string) (string, error) {
	if receipt.Status == store.SkillExtractionMutationApplied {
		return committedMutationResult(receipt)
	}
	if err := ensureSkillMutationLeaseActive(ctx); err != nil {
		return "", pendingSkillMutationError("create after lease loss", err)
	}
	if err := deps.Manager.Create(receipt.Slug, content); err != nil {
		return "", pendingSkillMutationError(fmt.Sprintf("create local skill %q", receipt.Slug), err)
	}
	if err := ensureSkillMutationLeaseActive(ctx); err != nil {
		return "", pendingSkillMutationError(fmt.Sprintf("persist created skill %q after lease loss", receipt.Slug), err)
	}
	if err := syncLearnerSkill(ctx, deps, receipt.Slug, content); err != nil {
		return "", pendingSkillMutationError(fmt.Sprintf("persist created skill %q", receipt.Slug), err)
	}
	return commitPreparedMutation(ctx, deps, receipt)
}

func applyPreparedUpdate(ctx context.Context, deps SkillManageDeps, receipt *store.SkillExtractionMutationReceipt, previous, content string) (string, error) {
	if receipt.Status == store.SkillExtractionMutationApplied {
		return committedMutationResult(receipt)
	}
	if store.HashSkillContent(previous) != receipt.BeforeHash {
		return "", pendingSkillMutationError(fmt.Sprintf("update skill %q from prepared base", receipt.Slug), errors.New("local base hash changed before physical write"))
	}
	if err := ensureSkillMutationLeaseActive(ctx); err != nil {
		return "", pendingSkillMutationError("update after lease loss", err)
	}
	if err := deps.Manager.Update(receipt.Slug, content); err != nil {
		return "", pendingSkillMutationError(fmt.Sprintf("update local skill %q", receipt.Slug), err)
	}
	if err := ensureSkillMutationLeaseActive(ctx); err != nil {
		return "", pendingSkillMutationError(fmt.Sprintf("persist updated skill %q after lease loss", receipt.Slug), err)
	}
	if err := syncLearnerSkill(ctx, deps, receipt.Slug, content); err != nil {
		return "", pendingSkillMutationError(fmt.Sprintf("persist updated skill %q", receipt.Slug), err)
	}
	return commitPreparedMutation(ctx, deps, receipt)
}

func mutationResultFromReceipt(receipt *store.SkillExtractionMutationReceipt) SkillManageMutationResult {
	if receipt == nil {
		return SkillManageMutationResult{}
	}
	return SkillManageMutationResult{Action: receipt.Action, Slug: receipt.Slug, Applied: receipt.Status == store.SkillExtractionMutationApplied}
}

// ReconcileSkillExtractionMutation resumes a prepared outbox entry without an
// LLM call. Object storage, when configured, is hydrated first and is therefore
// authoritative. The only physical replay is absent->after for create or
// before->after for update; every divergent state is terminalized as conflict.
func ReconcileSkillExtractionMutation(ctx context.Context, deps SkillManageDeps, receipt *store.SkillExtractionMutationReceipt) (SkillManageMutationResult, error) {
	if receipt == nil {
		return SkillManageMutationResult{}, errors.New("nil skill mutation receipt")
	}
	if receipt.Status == store.SkillExtractionMutationApplied {
		return mutationResultFromReceipt(receipt), nil
	}
	if receipt.Status == store.SkillExtractionMutationConflict {
		return mutationResultFromReceipt(receipt), fmt.Errorf("%w: %s", ErrSkillMutationConflict, receipt.LastError)
	}
	if receipt.Status != store.SkillExtractionMutationPrepared {
		return SkillManageMutationResult{}, fmt.Errorf("unknown skill mutation receipt status %q", receipt.Status)
	}
	if deps.Manager == nil || !skills.IsLearnerSkillsRoot(deps.Manager.RootDir()) {
		return SkillManageMutationResult{}, pendingSkillMutationError("reconcile mutation", errors.New("dedicated learner manager is not configured"))
	}
	if deps.JobID != receipt.JobID || deps.AgentID != receipt.AgentID || deps.WorkerID == "" {
		return SkillManageMutationResult{}, pendingSkillMutationError("reconcile mutation", errors.New("job, worker, or agent scope does not match receipt"))
	}
	content, err := deps.Manager.ValidateWrite(receipt.Slug, receipt.DesiredContent)
	if err != nil || store.HashSkillContent(content) != receipt.AfterHash {
		reason := "prepared desired content no longer passes validation"
		if err == nil {
			reason = "prepared desired content hash does not match receipt"
		}
		return markSkillMutationConflict(ctx, deps, receipt, reason)
	}

	leaseCtx, releaseLease, err := acquireSkillMutationLease(ctx, deps, receipt.Slug)
	if err != nil {
		return SkillManageMutationResult{}, pendingSkillMutationError("acquire reconciliation lease", err)
	}
	defer releaseLease()
	ctx = leaseCtx
	if err := ensureSkillAgentActive(ctx, deps); err != nil {
		return SkillManageMutationResult{}, pendingSkillMutationError("verify learner agent before reconciliation", err)
	}
	if deps.Workspace != nil {
		if err := skills.HydrateLearnerSkillsDown(ctx, deps.Workspace, deps.AgentID, deps.Manager.RootDir()); err != nil {
			return SkillManageMutationResult{}, pendingSkillMutationError("hydrate authoritative learner assets for reconciliation", err)
		}
	}
	unlock, err := skills.LockLearnerSkillOperation(deps.Manager.RootDir(), receipt.Slug)
	if err != nil {
		return SkillManageMutationResult{}, pendingSkillMutationError("lock local learner asset for reconciliation", err)
	}
	defer unlock()
	if err := ensureSkillMutationLeaseActive(ctx); err != nil {
		return SkillManageMutationResult{}, pendingSkillMutationError("reconcile after lease loss", err)
	}

	current, exists := deps.Manager.Read(receipt.Slug)
	currentHash := ""
	if exists {
		currentHash = store.HashSkillContent(current)
	}
	if exists && currentHash == receipt.AfterHash {
		// Hydration intentionally preserves legacy local assets while a remote
		// learner namespace has never been initialized. Therefore an after-hash
		// local file is not proof the object exists. An idempotent write closes the
		// first-create crash window before declaring the receipt applied.
		if deps.Workspace != nil {
			if err := syncLearnerSkill(ctx, deps, receipt.Slug, content); err != nil {
				return SkillManageMutationResult{}, pendingSkillMutationError("ensure reconciled learner asset is durable", err)
			}
		}
		out, err := commitPreparedMutation(ctx, deps, receipt)
		if err != nil {
			return SkillManageMutationResult{}, err
		}
		result, _ := ParseSkillManageMutationResult(out)
		return result, nil
	}

	switch receipt.Action {
	case "create":
		if exists {
			return markSkillMutationConflict(ctx, deps, receipt, fmt.Sprintf("create target has divergent content hash %s", currentHash))
		}
		if err := deps.Manager.Create(receipt.Slug, content); err != nil {
			return SkillManageMutationResult{}, pendingSkillMutationError("replay prepared local create", err)
		}
	case "update":
		if !exists {
			return markSkillMutationConflict(ctx, deps, receipt, "update target is missing")
		}
		if currentHash != receipt.BeforeHash {
			return markSkillMutationConflict(ctx, deps, receipt, fmt.Sprintf("update target has divergent content hash %s", currentHash))
		}
		if err := deps.Manager.Update(receipt.Slug, content); err != nil {
			return SkillManageMutationResult{}, pendingSkillMutationError("replay prepared local update", err)
		}
	default:
		return markSkillMutationConflict(ctx, deps, receipt, fmt.Sprintf("unsupported prepared action %q", receipt.Action))
	}
	if err := ensureSkillMutationLeaseActive(ctx); err != nil {
		return SkillManageMutationResult{}, pendingSkillMutationError("persist reconciled mutation after lease loss", err)
	}
	if err := syncLearnerSkill(ctx, deps, receipt.Slug, content); err != nil {
		return SkillManageMutationResult{}, pendingSkillMutationError("persist reconciled learner asset", err)
	}
	out, err := commitPreparedMutation(ctx, deps, receipt)
	if err != nil {
		return SkillManageMutationResult{}, err
	}
	result, _ := ParseSkillManageMutationResult(out)
	return result, nil
}

func markSkillMutationConflict(ctx context.Context, deps SkillManageDeps, receipt *store.SkillExtractionMutationReceipt, reason string) (SkillManageMutationResult, error) {
	receipts := skillMutationReceiptStore(deps)
	if receipts == nil {
		return SkillManageMutationResult{}, pendingSkillMutationError("record mutation conflict", errors.New("mutation receipt store is not configured"))
	}
	conflicted, err := receipts.ConflictSkillExtractionMutation(ctx, deps.JobID, deps.WorkerID, reason)
	if err != nil {
		return SkillManageMutationResult{}, pendingSkillMutationError("record mutation conflict", err)
	}
	return mutationResultFromReceipt(conflicted), fmt.Errorf("%w: %s", ErrSkillMutationConflict, reason)
}

func validateLearnerSkillCreate(ctx context.Context, deps SkillManageDeps, content string) error {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	items := deps.Manager.List()
	normalized := normalizeSkillContentNewlines(content)
	contentHash := store.HashSkillContent(normalized)
	knownSlugs := make(map[string]struct{}, len(items))
	for _, item := range items {
		knownSlugs[item.Slug] = struct{}{}
		existing, ok := deps.Manager.Read(item.Slug)
		if !ok {
			continue
		}
		existingNormalized := normalizeSkillContentNewlines(existing)
		if store.HashSkillContent(existingNormalized) == contentHash && existingNormalized == normalized {
			return fmt.Errorf("create refused: normalized content is identical to existing learner skill %q; it already exists, so read/update/merge that skill instead of creating another slug", item.Slug)
		}
	}

	// Prefer the relational ledger whenever it is available: the agent-wide
	// mutation lease makes this view stable for the remainder of this create,
	// including sibling Pods whose local Manager has not hydrated their latest
	// mutation yet. Local-only/orphan slugs are unioned in so they still count.
	if lister := skillUsageLister(deps); lister != nil && deps.AgentID != "" {
		rows, err := lister.ListSkillUsage(ctx, deps.AgentID)
		if err != nil {
			return fmt.Errorf("create refused because the agent-global learner skill ledger could not be read: %w", err)
		}
		for _, row := range rows {
			if row.Origin != "learner" || row.Slug == "" {
				continue
			}
			knownSlugs[row.Slug] = struct{}{}
			if row.ContentHash != "" && strings.EqualFold(row.ContentHash, contentHash) {
				return fmt.Errorf("create refused: normalized content is identical to existing learner skill %q in the agent-global ledger; read/update/merge that skill instead of creating another slug", row.Slug)
			}
		}
	}
	if deps.AssetMax > 0 && len(knownSlugs) >= deps.AssetMax {
		return fmt.Errorf("learner skill asset limit (%d) reached; create refused: read/update/merge an existing learner skill instead of creating a new one", deps.AssetMax)
	}
	return nil
}

func ensureLearnerCreateViewHealthy(ctx context.Context, deps SkillManageDeps) error {
	if deps.CreateHealthCheck != nil {
		if err := deps.CreateHealthCheck(ctx); err != nil {
			return fmt.Errorf("create refused because the learner asset view is not healthy: %w; retry after storage hydration succeeds", err)
		}
		return nil
	}
	if reason := strings.TrimSpace(deps.CreateDisabledReason); reason != "" {
		return fmt.Errorf("create refused because the learner asset view is not healthy: %s; retry after storage hydration succeeds", reason)
	}
	return nil
}

func skillUsageLister(deps SkillManageDeps) SkillUsageLister {
	if candidate, ok := deps.Upserter.(SkillUsageLister); ok {
		return candidate
	}
	if candidate, ok := deps.Deleter.(SkillUsageLister); ok {
		return candidate
	}
	return nil
}

func validateSkillUpdateCAS(ctx context.Context, deps SkillManageDeps, slug, currentContent, expectedHash string) error {
	expectedHash = strings.ToLower(strings.TrimSpace(expectedHash))
	if len(expectedHash) != 64 {
		return fmt.Errorf("update expected_hash for skill %q must be the 64-character content_hash returned by the latest read", slug)
	}
	for _, ch := range expectedHash {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return fmt.Errorf("update expected_hash for skill %q must be the hexadecimal content_hash returned by the latest read", slug)
		}
	}
	currentHash := store.HashSkillContent(currentContent)
	if currentHash != expectedHash {
		return skillUpdateConflict(slug, expectedHash, currentHash)
	}
	if lister := skillUsageLister(deps); lister != nil && deps.AgentID != "" {
		rows, err := lister.ListSkillUsage(ctx, deps.AgentID)
		if err != nil {
			return fmt.Errorf("verify agent-global version before updating skill %q: %w", slug, err)
		}
		for _, row := range rows {
			if row.Origin != "learner" || row.Slug != slug || row.ContentHash == "" {
				continue
			}
			ledgerHash := strings.ToLower(row.ContentHash)
			if ledgerHash != expectedHash {
				return skillUpdateConflict(slug, expectedHash, ledgerHash)
			}
			break
		}
	}
	return nil
}

func skillUpdateConflict(slug, expectedHash, currentHash string) error {
	return fmt.Errorf("skill %q changed since it was read (expected_hash %s, current content_hash %s); read it again, merge your change with the latest content, and retry update using the new content_hash", slug, expectedHash, currentHash)
}

func normalizeSkillContentNewlines(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return strings.ReplaceAll(content, "\r", "\n")
}

func ensureSkillAgentActive(ctx context.Context, deps SkillManageDeps) error {
	var deletionGuard skillAgentDeletionGuard
	if candidate, ok := deps.Upserter.(skillAgentDeletionGuard); ok {
		deletionGuard = candidate
	} else if candidate, ok := deps.Deleter.(skillAgentDeletionGuard); ok {
		deletionGuard = candidate
	}
	if deletionGuard != nil && deps.AgentID != "" {
		deleting, err := deletionGuard.IsAgentDeleting(ctx, deps.AgentID)
		if err != nil {
			return fmt.Errorf("verify learner skill deletion state: %w", err)
		}
		if deleting {
			return fmt.Errorf("agent %q is being deleted; learner skill mutation refused", deps.AgentID)
		}
	}
	guard, ok := deps.Upserter.(skillAgentGuard)
	if !ok {
		if candidate, candidateOK := deps.Deleter.(skillAgentGuard); candidateOK {
			guard = candidate
			ok = true
		}
	}
	if !ok || deps.AgentID == "" {
		return nil
	}
	if _, err := guard.GetAgent(ctx, deps.AgentID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("agent %q no longer exists; learner skill mutation refused", deps.AgentID)
		}
		return fmt.Errorf("verify learner skill agent: %w", err)
	}
	return nil
}

func acquireSkillMutationLease(ctx context.Context, deps SkillManageDeps, slug string) (context.Context, func(), error) {
	var leaser SkillMutationLeaser
	if candidate, ok := deps.Upserter.(SkillMutationLeaser); ok {
		leaser = candidate
	} else if candidate, ok := deps.Deleter.(SkillMutationLeaser); ok {
		leaser = candidate
	}
	lease, err := skills.AcquireLearnerSkillLeaseGuard(ctx, leaser, deps.AgentID, slug)
	if err != nil {
		return nil, nil, err
	}
	return lease.Context(), func() {
		if err := lease.Release(); err != nil {
			slog.Warn("release learner skill mutation lease failed", "agent", deps.AgentID, "slug", slug, "error", err)
		}
	}, nil
}

func ensureSkillMutationLeaseActive(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return fmt.Errorf("learner skill mutation lease is no longer active: %w", err)
	}
	return nil
}

func syncLearnerSkill(ctx context.Context, deps SkillManageDeps, slug, content string) error {
	if deps.Workspace == nil {
		return nil
	}
	if deps.AgentID == "" {
		return fmt.Errorf("agent id is required for learner skill persistence")
	}
	return skills.SyncLearnerSkillContent(ctx, deps.Workspace, deps.AgentID, slug, content)
}

func deleteLearnerSkill(ctx context.Context, deps SkillManageDeps, slug string) error {
	if deps.Workspace == nil {
		return nil
	}
	if deps.AgentID == "" {
		return fmt.Errorf("agent id is required for learner skill persistence")
	}
	return skills.DeleteLearnerSkillUp(ctx, deps.Workspace, deps.AgentID, slug)
}

// registerSkillManage 把 skill_manage 注册为 builtin。依赖按调用时从
// Registry 字段读取;所有动作过 owner 门控。ForTurn 重新注册 builtins,
// 闭包因此捕获回合副本、读到本回合的 chatterUserID。
func registerSkillManage(r *Registry) {
	r.Register("skill_manage", skillManageDescription(SkillManageAll), skillManageSchema(SkillManageAll), makeSkillManage(
		func() SkillManageDeps {
			return SkillManageDeps{
				Manager:   r.skillManager,
				Upserter:  r.skillLedger,
				Deleter:   r.skillLedger,
				AgentID:   r.agentID,
				Workspace: r.workspaceStore,
			}
		},
		func() SkillManageActions { return r.skillManageActions },
		func(action string) error {
			if r.skillManageActions != 0 {
				return nil
			}
			return fmt.Errorf("skill management is restricted to the agent owner")
		},
	))
}

// upsertSkillLedger is the relational half of foreground/lifecycle mutations.
// Its caller treats failure as a mutation failure and restores the prior
// local/remote state. Cadence jobs instead commit the ledger atomically with
// their durable mutation receipt.
func upsertSkillLedger(ctx context.Context, deps SkillManageDeps, slug, content string, firstCreate bool) error {
	if deps.Upserter == nil || deps.AgentID == "" {
		return nil
	}
	if err := deps.Upserter.UpsertSkillUsage(ctx, deps.AgentID, slug, store.HashSkillContent(content), firstCreate); err != nil {
		return err
	}
	return nil
}
