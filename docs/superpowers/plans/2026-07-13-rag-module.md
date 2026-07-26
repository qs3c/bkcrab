# RAG 板块（后端）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 bkcrab 新增用户级 RAG 知识库板块：用户建库/传文档/自动索引，agent 经配置授权后通过只读 `rag_search` 工具做混合检索。

**Architecture:** 内嵌 Go 流水线（解析→分块→embedding→先算后删再写）+ Milvus 向量库（每 KB 一 collection，dense+BM25 混合检索、原生 RRF 融合）+ 主库三张新表（KB/文档/索引任务）+ S3/MinIO 存原件。`rag.Service` 是唯一门面；`rag_search` 走 builtin 工具注册表，仅在 agent 配置了 KB 且系统 RAG 可用时注册。

**Tech Stack:** Go；`github.com/milvus-io/milvus/client/v2`（Milvus 2.5 Go SDK）；`github.com/ledongthuc/pdf`（PDF 文本提取）；`github.com/minio/minio-go/v7`（已有依赖）；OpenAI 兼容 `/embeddings` API。

**规格:** `docs/superpowers/specs/2026-07-13-rag-module-design.md`（已确认）。

**范围说明:** 本计划只覆盖后端（含 HTTP API 与 rag_search 工具）。Web UI（Next.js 控制台页面）在 API 稳定后另出一份计划。

**沙箱注意:** 服务器沙箱外网需代理（`go get` 拉依赖时注意 GOPROXY）；所有单测不依赖网络；Milvus/MySQL 集成测试用环境变量门控。

---

## 文件结构总览

```
internal/store/rag.go                 # 三张表的 record + CRUD（挂 DBStore）
internal/store/rag_test.go
internal/store/database.go            # migrationSQL 增补（modify）
internal/store/database_mysql.go      # mysqlMigrationSQL 增补（modify）
internal/store/store.go               # Store 接口增补（modify）
internal/config/config.go             # RAGCfg / RAGAgentCfg + 合并（modify）
internal/rag/split/split.go           # 分块器（结构感知 + 滑窗）+ token 估算
internal/rag/split/split_test.go
internal/rag/parse/parse.go           # 入口分发 + md/txt
internal/rag/parse/docx.go            # DOCX 提取（zip+xml，无新依赖）
internal/rag/parse/pdf.go             # PDF 提取（ledongthuc/pdf）
internal/rag/parse/parse_test.go
internal/rag/parse/testdata/          # 固定样例文件
internal/rag/embed/embed.go           # OpenAI 兼容 embeddings 客户端
internal/rag/embed/embed_test.go
internal/rag/vector/vector.go         # VectorStore 窄接口 + 数据类型
internal/rag/vector/fake.go           # 内存 fake（单测用）
internal/rag/vector/fake_test.go
internal/rag/vector/milvus.go         # Milvus 实现
internal/rag/vector/milvus_test.go    # env 门控集成测试
internal/rag/objects/objects.go       # 原件存储接口 + S3/LocalFS 实现
internal/rag/objects/objects_test.go
internal/rag/service.go               # Service 门面：KB CRUD
internal/rag/pipeline.go              # 上传 + worker 池 + doIndex + 重试/恢复
internal/rag/search.go                # 检索 + 跨 KB 归并 + 工具文本渲染
internal/rag/service_test.go
internal/rag/pipeline_test.go
internal/rag/search_test.go
internal/agent/tools/rag_search.go    # rag_search 工具注册
internal/agent/tools/rag_search_test.go
internal/agent/manager.go             # WithRAGService + buildAgent 装配（modify）
internal/gateway/gateway.go           # 引导时构造 rag.Service（modify）
internal/gateway/userspace.go         # managerOpts 追加（modify，锚点 :677）
internal/setup/handlers_rag.go        # HTTP API
internal/setup/handlers_rag_test.go
internal/setup/server.go              # 路由注册（modify，锚点 :195 起路由块）
docs/database.md                      # 新表文档（modify）
```

依赖方向：`tools/rag_search → rag.Service → (store, vector, objects, embed, split, parse)`。`rag` 包不 import `agent`/`setup`。

---

### Task 1: 引入依赖

**Files:**
- Modify: `go.mod` / `go.sum`

- [ ] **Step 1: 拉取依赖**

```bash
cd D:/fromGithub/bkcrab
go get github.com/milvus-io/milvus/client/v2@latest
go get github.com/ledongthuc/pdf@latest
go mod tidy
```

预期：go.mod 出现两个新 require；`go build ./...` 通过。若 Milvus SDK 主版本路径有变（以 pkg.go.dev/github.com/milvus-io/milvus/client/v2 为准），记录实际 module path，本计划后续 import 全部随之替换。

- [ ] **Step 2: Commit**

```bash
git add go.mod go.sum
git commit -m "build(rag): 引入 milvus go sdk 与 pdf 文本提取依赖"
```

---

### Task 2: store 三张表（record + 迁移 + CRUD）

**Files:**
- Create: `internal/store/rag.go`, `internal/store/rag_test.go`
- Modify: `internal/store/database.go`（`migrationSQL()` 返回的 DDL 列表末尾追加）、`internal/store/database_mysql.go`（`mysqlMigrationSQL()` 同步追加）、`internal/store/store.go`（Store 接口）

**注意:** 列类型/风格照抄同文件里 `skill_usage` 表的写法（database.go:1580 附近），MySQL 侧照抄 database_mysql.go 中对应表的 VARCHAR/DATETIME 风格。

- [ ] **Step 1: 写失败测试**（`internal/store/rag_test.go`，沿用本包 SQLite 测试的现有构造模式，参照 `skill_usage_test.go` 开头如何拿到测试 store）

```go
package store

import (
	"context"
	"testing"
	"time"
)

func TestRAGKBCRUD(t *testing.T) {
	st := newTestStore(t) // 与 skill_usage_test.go 相同的辅助函数；若名字不同以现文件为准
	ctx := context.Background()

	kb := &RAGKBRecord{
		ID: "kb_test1", UserID: "u_1", Name: "产品手册",
		EmbedProvider: "system", EmbedModel: "text-embedding-v3", EmbedDims: 1024,
		ChunkSize: 512, ChunkOverlap: 64, Status: "active",
	}
	if err := st.CreateRAGKB(ctx, kb); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetRAGKB(ctx, "kb_test1")
	if err != nil || got.Name != "产品手册" || got.EmbedDims != 1024 {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	list, err := st.ListRAGKBsByUser(ctx, "u_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v err=%v", list, err)
	}
	got.Status = "deleting"
	if err := st.UpdateRAGKB(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := st.DeleteRAGKB(ctx, "kb_test1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetRAGKB(ctx, "kb_test1"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRAGDocumentAndTaskLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	doc := &RAGDocumentRecord{
		ID: "doc_1", KBID: "kb_1", FileName: "a.md", FileType: "md",
		FileSize: 10, ObjectKey: "rag/u/kb/doc/a.md", Status: "PENDING", Version: 1,
	}
	if err := st.CreateRAGDocument(ctx, doc); err != nil {
		t.Fatalf("create doc: %v", err)
	}
	taskID, err := st.CreateRAGIndexTask(ctx, "doc_1", 3)
	if err != nil || taskID == 0 {
		t.Fatalf("create task: id=%d err=%v", taskID, err)
	}
	// 崩溃恢复语义：PENDING 与 RUNNING 都要被捞回
	if err := st.UpdateRAGIndexTask(ctx, taskID, "RUNNING", 0, ""); err != nil {
		t.Fatalf("update task: %v", err)
	}
	pend, err := st.ListRunnableRAGIndexTasks(ctx)
	if err != nil || len(pend) != 1 || pend[0].DocID != "doc_1" {
		t.Fatalf("runnable: %+v err=%v", pend, err)
	}
	if err := st.UpdateRAGIndexTask(ctx, taskID, "FAILED", 1, "boom"); err != nil {
		t.Fatalf("fail task: %v", err)
	}
	pend, _ = st.ListRunnableRAGIndexTasks(ctx)
	if len(pend) != 0 {
		t.Fatalf("FAILED 不应可运行: %+v", pend)
	}
	doc.Status = "DONE"
	doc.ChunkCount = 5
	now := time.Now()
	doc.IndexedAt = &now
	if err := st.UpdateRAGDocument(ctx, doc); err != nil {
		t.Fatalf("update doc: %v", err)
	}
	docs, err := st.ListRAGDocumentsByKB(ctx, "kb_1")
	if err != nil || len(docs) != 1 || docs[0].ChunkCount != 5 {
		t.Fatalf("list docs: %+v err=%v", docs, err)
	}
	if err := st.DeleteRAGDocument(ctx, "doc_1"); err != nil {
		t.Fatalf("delete doc: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/store/ -run TestRAG -v
```

预期：编译错误（RAGKBRecord 未定义）。

- [ ] **Step 3: 实现**（`internal/store/rag.go`）

```go
package store

import (
	"context"
	"database/sql"
	"time"
)

// RAGKBRecord 是 rag_kbs 行。embedding 三元组（provider/model/dims）在建库时
// 快照、之后只读——换模型必须走"重建索引"，保证库内向量维度一致。
type RAGKBRecord struct {
	ID            string
	UserID        string
	Name          string
	Description   string
	EmbedProvider string // "system" 或 "user"——快照时生效的配置来源，仅展示用
	EmbedModel    string
	EmbedDims     int
	ChunkSize     int
	ChunkOverlap  int
	Status        string // active / deleting
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RAGDocumentRecord 是 rag_documents 行。Version 每次重建索引递增，
// 向量库里旧版本 chunk 按 doc_version < Version 识别删除。
type RAGDocumentRecord struct {
	ID         string
	KBID       string
	FileName   string
	FileType   string // md / txt / pdf / docx
	FileSize   int64
	ObjectKey  string
	Status     string // PENDING / PROCESSING / DONE / FAILED
	ErrorMsg   string
	ChunkCount int
	TokenCount int
	Version    int
	UploadedAt time.Time
	IndexedAt  *time.Time
}

// RAGIndexTaskRecord 是 rag_index_tasks 行——异步索引的状态与崩溃恢复凭据。
type RAGIndexTaskRecord struct {
	ID         int64
	DocID      string
	Status     string // PENDING / RUNNING / DONE / FAILED
	RetryCount int
	MaxRetry   int
	ErrorMsg   string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

func (d *DBStore) CreateRAGKB(ctx context.Context, kb *RAGKBRecord) error {
	now := time.Now().UTC()
	kb.CreatedAt, kb.UpdatedAt = now, now
	_, err := d.db.ExecContext(ctx, d.rebind(`INSERT INTO rag_kbs
		(id, user_id, name, description, embed_provider, embed_model, embed_dims,
		 chunk_size, chunk_overlap, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`),
		kb.ID, kb.UserID, kb.Name, kb.Description, kb.EmbedProvider, kb.EmbedModel,
		kb.EmbedDims, kb.ChunkSize, kb.ChunkOverlap, kb.Status, kb.CreatedAt, kb.UpdatedAt)
	return err
}

func (d *DBStore) GetRAGKB(ctx context.Context, id string) (*RAGKBRecord, error) {
	row := d.db.QueryRowContext(ctx, d.rebind(`SELECT id, user_id, name, description,
		embed_provider, embed_model, embed_dims, chunk_size, chunk_overlap, status,
		created_at, updated_at FROM rag_kbs WHERE id = ?`), id)
	var kb RAGKBRecord
	err := row.Scan(&kb.ID, &kb.UserID, &kb.Name, &kb.Description, &kb.EmbedProvider,
		&kb.EmbedModel, &kb.EmbedDims, &kb.ChunkSize, &kb.ChunkOverlap, &kb.Status,
		&kb.CreatedAt, &kb.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return &kb, err
}

func (d *DBStore) ListRAGKBsByUser(ctx context.Context, userID string) ([]RAGKBRecord, error) {
	rows, err := d.db.QueryContext(ctx, d.rebind(`SELECT id, user_id, name, description,
		embed_provider, embed_model, embed_dims, chunk_size, chunk_overlap, status,
		created_at, updated_at FROM rag_kbs WHERE user_id = ? ORDER BY created_at`), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RAGKBRecord
	for rows.Next() {
		var kb RAGKBRecord
		if err := rows.Scan(&kb.ID, &kb.UserID, &kb.Name, &kb.Description, &kb.EmbedProvider,
			&kb.EmbedModel, &kb.EmbedDims, &kb.ChunkSize, &kb.ChunkOverlap, &kb.Status,
			&kb.CreatedAt, &kb.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, kb)
	}
	return out, rows.Err()
}

func (d *DBStore) UpdateRAGKB(ctx context.Context, kb *RAGKBRecord) error {
	kb.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx, d.rebind(`UPDATE rag_kbs SET name=?, description=?,
		chunk_size=?, chunk_overlap=?, status=?, updated_at=? WHERE id=?`),
		kb.Name, kb.Description, kb.ChunkSize, kb.ChunkOverlap, kb.Status, kb.UpdatedAt, kb.ID)
	return err
}

func (d *DBStore) DeleteRAGKB(ctx context.Context, id string) error {
	// 级联：先删子行再删 KB。文档对应的向量/S3 清理由 rag.Service 负责。
	if _, err := d.db.ExecContext(ctx, d.rebind(
		`DELETE FROM rag_index_tasks WHERE doc_id IN (SELECT id FROM rag_documents WHERE kb_id = ?)`), id); err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx, d.rebind(`DELETE FROM rag_documents WHERE kb_id = ?`), id); err != nil {
		return err
	}
	_, err := d.db.ExecContext(ctx, d.rebind(`DELETE FROM rag_kbs WHERE id = ?`), id)
	return err
}

// —— documents / tasks 的 CRUD 同风格实现 ——
// CreateRAGDocument / GetRAGDocument / ListRAGDocumentsByKB / UpdateRAGDocument /
// DeleteRAGDocument（同时删其 tasks）/ CreateRAGIndexTask（返回自增 id）/
// UpdateRAGIndexTask(id, status, retryCount, errMsg)（RUNNING 时写 started_at，
// DONE/FAILED 时写 finished_at）/ ListRunnableRAGIndexTasks（status IN
// ('PENDING','RUNNING') 按 created_at 升序——RUNNING 也捞是崩溃恢复语义）。
// 逐个方法照上面的 ExecContext/QueryRowContext 模式写全，此处不省略字段。
```

> 注：`d.rebind` 若本包不存在（现有代码可能直接写 `?` 占位符并依赖方言层），以 `skill_usage.go` 的实际占位符处理方式为准，保持完全一致。

**迁移 DDL**（追加到 `database.go` 的 `migrationSQL()` 列表末尾；MySQL 版同步追加到 `mysqlMigrationSQL()`，类型换成 `VARCHAR(64)/VARCHAR(255)/TEXT/BIGINT/DATETIME(6)`，风格照抄相邻表）：

```sql
CREATE TABLE IF NOT EXISTS rag_kbs (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    embed_provider TEXT NOT NULL DEFAULT 'system',
    embed_model    TEXT NOT NULL,
    embed_dims     INTEGER NOT NULL,
    chunk_size     INTEGER NOT NULL DEFAULT 512,
    chunk_overlap  INTEGER NOT NULL DEFAULT 64,
    status         TEXT NOT NULL DEFAULT 'active',
    created_at     TIMESTAMP NOT NULL,
    updated_at     TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rag_kbs_user ON rag_kbs (user_id);

CREATE TABLE IF NOT EXISTS rag_documents (
    id          TEXT PRIMARY KEY,
    kb_id       TEXT NOT NULL,
    file_name   TEXT NOT NULL,
    file_type   TEXT NOT NULL,
    file_size   INTEGER NOT NULL DEFAULT 0,
    object_key  TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'PENDING',
    error_msg   TEXT NOT NULL DEFAULT '',
    chunk_count INTEGER NOT NULL DEFAULT 0,
    token_count INTEGER NOT NULL DEFAULT 0,
    version     INTEGER NOT NULL DEFAULT 1,
    uploaded_at TIMESTAMP NOT NULL,
    indexed_at  TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_rag_documents_kb ON rag_documents (kb_id);

CREATE TABLE IF NOT EXISTS rag_index_tasks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    doc_id      TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'PENDING',
    retry_count INTEGER NOT NULL DEFAULT 0,
    max_retry   INTEGER NOT NULL DEFAULT 3,
    error_msg   TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMP NOT NULL,
    started_at  TIMESTAMP,
    finished_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_rag_tasks_status ON rag_index_tasks (status, created_at);
```

（SQLite `AUTOINCREMENT`/MySQL `AUTO_INCREMENT`/PG `BIGSERIAL`——照抄相邻自增表的跨方言处理方式；若现有 migrationSQL 是 SQLite/PG 共用一份，参考 `token_usage_daily` 等表的自增写法。）

**Store 接口**（`store.go` 接口声明区追加，与现有方法排版一致）：

```go
// RAG 知识库
CreateRAGKB(ctx context.Context, kb *RAGKBRecord) error
GetRAGKB(ctx context.Context, id string) (*RAGKBRecord, error)
ListRAGKBsByUser(ctx context.Context, userID string) ([]RAGKBRecord, error)
UpdateRAGKB(ctx context.Context, kb *RAGKBRecord) error
DeleteRAGKB(ctx context.Context, id string) error
CreateRAGDocument(ctx context.Context, doc *RAGDocumentRecord) error
GetRAGDocument(ctx context.Context, id string) (*RAGDocumentRecord, error)
ListRAGDocumentsByKB(ctx context.Context, kbID string) ([]RAGDocumentRecord, error)
UpdateRAGDocument(ctx context.Context, doc *RAGDocumentRecord) error
DeleteRAGDocument(ctx context.Context, id string) error
CreateRAGIndexTask(ctx context.Context, docID string, maxRetry int) (int64, error)
UpdateRAGIndexTask(ctx context.Context, id int64, status string, retryCount int, errMsg string) error
ListRunnableRAGIndexTasks(ctx context.Context) ([]RAGIndexTaskRecord, error)
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/store/ -run TestRAG -v
go test ./internal/store/ >NUL 2>&1 || go test ./internal/store/
```

预期：TestRAGKBCRUD、TestRAGDocumentAndTaskLifecycle PASS，且全包无回归。

- [ ] **Step 5: Commit**

```bash
git add internal/store/rag.go internal/store/rag_test.go internal/store/database.go internal/store/database_mysql.go internal/store/store.go
git commit -m "feat(store): RAG 三张表(kb/document/index_task)迁移与 CRUD"
```

---

### Task 3: config 配置结构与合并

**Files:**
- Modify: `internal/config/config.go`
- Test: 本包现有 config 测试文件（追加用例）

- [ ] **Step 1: 写失败测试**（追加到 config 包测试）

```go
func TestRAGAgentCfgMerge(t *testing.T) {
	fileCfg := AgentFileConfig{RAG: &RAGAgentCfg{KBs: []string{"kb_a", "kb_b"}, TopN: 8}}
	var resolved ResolvedAgent
	applyAgentFileConfig(&resolved, fileCfg) // 若合并函数名不同，以 config.go:944 附近实际合并点为准
	if len(resolved.RAG.KBs) != 2 || resolved.RAG.TopN != 8 {
		t.Fatalf("RAG cfg 未合并: %+v", resolved.RAG)
	}
}

func TestRAGCfgDefaults(t *testing.T) {
	var c RAGCfg
	c.ApplyDefaults()
	if c.Limits.MaxFileMB != 20 || c.Limits.MaxDocsPerKB != 200 || c.Limits.MaxKBsPerUser != 20 {
		t.Fatalf("默认配额错误: %+v", c.Limits)
	}
}
```

- [ ] **Step 2: 跑测试确认编译失败**

```bash
go test ./internal/config/ -run TestRAG -v
```

- [ ] **Step 3: 实现**（config.go；类型放在 SkillsCfg 附近，保持相邻）

```go
// RAGCfg 是系统级 RAG 配置（bkcrab.json 顶层 "rag"）。
// 用户级可通过 configs 表 scope=user 覆盖 Embedding 段。
type RAGCfg struct {
	Milvus    MilvusCfg       `json:"milvus,omitempty"`
	Embedding RAGEmbeddingCfg `json:"embedding,omitempty"`
	Limits    RAGLimitsCfg    `json:"limits,omitempty"`
}

type MilvusCfg struct {
	Address  string `json:"address,omitempty"` // host:port，空 = RAG 板块整体不可用
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// RAGEmbeddingCfg 描述一个 OpenAI 兼容 embeddings 端点。
// Endpoint 是 base URL（如 https://dashscope.aliyuncs.com/compatible-mode/v1），
// 客户端在其后拼 /embeddings。
type RAGEmbeddingCfg struct {
	Endpoint string `json:"endpoint,omitempty"`
	APIKey   string `json:"apiKey,omitempty"`
	Model    string `json:"model,omitempty"`
	Dims     int    `json:"dims,omitempty"`
}

type RAGLimitsCfg struct {
	MaxFileMB     int `json:"maxFileMB,omitempty"`
	MaxDocsPerKB  int `json:"maxDocsPerKB,omitempty"`
	MaxKBsPerUser int `json:"maxKBsPerUser,omitempty"`
}

func (c *RAGCfg) ApplyDefaults() {
	if c.Limits.MaxFileMB == 0 {
		c.Limits.MaxFileMB = 20
	}
	if c.Limits.MaxDocsPerKB == 0 {
		c.Limits.MaxDocsPerKB = 200
	}
	if c.Limits.MaxKBsPerUser == 0 {
		c.Limits.MaxKBsPerUser = 20
	}
}

// Available 报告系统级 RAG 是否配置完整（Milvus 地址 + embedding 端点/模型/维度）。
func (c *RAGCfg) Available() bool {
	return c.Milvus.Address != "" && c.Embedding.Endpoint != "" &&
		c.Embedding.Model != "" && c.Embedding.Dims > 0
}

// RAGAgentCfg 是 agent 级授权（agents.config JSON 的 "rag" 字段）。
// KBs 引用 agent 所属用户自己的 KB id；装配时校验归属并剔除悬空引用。
type RAGAgentCfg struct {
	KBs  []string `json:"kbs,omitempty"`
	TopN int      `json:"topN,omitempty"` // 0 = 默认 5
}
```

接线点（都在 config.go 内）：
1. `Config` 结构体（约 :312 `Skills SkillsCfg` 附近）加 `RAG RAGCfg \`json:"rag,omitempty"\``；
2. `AgentFileConfig`（:663）加 `RAG *RAGAgentCfg \`json:"rag,omitempty"\``；
3. `ResolvedAgent` 加 `RAG RAGAgentCfg` 字段；
4. 文件配置合并点（:944 附近 `resolved.Skills = fileCfg.Skills` 同一段）加：`if fileCfg.RAG != nil { resolved.RAG = *fileCfg.RAG }`；
5. 系统配置解析处（fileCfg → resolved 的同段，:944 附近 `resolved.Skills = fileCfg.Skills`）加 `resolved.RAG = fileCfg.RAG` 并调 `ApplyDefaults()`。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/config/ -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/*_test.go
git commit -m "feat(config): RAG 系统级/agent 级配置结构与合并"
```

---

### Task 4: rag/split 分块器

**Files:**
- Create: `internal/rag/split/split.go`, `internal/rag/split/split_test.go`

- [ ] **Step 1: 写失败测试**

```go
package split

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"你好世界", 4},            // CJK 每字 1 token
		{"hello world!", 3},     // 12 rune / 4 = 3
		{"中文 mixed text", 5},    // 2 CJK + 11 其它 rune → 2 + ceil(11/4)=3 → 5
	}
	for _, c := range cases {
		if got := EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSlidingWindowRespectsSizeAndOverlap(t *testing.T) {
	// 40 个句子，每句约 12 token，chunk_size=100 → 每块约 8 句
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString("这是一个用于测试的句子。")
	}
	chunks := SlidingWindow(sb.String(), Config{ChunkSize: 100, ChunkOverlap: 20}, "", 0)
	if len(chunks) < 4 {
		t.Fatalf("期望切成多块, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Tokens > 130 { // 允许句子边界溢出但不能失控
			t.Errorf("chunk %d tokens=%d 超限", i, c.Tokens)
		}
		if c.Index != i {
			t.Errorf("chunk index 不连续: %d != %d", c.Index, i)
		}
	}
	// overlap：后一块开头应包含前一块结尾的内容
	if !strings.Contains(chunks[1].Content, "测试的句子") {
		t.Errorf("chunk1 未包含重叠内容")
	}
}

func TestMarkdownStructureSplit(t *testing.T) {
	md := `# 安装指南

前置要求正文。

## 下载

下载步骤正文，很短。

## 配置

` + strings.Repeat("配置项说明。", 200) + `
`
	chunks := Markdown(md, Config{ChunkSize: 200, ChunkOverlap: 30})
	if len(chunks) < 3 {
		t.Fatalf("结构分块数量不足: %d", len(chunks))
	}
	var sawDownload, sawConfig bool
	for _, c := range chunks {
		if c.SectionTitle == "安装指南 > 下载" {
			sawDownload = true
		}
		if strings.HasPrefix(c.SectionTitle, "安装指南 > 配置") {
			sawConfig = true
			if c.Tokens > 260 {
				t.Errorf("长节应再被滑窗切分, tokens=%d", c.Tokens)
			}
		}
	}
	if !sawDownload || !sawConfig {
		t.Fatalf("section title 缺失: download=%v config=%v", sawDownload, sawConfig)
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Fatalf("全文 chunk index 必须连续")
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/rag/split/ -v
```

- [ ] **Step 3: 实现**（split.go）

```go
// Package split 把解析后的纯文本切成可检索的 chunk。
// 两个策略：Markdown 结构感知（标题层级切 + 超长节内滑窗），
// SlidingWindow 滑动窗口（无结构文本的兜底）。token 数是估算值，
// 只用于控制 chunk 大小，不追求与任何 tokenizer 精确一致。
package split

import (
	"strings"
	"unicode"
)

type Config struct {
	ChunkSize    int // 目标 token 数，默认 512
	ChunkOverlap int // 相邻 chunk 重叠 token 数，默认 64
}

func (c *Config) normalize() {
	if c.ChunkSize <= 0 {
		c.ChunkSize = 512
	}
	if c.ChunkOverlap < 0 || c.ChunkOverlap >= c.ChunkSize {
		c.ChunkOverlap = c.ChunkSize / 8
	}
}

type Chunk struct {
	Index        int
	Content      string
	SectionTitle string
	PageNum      int
	Tokens       int
}

// EstimateTokens：CJK 每字计 1，其余每 4 rune 计 1（向上取整）。
func EstimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// splitSentences 按中英句末标点与换行切句，保留分隔符。
func splitSentences(text string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range text {
		cur.WriteRune(r)
		switch r {
		case '。', '！', '？', '；', '.', '!', '?', ';', '\n':
			if s := cur.String(); strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
			cur.Reset()
		}
	}
	if s := cur.String(); strings.TrimSpace(s) != "" {
		out = append(out, s)
	}
	return out
}

// SlidingWindow 贪心装句到 ChunkSize，块间保留约 ChunkOverlap token 的尾部句子。
func SlidingWindow(text string, cfg Config, sectionTitle string, pageNum int) []Chunk {
	cfg.normalize()
	sents := splitSentences(text)
	var chunks []Chunk
	var buf []string
	bufTokens := 0
	flush := func() {
		if bufTokens == 0 {
			return
		}
		content := strings.TrimSpace(strings.Join(buf, ""))
		chunks = append(chunks, Chunk{
			Index: len(chunks), Content: content,
			SectionTitle: sectionTitle, PageNum: pageNum,
			Tokens: EstimateTokens(content),
		})
		// 保留尾部句子作为下一块的重叠前缀
		var keep []string
		keepTokens := 0
		for i := len(buf) - 1; i >= 0 && keepTokens < cfg.ChunkOverlap; i-- {
			keep = append([]string{buf[i]}, keep...)
			keepTokens += EstimateTokens(buf[i])
		}
		buf = keep
		bufTokens = keepTokens
	}
	for _, s := range sents {
		st := EstimateTokens(s)
		if bufTokens+st > cfg.ChunkSize && bufTokens > 0 {
			flush()
		}
		buf = append(buf, s)
		bufTokens += st
	}
	// 最后一段（不重复 flush 的 overlap 保留逻辑）
	if bufTokens > 0 {
		content := strings.TrimSpace(strings.Join(buf, ""))
		if content != "" {
			// 若与上一块内容完全相同（纯 overlap 残留）则丢弃
			if n := len(chunks); n == 0 || chunks[n-1].Content != content {
				chunks = append(chunks, Chunk{
					Index: len(chunks), Content: content,
					SectionTitle: sectionTitle, PageNum: pageNum,
					Tokens: EstimateTokens(content),
				})
			}
		}
	}
	return chunks
}

type section struct {
	title string // 面包屑："安装指南 > 下载"
	body  strings.Builder
}

// Markdown 按 ATX 标题（#..######）切节，节内超长再滑窗；
// 返回的 chunk Index 全文连续。
func Markdown(md string, cfg Config) []Chunk {
	cfg.normalize()
	var sections []*section
	stack := []string{} // 标题面包屑栈，stack[i] 是第 i+1 级标题文本
	cur := &section{}
	sections = append(sections, cur)
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		level := 0
		for level < len(trimmed) && level < 6 && trimmed[level] == '#' {
			level++
		}
		if level > 0 && level < len(trimmed) && trimmed[level] == ' ' {
			title := strings.TrimSpace(trimmed[level:])
			if level <= len(stack) {
				stack = stack[:level-1]
			}
			for len(stack) < level-1 {
				stack = append(stack, "")
			}
			stack = append(stack, title)
			cur = &section{title: strings.Join(stack, " > ")}
			sections = append(sections, cur)
			continue
		}
		cur.body.WriteString(line)
		cur.body.WriteString("\n")
	}
	var out []Chunk
	for _, sec := range sections {
		body := strings.TrimSpace(sec.body.String())
		if body == "" {
			continue
		}
		for _, c := range SlidingWindow(body, cfg, sec.title, 0) {
			c.Index = len(out)
			out = append(out, c)
		}
	}
	return out
}

// Pages 对逐页文本（PDF）做滑窗分块，保留页码；Index 全文连续。
func Pages(pages []string, cfg Config) []Chunk {
	var out []Chunk
	for i, p := range pages {
		for _, c := range SlidingWindow(p, cfg, "", i+1) {
			c.Index = len(out)
			out = append(out, c)
		}
	}
	return out
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/rag/split/ -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/split/
git commit -m "feat(rag): 分块器——markdown 结构感知 + 滑动窗口 + token 估算"
```

---

### Task 5: rag/parse 文档解析

**Files:**
- Create: `internal/rag/parse/parse.go`, `internal/rag/parse/docx.go`, `internal/rag/parse/pdf.go`, `internal/rag/parse/parse_test.go`, `internal/rag/parse/testdata/sample.md`

- [ ] **Step 1: 写失败测试**

```go
package parse

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestParseMarkdownAndTxt(t *testing.T) {
	r, err := Parse(strings.NewReader("# 标题\n\n正文"), "a.md")
	if err != nil || r.Format != "md" || !strings.Contains(r.Pages[0].Text, "# 标题") {
		t.Fatalf("md: %+v err=%v", r, err)
	}
	r, err = Parse(strings.NewReader("纯文本"), "b.TXT")
	if err != nil || r.Format != "txt" {
		t.Fatalf("txt: %+v err=%v", r, err)
	}
}

func TestParseRejectsUnknownExt(t *testing.T) {
	if _, err := Parse(strings.NewReader("x"), "a.exe"); err == nil {
		t.Fatal("未知扩展名应报错")
	}
}

// makeDocx 在内存里构造最小 docx：两段，第一段 Heading1 样式。
func makeDocx(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	doc, _ := w.Create("word/document.xml")
	doc.Write([]byte(`<?xml version="1.0"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>
<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>第一章</w:t></w:r></w:p>
<w:p><w:r><w:t>这是正文</w:t></w:r><w:r><w:t>第二段落。</w:t></w:r></w:p>
</w:body></w:document>`))
	w.Close()
	return buf.Bytes()
}

func TestParseDocx(t *testing.T) {
	b := makeDocx(t)
	r, err := Parse(bytes.NewReader(b), "c.docx")
	if err != nil {
		t.Fatalf("docx: %v", err)
	}
	text := r.Pages[0].Text
	if !strings.Contains(text, "# 第一章") {
		t.Errorf("Heading1 应转成 markdown 标题, got: %q", text)
	}
	if !strings.Contains(text, "这是正文第二段落。") {
		t.Errorf("同段 w:t 应拼接, got: %q", text)
	}
}

func TestParseEmptyContent(t *testing.T) {
	if _, err := Parse(strings.NewReader("   \n  "), "a.txt"); err == nil {
		t.Fatal("空内容应报错(扫描件/空文件都走这条错误)")
	}
}
```

（PDF 用例：`testdata/` 放一个一页的最小 PDF——执行时用任意工具生成一个含 "hello rag" 文本的单页 PDF 提交进 testdata，测试断言 `Parse` 出的文本包含 `hello rag`。若无现成工具，可用 Python reportlab 或在系统里打印导出；生成后固定提交，不在测试运行时生成。）

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/rag/parse/ -v
```

- [ ] **Step 3: 实现**

`parse.go`：

```go
// Package parse 把上传的文档统一解析成逐页纯文本（MD 保留原文以便结构分块）。
package parse

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

type Page struct {
	Num  int
	Text string
}

type Result struct {
	Format string // md / txt / pdf / docx
	Pages  []Page // md/txt/docx 只有一页；pdf 逐页
}

var ErrEmptyContent = errors.New("文档无有效文本内容(扫描件或空文件)")

// SupportedExt 报告扩展名是否受支持（上传入口用它做校验）。
func SupportedExt(fileName string) bool {
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".md", ".markdown", ".txt", ".pdf", ".docx":
		return true
	}
	return false
}

func Parse(r io.Reader, fileName string) (*Result, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("读取文件: %w", err)
	}
	ext := strings.ToLower(filepath.Ext(fileName))
	var res *Result
	switch ext {
	case ".md", ".markdown":
		res = &Result{Format: "md", Pages: []Page{{Num: 1, Text: string(data)}}}
	case ".txt":
		res = &Result{Format: "txt", Pages: []Page{{Num: 1, Text: string(data)}}}
	case ".docx":
		text, err := extractDocx(data)
		if err != nil {
			return nil, err
		}
		res = &Result{Format: "docx", Pages: []Page{{Num: 1, Text: text}}}
	case ".pdf":
		pages, err := extractPDF(data)
		if err != nil {
			return nil, err
		}
		res = &Result{Format: "pdf", Pages: pages}
	default:
		return nil, fmt.Errorf("不支持的文件类型 %q(支持 md/txt/pdf/docx)", ext)
	}
	total := 0
	for _, p := range res.Pages {
		total += len(strings.TrimSpace(p.Text))
	}
	if total == 0 {
		return nil, ErrEmptyContent
	}
	return res, nil
}
```

`docx.go`（零新依赖；Heading 样式转 markdown 标题，让下游直接走结构分块）：

```go
package parse

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// extractDocx 解 zip 读 word/document.xml，按段落提取文本。
// pStyle 为 HeadingN / N 级标题样式时输出 "#"*N 前缀，正文原样。
func extractDocx(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx 不是合法 zip: %w", err)
	}
	var docXML io.ReadCloser
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docXML, err = f.Open()
			if err != nil {
				return "", err
			}
			break
		}
	}
	if docXML == nil {
		return "", fmt.Errorf("docx 缺少 word/document.xml")
	}
	defer docXML.Close()

	dec := xml.NewDecoder(docXML)
	var out strings.Builder
	var para strings.Builder
	headingLevel := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docx xml 解析: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				para.Reset()
				headingLevel = 0
			case "pStyle":
				for _, a := range t.Attr {
					if a.Name.Local == "val" {
						headingLevel = headingStyleLevel(a.Value)
					}
				}
			case "t":
				var txt string
				if err := dec.DecodeElement(&txt, &t); err == nil {
					para.WriteString(txt)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "p" {
				line := strings.TrimSpace(para.String())
				if line == "" {
					continue
				}
				if headingLevel > 0 {
					out.WriteString(strings.Repeat("#", headingLevel) + " ")
				}
				out.WriteString(line)
				out.WriteString("\n\n")
			}
		}
	}
	return out.String(), nil
}

// headingStyleLevel 识别 "Heading1"、"heading 2"、"1"（中文 Word 常见样式 id）等。
func headingStyleLevel(styleVal string) int {
	s := strings.ToLower(strings.ReplaceAll(styleVal, " ", ""))
	s = strings.TrimPrefix(s, "heading")
	if len(s) == 1 && s[0] >= '1' && s[0] <= '6' {
		return int(s[0] - '0')
	}
	return 0
}
```

`pdf.go`：

```go
package parse

import (
	"bytes"
	"fmt"

	pdflib "github.com/ledongthuc/pdf"
)

// extractPDF 逐页提取文本。ledongthuc/pdf 只做文本层提取，
// 扫描件没有文本层——由 Parse 的空内容检查统一报错。
func extractPDF(data []byte) ([]Page, error) {
	r, err := pdflib.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("pdf 解析: %w", err)
	}
	var pages []Page
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			continue // 单页失败跳过，整篇空由上层兜底
		}
		pages = append(pages, Page{Num: i, Text: text})
	}
	return pages, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/rag/parse/ -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/parse/
git commit -m "feat(rag): 文档解析 md/txt/docx/pdf(纯文本提取,无 OCR)"
```

---

### Task 6: rag/embed OpenAI 兼容 embeddings 客户端

**Files:**
- Create: `internal/rag/embed/embed.go`, `internal/rag/embed/embed_test.go`

- [ ] **Step 1: 写失败测试**（httptest 假服务端：断言批大小 ≤16、Authorization 头、model 字段；返回可控向量；一条错误路径断言维度不符报错）

```go
package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbedBatchesAndDims(t *testing.T) {
	var batches [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth 头缺失")
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "m1" {
			t.Errorf("model = %s", req.Model)
		}
		if len(req.Input) > 16 {
			t.Errorf("批大小超限: %d", len(req.Input))
		}
		batches = append(batches, req.Input)
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		resp := struct {
			Data []item `json:"data"`
		}{}
		for i := range req.Input {
			resp.Data = append(resp.Data, item{Embedding: []float32{1, 2, 3}, Index: i})
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key", "m1", 3)
	texts := make([]string, 20)
	for i := range texts {
		texts[i] = "t"
	}
	vecs, err := c.Embed(context.Background(), texts)
	if err != nil || len(vecs) != 20 {
		t.Fatalf("embed: n=%d err=%v", len(vecs), err)
	}
	if len(batches) != 2 {
		t.Fatalf("应分 2 批, got %d", len(batches))
	}
}

func TestEmbedDimsMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "m", 3)
	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("维度不符应报错")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/rag/embed/ -v
```

- [ ] **Step 3: 实现**（embed.go）

```go
// Package embed 是 OpenAI 兼容 /embeddings 端点的最小客户端。
// endpoint 是 base URL(如 https://api.openai.com/v1)，请求体 {model, input[]}。
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const batchSize = 16

type Client struct {
	endpoint string
	apiKey   string
	model    string
	dims     int
	hc       *http.Client
}

func New(endpoint, apiKey, model string, dims int) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey, model: model, dims: dims,
		hc: &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) Model() string { return c.model }
func (c *Client) Dims() int     { return c.dims }

func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := min(start+batchSize, len(texts))
		vecs, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("embedding 批次 %d-%d: %w", start, end, err)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": c.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("embeddings 端点返回 %d: %s", resp.StatusCode, string(b))
	}
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("解析响应: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("返回向量数 %d != 输入数 %d", len(parsed.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("非法 index %d", d.Index)
		}
		if len(d.Embedding) != c.dims {
			return nil, fmt.Errorf("维度不符: 期望 %d 实际 %d(检查 KB 绑定的模型与端点)", c.dims, len(d.Embedding))
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/rag/embed/ -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/embed/
git commit -m "feat(rag): openai 兼容 embeddings 客户端(批量+维度校验)"
```

---

### Task 7: rag/vector 接口 + 内存 fake

**Files:**
- Create: `internal/rag/vector/vector.go`, `internal/rag/vector/fake.go`, `internal/rag/vector/fake_test.go`

- [ ] **Step 1: 写失败测试**（fake 的语义即接口契约：版本化删除、双路召回融合）

```go
package vector

import (
	"context"
	"testing"
)

func mkChunk(docID string, idx, version int, content string, vec []float32) ChunkData {
	return ChunkData{DocID: docID, Index: idx, Content: content, DocVersion: version, Vector: vec}
}

func TestFakeUpsertSearchAndVersionDelete(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	if err := f.EnsureCollection(ctx, "kb1", 2); err != nil {
		t.Fatal(err)
	}
	chunks := []ChunkData{
		mkChunk("d1", 0, 1, "北京的天气预报", []float32{1, 0}),
		mkChunk("d1", 1, 1, "上海美食推荐指南", []float32{0, 1}),
	}
	if err := f.UpsertChunks(ctx, "kb1", chunks); err != nil {
		t.Fatal(err)
	}
	// 向量相近 + 关键词命中 → 融合后第一条应是天气
	hits, err := f.HybridSearch(ctx, "kb1", []float32{0.9, 0.1}, "天气", 2)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search: %v err=%v", hits, err)
	}
	if hits[0].DocID != "d1" || hits[0].ChunkIndex != 0 {
		t.Fatalf("top1 应是天气 chunk: %+v", hits[0])
	}
	// 版本化替换：写 v2 后删 v1，旧 chunk 消失
	if err := f.UpsertChunks(ctx, "kb1", []ChunkData{mkChunk("d1", 0, 2, "只剩这一条", []float32{1, 0})}); err != nil {
		t.Fatal(err)
	}
	if err := f.DeleteOldVersions(ctx, "kb1", "d1", 2); err != nil {
		t.Fatal(err)
	}
	hits, _ = f.HybridSearch(ctx, "kb1", []float32{0, 1}, "", 10)
	if len(hits) != 1 || hits[0].Content != "只剩这一条" {
		t.Fatalf("旧版本应被删净: %+v", hits)
	}
	// DropCollection 后查询报错
	if err := f.DropCollection(ctx, "kb1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.HybridSearch(ctx, "kb1", []float32{1, 0}, "x", 1); err == nil {
		t.Fatal("collection 已删应报错")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/rag/vector/ -v
```

- [ ] **Step 3: 实现**

`vector.go`：

```go
// Package vector 定义 RAG 的向量库窄接口。首个生产实现是 Milvus；
// Fake 供单测。实现必须保证：UpsertChunks 与 DeleteOldVersions 分离，
// 让调用方能实现"先算后删再写"——新版本写入成功后才删旧版本。
package vector

import "context"

type ChunkData struct {
	DocID        string
	Index        int
	Content      string
	SectionTitle string
	PageNum      int
	DocVersion   int
	Vector       []float32
}

type SearchHit struct {
	DocID        string
	ChunkIndex   int
	Content      string
	SectionTitle string
	PageNum      int
	Score        float64
}

type Store interface {
	EnsureCollection(ctx context.Context, kbID string, dims int) error
	UpsertChunks(ctx context.Context, kbID string, chunks []ChunkData) error
	// DeleteOldVersions 删除 docID 中 doc_version < keepVersion 的实体。
	DeleteOldVersions(ctx context.Context, kbID, docID string, keepVersion int) error
	// DeleteDoc 删除该文档全部版本的实体（删文档用）。
	DeleteDoc(ctx context.Context, kbID, docID string) error
	DropCollection(ctx context.Context, kbID string) error
	// HybridSearch：dense 向量路 + 全文路（queryText 为空则只走向量路），
	// RRF 融合后取 topK。
	HybridSearch(ctx context.Context, kbID string, queryVec []float32, queryText string, topK int) ([]SearchHit, error)
}
```

`fake.go`：内存 map（kbID → []entry）+ 读写锁。dense 路按余弦相似度排序；全文路按"query 的每个 2-gram（中文）/小写词（英文）在 content 中出现的个数"排序（够单测断言用即可）；两路各取 topK 后按 `1/(60+rank+1)` 做 RRF 融合，同 chunk 分数累加，输出按分数降序。`EnsureCollection` 幂等记录 dims，dims 与已有不符报错；未 Ensure 的 kbID 一律报 `fmt.Errorf("collection %s 不存在", kbID)`。代码直接照上述语义实现（约 120 行），全部行为都有上面测试覆盖。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/rag/vector/ -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/vector/
git commit -m "feat(rag): VectorStore 窄接口与内存 fake(RRF 混合检索语义)"
```

---

### Task 8: rag/vector Milvus 实现

**Files:**
- Create: `internal/rag/vector/milvus.go`, `internal/rag/vector/milvus_test.go`

**先决:** 本任务的 SDK 符号以 Task 1 拉到的版本为准。写码前先看
`https://milvus.io/docs/full-text-search.md` 与 Go SDK 的
`client/v2/milvusclient` 包文档（hybrid search + BM25 function 示例）。
下面代码按 Milvus 2.5 / client v2 的公开 API 编写，符号有出入时以 SDK 为准调整，语义不变。

- [ ] **Step 1: 写 env 门控集成测试**（`milvus_test.go`；无 `RAG_TEST_MILVUS_ADDR` 时 Skip——本地 `docker run -d -p 19530:19530 milvusdb/milvus:v2.5-latest standalone` 后 `set RAG_TEST_MILVUS_ADDR=localhost:19530` 运行）

```go
package vector

import (
	"context"
	"os"
	"testing"
)

func TestMilvusRoundTrip(t *testing.T) {
	addr := os.Getenv("RAG_TEST_MILVUS_ADDR")
	if addr == "" {
		t.Skip("RAG_TEST_MILVUS_ADDR 未设置,跳过 Milvus 集成测试")
	}
	m, err := NewMilvus(context.Background(), addr, "", "")
	if err != nil {
		t.Fatal(err)
	}
	kbID := "test_roundtrip"
	ctx := context.Background()
	_ = m.DropCollection(ctx, kbID)
	if err := m.EnsureCollection(ctx, kbID, 4); err != nil {
		t.Fatal(err)
	}
	defer m.DropCollection(ctx, kbID)
	chunks := []ChunkData{
		{DocID: "d1", Index: 0, Content: "北京今天天气晴朗", DocVersion: 1, Vector: []float32{1, 0, 0, 0}},
		{DocID: "d1", Index: 1, Content: "上海美食小笼包很有名", DocVersion: 1, Vector: []float32{0, 1, 0, 0}},
	}
	if err := m.UpsertChunks(ctx, kbID, chunks); err != nil {
		t.Fatal(err)
	}
	hits, err := m.HybridSearch(ctx, kbID, []float32{0.9, 0.1, 0, 0}, "天气", 2)
	if err != nil || len(hits) == 0 || hits[0].ChunkIndex != 0 {
		t.Fatalf("hybrid: %+v err=%v", hits, err)
	}
	if err := m.UpsertChunks(ctx, kbID, []ChunkData{{DocID: "d1", Index: 0, Content: "新版本", DocVersion: 2, Vector: []float32{1, 0, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteOldVersions(ctx, kbID, "d1", 2); err != nil {
		t.Fatal(err)
	}
	hits, _ = m.HybridSearch(ctx, kbID, []float32{1, 0, 0, 0}, "", 10)
	if len(hits) != 1 {
		t.Fatalf("旧版本应删净: %+v", hits)
	}
}
```

- [ ] **Step 2: 实现**（milvus.go；要点如下，全部落码）

```go
package vector

// Milvus 实现要点（对照 SDK 调整符号，语义固定）：
//
// collection 命名：ragCollectionName(kbID) = "rag_" + 非法字符替换为 "_" 的 kbID。
//
// EnsureCollection(kbID, dims)：
//   HasCollection 已存在 → 直接返回（幂等）。
//   schema 字段：
//     id           VarChar(128)  PK，值 = fmt.Sprintf("%s_%d_%d", docID, docVersion, index)
//     doc_id       VarChar(128)
//     chunk_index  Int64
//     section_title VarChar(512)
//     page_num     Int64
//     doc_version  Int64
//     content      VarChar(65535)  WithEnableAnalyzer(true)
//                  analyzer_params: {"type":"chinese"}   // jieba 分词，中文 BM25 的关键
//     content_sparse SparseFloatVector                    // BM25 函数输出
//     embedding    FloatVector(dims)
//   Function: BM25, input=content, output=content_sparse。
//   索引：embedding → AUTOINDEX(metric COSINE)；content_sparse →
//   SPARSE_INVERTED_INDEX(metric BM25)。建完 LoadCollection。
//
// UpsertChunks：按列或按行构造数据后 client.Upsert；PK 同值覆盖，
//   同一 doc_version 重跑天然幂等。
//
// DeleteOldVersions：client.Delete，expr:
//   fmt.Sprintf(`doc_id == "%s" && doc_version < %d`, escape(docID), keepVersion)
// DeleteDoc：expr: doc_id == "..."
// DropCollection：client.DropCollection，NotFound 视为成功。
//
// HybridSearch：
//   queryText 为空 → 单路 client.Search(embedding, COSINE, topK)。
//   否则 client.HybridSearch(
//     NewAnnRequest("embedding", topK, FloatVector(queryVec)),
//     NewAnnRequest("content_sparse", topK, Text(queryText)),  // BM25 全文路
//   ).WithReranker(NewRRFReranker().WithK(60))
//   OutputFields: doc_id, chunk_index, content, section_title, page_num。
//   结果映射为 []SearchHit，Score 用返回的融合分。
//
// escape(s)：把 `"` 与 `\` 转义，防 expr 注入（docID 是我们生成的 uuid，
// 转义是纵深防御）。
```

实现完整文件（约 250 行），连接在 `NewMilvus(ctx, addr, user, pass)` 中建立（`milvusclient.New(ctx, &milvusclient.ClientConfig{Address, Username, Password})`），持有单个 client，方法并发安全（SDK client 本身线程安全）。

- [ ] **Step 3: 编译 + （可选）本地集成验证**

```bash
go build ./internal/rag/vector/
go test ./internal/rag/vector/ -run TestMilvus -v   # 无 env 时输出 SKIP 即为预期
```

- [ ] **Step 4: Commit**

```bash
git add internal/rag/vector/milvus.go internal/rag/vector/milvus_test.go
git commit -m "feat(rag): Milvus VectorStore 实现(BM25 全文+dense 混合,原生 RRF)"
```

---

### Task 9: rag/objects 原件存储

**Files:**
- Create: `internal/rag/objects/objects.go`, `internal/rag/objects/objects_test.go`

- [ ] **Step 1: 写失败测试**（LocalFS 实现 + Key 形状）

```go
package objects

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestKeyShape(t *testing.T) {
	k := Key("u1", "kb1", "doc1", "手册 v2.pdf")
	if k != "rag/u1/kb1/doc1/手册 v2.pdf" {
		t.Fatalf("key = %q", k)
	}
	if strings.Contains(Key("u1", "kb1", "doc1", "../../etc/passwd"), "..") {
		t.Fatal("文件名必须被清洗,不能带路径穿越")
	}
}

func TestLocalFSRoundTrip(t *testing.T) {
	s := NewLocalFS(t.TempDir())
	ctx := context.Background()
	key := Key("u1", "kb1", "doc1", "a.md")
	if err := s.Put(ctx, key, bytes.NewReader([]byte("hello")), 5, "text/markdown"); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}
	if err := s.DeletePrefix(ctx, "rag/u1/kb1/"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, key); err == nil {
		t.Fatal("删除前缀后应取不到")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/rag/objects/ -v
```

- [ ] **Step 3: 实现**（objects.go）

```go
// Package objects 存 RAG 原始文档。接口刻意窄：流水线只需要
// Put/Get/DeletePrefix。生产用 S3(MinIO)，开发/单测用 LocalFS。
package objects

import (
	"context"
	"io"
	"path"
	"strings"
)

type Store interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	DeletePrefix(ctx context.Context, prefix string) error
}

// Key 生成 rag/<user>/<kb>/<doc>/<fileName>。fileName 只保留基名，
// 斜杠与 ".." 全部清洗掉，防路径穿越。
func Key(userID, kbID, docID, fileName string) string {
	base := path.Base(strings.ReplaceAll(fileName, "\\", "/"))
	base = strings.ReplaceAll(base, "..", "_")
	if base == "" || base == "." || base == "/" {
		base = "file"
	}
	return path.Join("rag", userID, kbID, docID, base)
}
```

- `LocalFS`：root + `filepath.FromSlash(key)`；Put 建父目录写临时文件后 rename；DeletePrefix 用 `os.RemoveAll(root/前缀目录)`（约束 prefix 必须以 "/" 结尾且至少三段，防误删根）。
- `S3`：字段 `client *minio.Client, bucket string`；`NewS3(cfg workspace.S3Config)` 复用同一配置结构（import `internal/workspace`），构造方式照抄 `workspace.NewS3` 里的 `minio.New(...)` 三行；Put→`client.PutObject`，Get→`GetObject`+首字节探测 NotFound，DeletePrefix→`ListObjects(prefix)` 循环 `RemoveObject`。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/rag/objects/ -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/objects/
git commit -m "feat(rag): 原件对象存储(S3/LocalFS,rag 前缀键)"
```

---

### Task 10: rag.Service —— KB 管理

**Files:**
- Create: `internal/rag/service.go`, `internal/rag/service_test.go`

- [ ] **Step 1: 写失败测试**（fake vector + LocalFS objects + SQLite store）

```go
package rag

import (
	"context"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/vector"
)

func newTestService(t *testing.T) (*Service, *vector.Fake) {
	t.Helper()
	st := newTestStore(t) // 参照 store 包单测的 SQLite 构造方式，通过 store.NewDBStore("sqlite", ...) 建
	fake := vector.NewFake()
	cfg := config.RAGCfg{
		Milvus:    config.MilvusCfg{Address: "fake"},
		Embedding: config.RAGEmbeddingCfg{Endpoint: "http://fake", Model: "m1", Dims: 4},
	}
	cfg.ApplyDefaults()
	svc := New(Deps{Store: st, Vector: fake, Objects: objects.NewLocalFS(t.TempDir()), Cfg: cfg})
	return svc, fake
}

func TestCreateKBSnapshotsEmbeddingAndEnsuresCollection(t *testing.T) {
	svc, fake := newTestService(t)
	kb, err := svc.CreateKB(context.Background(), "u1", "产品手册", "说明", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if kb.EmbedModel != "m1" || kb.EmbedDims != 4 {
		t.Fatalf("embedding 未快照: %+v", kb)
	}
	if kb.ChunkSize != 512 || kb.ChunkOverlap != 64 {
		t.Fatalf("分块默认值: %+v", kb)
	}
	if !fake.HasCollection(kb.ID) {
		t.Fatal("建库必须同步建 collection")
	}
}

func TestCreateKBQuota(t *testing.T) {
	svc, _ := newTestService(t)
	svc.cfg.Limits.MaxKBsPerUser = 1
	if _, err := svc.CreateKB(context.Background(), "u1", "a", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateKB(context.Background(), "u1", "b", "", 0, 0); err == nil {
		t.Fatal("超配额应报错")
	}
}

func TestDeleteKBCascades(t *testing.T) {
	svc, fake := newTestService(t)
	ctx := context.Background()
	kb, _ := svc.CreateKB(ctx, "u1", "a", "", 0, 0)
	if err := svc.DeleteKB(ctx, "u1", kb.ID); err != nil {
		t.Fatal(err)
	}
	if fake.HasCollection(kb.ID) {
		t.Fatal("删库必须 drop collection")
	}
	if _, err := svc.GetKB(ctx, "u1", kb.ID); err == nil {
		t.Fatal("KB 行应已删除")
	}
}

func TestGetKBEnforcesOwnership(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	kb, _ := svc.CreateKB(ctx, "u1", "a", "", 0, 0)
	if _, err := svc.GetKB(ctx, "u2", kb.ID); err == nil {
		t.Fatal("跨用户访问必须拒绝")
	}
}
```

（fake 需补一个测试辅助方法 `HasCollection(kbID) bool`，加在 fake.go。）

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/rag/ -v
```

- [ ] **Step 3: 实现**（service.go）

```go
// Package rag 是 RAG 板块的门面：KB 管理、文档入库流水线、检索。
// 所有方法带 ownerID 参数并做归属校验——HTTP 层与工具层都不重复实现权限。
package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

var (
	ErrNotFound  = errors.New("知识库或文档不存在")
	ErrForbidden = errors.New("无权访问该知识库")
	ErrQuota     = errors.New("超出配额限制")
)

// UserEmbedCfgFn 查用户级 embedding 覆盖（configs 表 scope=user）。
// 返回 false 表示无覆盖，用系统级。
type UserEmbedCfgFn func(ctx context.Context, userID string) (config.RAGEmbeddingCfg, bool)

type Deps struct {
	Store        store.Store
	Vector       vector.Store
	Objects      objects.Store
	Cfg          config.RAGCfg
	UserEmbedCfg UserEmbedCfgFn // 可为 nil：只用系统级
	Workers      int            // 索引 worker 数，0 = 2
}

type Service struct {
	st      store.Store
	vec     vector.Store
	obj     objects.Store
	cfg     config.RAGCfg
	userCfg UserEmbedCfgFn
	tasks   chan int64 // 待执行 task id 队列（pipeline.go）
}

func New(d Deps) *Service {
	if d.Workers <= 0 {
		d.Workers = 2
	}
	s := &Service{
		st: d.Store, vec: d.Vector, obj: d.Objects,
		cfg: d.Cfg, userCfg: d.UserEmbedCfg,
		tasks: make(chan int64, 256),
	}
	return s
}

// resolveEmbedding：endpoint/key 取用户覆盖(若有)否则系统级；
// model/dims 建库时来自同一来源做快照，检索/入库时改从 KB 快照读。
func (s *Service) resolveEmbedding(ctx context.Context, userID string) (config.RAGEmbeddingCfg, string) {
	if s.userCfg != nil {
		if c, ok := s.userCfg(ctx, userID); ok && c.Endpoint != "" && c.Model != "" && c.Dims > 0 {
			return c, "user"
		}
	}
	return s.cfg.Embedding, "system"
}

// embedderForKB：端点与 key 按当前配置解析，模型与维度永远用 KB 快照。
func (s *Service) embedderForKB(ctx context.Context, kb *store.RAGKBRecord) *embed.Client {
	ec, _ := s.resolveEmbedding(ctx, kb.UserID)
	return embed.New(ec.Endpoint, ec.APIKey, kb.EmbedModel, kb.EmbedDims)
}

func (s *Service) CreateKB(ctx context.Context, userID, name, description string, chunkSize, chunkOverlap int) (*store.RAGKBRecord, error) {
	if name == "" {
		return nil, errors.New("知识库名称不能为空")
	}
	existing, err := s.st.ListRAGKBsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(existing) >= s.cfg.Limits.MaxKBsPerUser {
		return nil, fmt.Errorf("%w: 每用户最多 %d 个知识库", ErrQuota, s.cfg.Limits.MaxKBsPerUser)
	}
	ec, src := s.resolveEmbedding(ctx, userID)
	if ec.Endpoint == "" || ec.Model == "" || ec.Dims <= 0 {
		return nil, errors.New("embedding 未配置,请先在系统或用户设置中配置")
	}
	if chunkSize <= 0 {
		chunkSize = 512
	}
	if chunkOverlap <= 0 || chunkOverlap >= chunkSize {
		chunkOverlap = 64
	}
	kb := &store.RAGKBRecord{
		ID: "kb_" + uuid.NewString()[:12], UserID: userID,
		Name: name, Description: description,
		EmbedProvider: src, EmbedModel: ec.Model, EmbedDims: ec.Dims,
		ChunkSize: chunkSize, ChunkOverlap: chunkOverlap, Status: "active",
	}
	if err := s.st.CreateRAGKB(ctx, kb); err != nil {
		return nil, err
	}
	if err := s.vec.EnsureCollection(ctx, kb.ID, kb.EmbedDims); err != nil {
		_ = s.st.DeleteRAGKB(ctx, kb.ID) // 回滚主库行，保持两边一致
		return nil, fmt.Errorf("创建向量 collection: %w", err)
	}
	return kb, nil
}

// GetKB 带归属校验；ownerID 为空表示跳过校验（内部/管理员路径显式传空）。
func (s *Service) GetKB(ctx context.Context, ownerID, kbID string) (*store.RAGKBRecord, error) {
	kb, err := s.st.GetRAGKB(ctx, kbID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ownerID != "" && kb.UserID != ownerID {
		return nil, ErrForbidden
	}
	return kb, nil
}

func (s *Service) ListKBs(ctx context.Context, userID string) ([]store.RAGKBRecord, error) {
	return s.st.ListRAGKBsByUser(ctx, userID)
}

func (s *Service) UpdateKB(ctx context.Context, ownerID, kbID, name, description string, chunkSize, chunkOverlap int) (*store.RAGKBRecord, error) {
	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return nil, err
	}
	if name != "" {
		kb.Name = name
	}
	kb.Description = description
	if chunkSize > 0 {
		kb.ChunkSize = chunkSize
	}
	if chunkOverlap > 0 && chunkOverlap < kb.ChunkSize {
		kb.ChunkOverlap = chunkOverlap
	}
	if err := s.st.UpdateRAGKB(ctx, kb); err != nil {
		return nil, err
	}
	return kb, nil
}

// DeleteKB：先置 deleting 挡新任务，再清向量库、对象存储、主库行。
// 在途任务看到 status=deleting 会自行放弃（pipeline.go 的 doIndex 开头检查）。
func (s *Service) DeleteKB(ctx context.Context, ownerID, kbID string) error {
	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return err
	}
	kb.Status = "deleting"
	if err := s.st.UpdateRAGKB(ctx, kb); err != nil {
		return err
	}
	if err := s.vec.DropCollection(ctx, kbID); err != nil {
		slog.Warn("rag: drop collection 失败,继续删除", "kb", kbID, "err", err)
	}
	if err := s.obj.DeletePrefix(ctx, fmt.Sprintf("rag/%s/%s/", kb.UserID, kbID)); err != nil {
		slog.Warn("rag: 删除对象前缀失败,继续删除", "kb", kbID, "err", err)
	}
	return s.st.DeleteRAGKB(ctx, kbID)
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/rag/ -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/service.go internal/rag/service_test.go internal/rag/vector/fake.go
git commit -m "feat(rag): Service 门面——KB CRUD、配额、embedding 快照、级联删除"
```

---

### Task 11: rag.Service —— 入库流水线

**Files:**
- Create: `internal/rag/pipeline.go`, `internal/rag/pipeline_test.go`
- Modify: `internal/rag/service_test.go`（newTestService 增加可注入的 embed 端点）

- [ ] **Step 1: 写失败测试**（关键断言：先算后删再写顺序、重试、崩溃恢复、deleting 放弃）

```go
package rag

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeEmbedServer 起 httptest 服务返回固定维度向量（同 embed 包测试的写法），
// newTestService 的 Embedding.Endpoint 指向它。

func waitDocStatus(t *testing.T, svc *Service, docID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		doc, err := svc.st.GetRAGDocument(context.Background(), docID)
		if err == nil && doc.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("doc %s 未达到状态 %s", docID, want)
}

func TestUploadIndexesDocumentEndToEnd(t *testing.T) {
	svc, fake := newTestService(t) // 内部已 Start(ctx)
	ctx := context.Background()
	kb, _ := svc.CreateKB(ctx, "u1", "手册", "", 0, 0)
	doc, err := svc.UploadDocument(ctx, "u1", kb.ID, "guide.md",
		strings.NewReader("# 安装\n\n先下载安装包。\n\n# 使用\n\n双击运行。"), 64)
	if err != nil {
		t.Fatal(err)
	}
	waitDocStatus(t, svc, doc.ID, "DONE")
	got, _ := svc.st.GetRAGDocument(ctx, doc.ID)
	if got.ChunkCount == 0 || got.TokenCount == 0 || got.IndexedAt == nil {
		t.Fatalf("统计未回写: %+v", got)
	}
	if n := fake.Count(kb.ID); n != got.ChunkCount {
		t.Fatalf("向量库 chunk 数 %d != 文档统计 %d", n, got.ChunkCount)
	}
}

func TestReindexDeletesOldVersionOnlyAfterNewWrite(t *testing.T) {
	svc, fake := newTestService(t)
	ctx := context.Background()
	kb, _ := svc.CreateKB(ctx, "u1", "手册", "", 0, 0)
	doc, _ := svc.UploadDocument(ctx, "u1", kb.ID, "a.md", strings.NewReader("# A\n\n内容一。"), 32)
	waitDocStatus(t, svc, doc.ID, "DONE")
	if err := svc.ReindexDocument(ctx, "u1", kb.ID, doc.ID); err != nil {
		t.Fatal(err)
	}
	waitDocStatus(t, svc, doc.ID, "DONE")
	got, _ := svc.st.GetRAGDocument(ctx, doc.ID)
	if got.Version != 2 {
		t.Fatalf("version 应递增到 2: %+v", got)
	}
	// fake 记录操作序列：必须是 Upsert(v2) 先于 DeleteOldVersions
	ops := fake.Ops(kb.ID)
	upsertIdx, deleteIdx := -1, -1
	for i, op := range ops {
		if op == "upsert_v2" && upsertIdx == -1 {
			upsertIdx = i
		}
		if op == "delete_old_v2" {
			deleteIdx = i
		}
	}
	if upsertIdx == -1 || deleteIdx == -1 || upsertIdx > deleteIdx {
		t.Fatalf("先算后删再写顺序被破坏: %v", ops)
	}
}

func TestUploadRejectsOversizeAndBadExt(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	kb, _ := svc.CreateKB(ctx, "u1", "手册", "", 0, 0)
	if _, err := svc.UploadDocument(ctx, "u1", kb.ID, "x.exe", strings.NewReader("x"), 1); err == nil {
		t.Fatal("不支持的扩展名应拒绝")
	}
	svc.cfg.Limits.MaxFileMB = 0 // 构造 0MB 上限便于断言
	if _, err := svc.UploadDocument(ctx, "u1", kb.ID, "x.md", strings.NewReader("x"), 1); err == nil {
		t.Fatal("超大小应拒绝")
	}
}

func TestFailedEmbedRetriesThenFails(t *testing.T) {
	// newTestService 的 embed 假服务端支持设置"前 N 次返回 500"；
	// 设为永远 500 → 任务重试 3 次后 FAILED，doc.ErrorMsg 非空。
	svc, _ := newTestServiceWithFailingEmbed(t)
	ctx := context.Background()
	kb, _ := svc.CreateKB(ctx, "u1", "手册", "", 0, 0)
	doc, _ := svc.UploadDocument(ctx, "u1", kb.ID, "a.md", strings.NewReader("内容"), 8)
	waitDocStatus(t, svc, doc.ID, "FAILED")
	got, _ := svc.st.GetRAGDocument(ctx, doc.ID)
	if got.ErrorMsg == "" {
		t.Fatal("失败原因应回写")
	}
}

func TestRecoverPendingTasksOnStart(t *testing.T) {
	// 直接向 store 写一条 PENDING 任务与 doc 行(模拟崩溃残留)，
	// New + Start 后应被执行到 DONE。
	svc, _ := newTestService(t) // 该构造器内部 Start 前先注入残留行,详见实现
	_ = svc
	// 具体写法：newTestService 拆出 newTestServiceNoStart，本测试
	// 用 NoStart 版本先插 doc(PENDING)+task(PENDING)+原件到 objects，
	// 再 svc.Start(ctx)，waitDocStatus DONE。
}
```

（fake 需再补两个测试辅助：`Count(kbID) int` 与 `Ops(kbID) []string`——fake 在 Upsert/DeleteOldVersions 时追加 `"upsert_v<version>"`/`"delete_old_v<keep>"` 记录。）

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/rag/ -run 'TestUpload|TestReindex|TestFailed|TestRecover' -v
```

- [ ] **Step 3: 实现**（pipeline.go）

```go
package rag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/split"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

// Start 启动索引 worker 并把主库里残留的可运行任务重新入队（崩溃恢复）。
// ctx 取消时 worker 退出。多 pod 部署下同一任务可能被两个 pod 执行——
// Upsert 以 (doc,version,index) 为 PK 幂等，重复执行无害。
func (s *Service) Start(ctx context.Context) {
	for i := 0; i < cap(s.workersSem()); i++ { // 见下：用固定 worker 数
	}
	workers := s.workerCount
	if workers <= 0 {
		workers = 2
	}
	for i := 0; i < workers; i++ {
		go s.worker(ctx)
	}
	go func() {
		tasks, err := s.st.ListRunnableRAGIndexTasks(ctx)
		if err != nil {
			slog.Error("rag: 恢复任务失败", "err", err)
			return
		}
		for _, t := range tasks {
			select {
			case s.tasks <- t.ID:
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.tasks:
			s.runTask(ctx, id)
		}
	}
}

// UploadDocument：校验 → 原件入对象存储 → doc 行(PENDING) + task 行 → 入队。
func (s *Service) UploadDocument(ctx context.Context, ownerID, kbID, fileName string, r io.Reader, size int64) (*store.RAGDocumentRecord, error) {
	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return nil, err
	}
	if kb.Status != "active" {
		return nil, errors.New("知识库正在删除中")
	}
	if !parse.SupportedExt(fileName) {
		return nil, fmt.Errorf("不支持的文件类型(支持 md/txt/pdf/docx)")
	}
	if size > int64(s.cfg.Limits.MaxFileMB)*1024*1024 {
		return nil, fmt.Errorf("%w: 单文件上限 %dMB", ErrQuota, s.cfg.Limits.MaxFileMB)
	}
	docs, err := s.st.ListRAGDocumentsByKB(ctx, kbID)
	if err != nil {
		return nil, err
	}
	if len(docs) >= s.cfg.Limits.MaxDocsPerKB {
		return nil, fmt.Errorf("%w: 每知识库最多 %d 篇文档", ErrQuota, s.cfg.Limits.MaxDocsPerKB)
	}
	docID := "doc_" + uuid.NewString()[:12]
	key := objects.Key(kb.UserID, kbID, docID, fileName)
	if err := s.obj.Put(ctx, key, r, size, ""); err != nil {
		return nil, fmt.Errorf("保存原件: %w", err)
	}
	doc := &store.RAGDocumentRecord{
		ID: docID, KBID: kbID, FileName: fileName,
		FileType: strings.TrimPrefix(strings.ToLower(fileNameExt(fileName)), "."),
		FileSize: size, ObjectKey: key, Status: "PENDING", Version: 1,
		UploadedAt: time.Now().UTC(),
	}
	if err := s.st.CreateRAGDocument(ctx, doc); err != nil {
		return nil, err
	}
	return doc, s.enqueue(ctx, docID)
}

// ReindexDocument：version+1 置 PENDING 重新入队（换分块参数/修复失败用）。
func (s *Service) ReindexDocument(ctx context.Context, ownerID, kbID, docID string) error {
	if _, err := s.GetKB(ctx, ownerID, kbID); err != nil {
		return err
	}
	doc, err := s.st.GetRAGDocument(ctx, docID)
	if err != nil || doc.KBID != kbID {
		return ErrNotFound
	}
	doc.Version++
	doc.Status = "PENDING"
	doc.ErrorMsg = ""
	if err := s.st.UpdateRAGDocument(ctx, doc); err != nil {
		return err
	}
	return s.enqueue(ctx, docID)
}

// DeleteDocument：删向量 → 删原件 → 删行。
func (s *Service) DeleteDocument(ctx context.Context, ownerID, kbID, docID string) error {
	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return err
	}
	doc, err := s.st.GetRAGDocument(ctx, docID)
	if err != nil || doc.KBID != kbID {
		return ErrNotFound
	}
	if err := s.vec.DeleteDoc(ctx, kbID, docID); err != nil {
		slog.Warn("rag: 删向量失败,继续", "doc", docID, "err", err)
	}
	if err := s.obj.DeletePrefix(ctx, fmt.Sprintf("rag/%s/%s/%s/", kb.UserID, kbID, docID)); err != nil {
		slog.Warn("rag: 删原件失败,继续", "doc", docID, "err", err)
	}
	return s.st.DeleteRAGDocument(ctx, docID)
}

func (s *Service) enqueue(ctx context.Context, docID string) error {
	taskID, err := s.st.CreateRAGIndexTask(ctx, docID, 3)
	if err != nil {
		return err
	}
	select {
	case s.tasks <- taskID:
	default:
		// 队列满：任务已持久化，靠下次重启恢复；不阻塞上传请求。
		slog.Warn("rag: 任务队列已满,任务延后执行", "task", taskID)
	}
	return nil
}

// runTask 执行单个索引任务：doIndex 失败按指数退避重试(1s/2s/4s)。
func (s *Service) runTask(ctx context.Context, taskID int64) {
	// 读 task 行拿 docID 与 retryCount（store 无 GetRAGIndexTask 则在
	// Task 2 补上——测试同步补断言）。
	task, err := s.getTask(ctx, taskID)
	if err != nil {
		slog.Error("rag: 读任务失败", "task", taskID, "err", err)
		return
	}
	_ = s.st.UpdateRAGIndexTask(ctx, taskID, "RUNNING", task.RetryCount, "")
	err = s.doIndex(ctx, task.DocID)
	if err == nil {
		_ = s.st.UpdateRAGIndexTask(ctx, taskID, "DONE", task.RetryCount, "")
		return
	}
	slog.Error("rag: 索引失败", "doc", task.DocID, "retry", task.RetryCount, "err", err)
	if task.RetryCount+1 <= task.MaxRetry {
		_ = s.st.UpdateRAGIndexTask(ctx, taskID, "PENDING", task.RetryCount+1, err.Error())
		go func() {
			delay := time.Duration(1<<task.RetryCount) * time.Second
			select {
			case <-time.After(delay):
				select {
				case s.tasks <- taskID:
				case <-ctx.Done():
				}
			case <-ctx.Done():
			}
		}()
		return
	}
	_ = s.st.UpdateRAGIndexTask(ctx, taskID, "FAILED", task.RetryCount, err.Error())
	if doc, derr := s.st.GetRAGDocument(ctx, task.DocID); derr == nil {
		doc.Status = "FAILED"
		doc.ErrorMsg = err.Error()
		_ = s.st.UpdateRAGDocument(ctx, doc)
	}
}

// doIndex：取原件 → 解析 → 分块 → embedding → 写新版本 → 删旧版本 → 回写统计。
// "先算后删再写"：Upsert 成功前绝不 DeleteOldVersions，索引期间旧数据可查。
func (s *Service) doIndex(ctx context.Context, docID string) error {
	doc, err := s.st.GetRAGDocument(ctx, docID)
	if err != nil {
		return err
	}
	kb, err := s.st.GetRAGKB(ctx, doc.KBID)
	if err != nil {
		return err
	}
	if kb.Status != "active" {
		return nil // 库在删除中，放弃即可
	}
	doc.Status = "PROCESSING"
	_ = s.st.UpdateRAGDocument(ctx, doc)

	rc, err := s.obj.Get(ctx, doc.ObjectKey)
	if err != nil {
		return fmt.Errorf("读原件: %w", err)
	}
	parsed, perr := parse.Parse(rc, doc.FileName)
	rc.Close()
	if perr != nil {
		return perr
	}

	cfg := split.Config{ChunkSize: kb.ChunkSize, ChunkOverlap: kb.ChunkOverlap}
	var chunks []split.Chunk
	switch parsed.Format {
	case "md", "docx": // docx 已归一化为 markdown 标题
		chunks = split.Markdown(parsed.Pages[0].Text, cfg)
	case "pdf":
		pages := make([]string, len(parsed.Pages))
		for i, p := range parsed.Pages {
			pages[i] = p.Text
		}
		chunks = split.Pages(pages, cfg)
	default:
		chunks = split.SlidingWindow(parsed.Pages[0].Text, cfg, "", 0)
	}
	if len(chunks) == 0 {
		return errors.New("分块结果为空")
	}

	texts := make([]string, len(chunks))
	totalTokens := 0
	for i, c := range chunks {
		texts[i] = c.Content
		totalTokens += c.Tokens
	}
	vecs, err := s.embedderForKB(ctx, kb).Embed(ctx, texts)
	if err != nil {
		return err
	}

	data := make([]vector.ChunkData, len(chunks))
	for i, c := range chunks {
		data[i] = vector.ChunkData{
			DocID: doc.ID, Index: c.Index, Content: c.Content,
			SectionTitle: c.SectionTitle, PageNum: c.PageNum,
			DocVersion: doc.Version, Vector: vecs[i],
		}
	}
	if err := s.vec.UpsertChunks(ctx, kb.ID, data); err != nil {
		return fmt.Errorf("写入向量库: %w", err)
	}
	if err := s.vec.DeleteOldVersions(ctx, kb.ID, doc.ID, doc.Version); err != nil {
		slog.Warn("rag: 删旧版本失败(下次重建时清理)", "doc", doc.ID, "err", err)
	}

	doc.Status = "DONE"
	doc.ChunkCount = len(chunks)
	doc.TokenCount = totalTokens
	now := time.Now().UTC()
	doc.IndexedAt = &now
	return s.st.UpdateRAGDocument(ctx, doc)
}
```

（`getTask` 需要 store 增加 `GetRAGIndexTask(ctx, id)`——回到 Task 2 的文件补方法与接口声明，并在 rag_test.go 补一行断言；`fileNameExt` 用 `path/filepath.Ext`。`workersSem` 笔误不实现——`Service` 直接存 `workerCount int`，`New` 里赋值。）

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/rag/... -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/ internal/store/
git commit -m "feat(rag): 入库流水线——异步任务+先算后删再写+指数退避+崩溃恢复"
```

---

### Task 12: rag.Service —— 检索与工具文本

**Files:**
- Create: `internal/rag/search.go`, `internal/rag/search_test.go`

- [ ] **Step 1: 写失败测试**

```go
package rag

import (
	"context"
	"strings"
	"testing"
)

func TestSearchAcrossKBsMergesAndFormats(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	kb1, _ := svc.CreateKB(ctx, "u1", "产品手册", "", 0, 0)
	kb2, _ := svc.CreateKB(ctx, "u1", "运维文档", "", 0, 0)
	d1, _ := svc.UploadDocument(ctx, "u1", kb1.ID, "guide.md", strings.NewReader("# 安装\n\n安装需要管理员权限。"), 64)
	d2, _ := svc.UploadDocument(ctx, "u1", kb2.ID, "ops.md", strings.NewReader("# 部署\n\n部署走蓝绿发布。"), 64)
	waitDocStatus(t, svc, d1.ID, "DONE")
	waitDocStatus(t, svc, d2.ID, "DONE")

	res, err := svc.Search(ctx, "u1", []string{kb1.ID, kb2.ID}, "安装权限", 5)
	if err != nil || len(res) == 0 {
		t.Fatalf("search: %v err=%v", res, err)
	}
	if res[0].KBID != kb1.ID || res[0].DocName != "guide.md" {
		t.Fatalf("top1 应来自 kb1/guide.md: %+v", res[0])
	}
	text := FormatHits(res)
	if !strings.Contains(text, "[来源: guide.md") || !strings.Contains(text, "安装") {
		t.Fatalf("工具文本缺引用头: %q", text)
	}
}

func TestSearchOwnershipAndEmptyResult(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	kb, _ := svc.CreateKB(ctx, "u1", "a", "", 0, 0)
	if _, err := svc.Search(ctx, "u2", []string{kb.ID}, "q", 5); err == nil {
		t.Fatal("跨用户 KB 检索必须拒绝")
	}
	res, err := svc.Search(ctx, "u1", []string{kb.ID}, "毫无关联的查询", 5)
	if err != nil {
		t.Fatal(err)
	}
	if FormatHits(res) == "" {
		t.Fatal("空结果也要有明确文案")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/rag/ -run TestSearch -v
```

- [ ] **Step 3: 实现**（search.go）

```go
package rag

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type Hit struct {
	KBID         string
	KBName       string
	DocID        string
	DocName      string
	ChunkIndex   int
	SectionTitle string
	PageNum      int
	Content      string
	Score        float64
}

// Search 在 ownerID 名下的多个 KB 里混合检索并按分数归并取 topN。
// 每个 kbID 都做归属校验；查询向量按 KB 快照模型计算，同模型只算一次。
func (s *Service) Search(ctx context.Context, ownerID string, kbIDs []string, query string, topN int) ([]Hit, error) {
	if topN <= 0 {
		topN = 5
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query 不能为空")
	}
	type kbe struct {
		kb  *store.RAGKBRecord
		vec []float32
	}
	var targets []kbe
	vecCache := map[string][]float32{} // model → 查询向量
	for _, id := range kbIDs {
		kb, err := s.GetKB(ctx, ownerID, id)
		if err != nil {
			return nil, err
		}
		if kb.Status != "active" {
			continue
		}
		qv, ok := vecCache[kb.EmbedModel]
		if !ok {
			vs, err := s.embedderForKB(ctx, kb).Embed(ctx, []string{query})
			if err != nil {
				return nil, fmt.Errorf("查询向量化(%s): %w", kb.EmbedModel, err)
			}
			qv = vs[0]
			vecCache[kb.EmbedModel] = qv
		}
		targets = append(targets, kbe{kb: kb, vec: qv})
	}
	// 文档名映射（引用头用）
	var out []Hit
	for _, t := range targets {
		hits, err := s.vec.HybridSearch(ctx, t.kb.ID, t.vec, query, topN)
		if err != nil {
			return nil, fmt.Errorf("检索 %s: %w", t.kb.Name, err)
		}
		docs, err := s.st.ListRAGDocumentsByKB(ctx, t.kb.ID)
		if err != nil {
			return nil, err
		}
		names := map[string]string{}
		for _, d := range docs {
			names[d.ID] = d.FileName
		}
		for _, h := range hits {
			out = append(out, Hit{
				KBID: t.kb.ID, KBName: t.kb.Name,
				DocID: h.DocID, DocName: names[h.DocID],
				ChunkIndex: h.ChunkIndex, SectionTitle: h.SectionTitle,
				PageNum: h.PageNum, Content: h.Content, Score: h.Score,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > topN {
		out = out[:topN]
	}
	return out, nil
}

// FormatHits 渲染为 rag_search 的工具返回文本。
func FormatHits(hits []Hit) string {
	if len(hits) == 0 {
		return "未在授权的知识库中检索到相关内容。"
	}
	var sb strings.Builder
	for i, h := range hits {
		sb.WriteString(fmt.Sprintf("[来源: %s", h.DocName))
		if h.SectionTitle != "" {
			sb.WriteString(" · " + h.SectionTitle)
		}
		if h.PageNum > 0 {
			sb.WriteString(fmt.Sprintf(" · 第%d页", h.PageNum))
		}
		sb.WriteString(fmt.Sprintf(" · chunk#%d · 知识库:%s]\n", h.ChunkIndex, h.KBName))
		sb.WriteString(strings.TrimSpace(h.Content))
		if i < len(hits)-1 {
			sb.WriteString("\n\n---\n\n")
		}
	}
	return sb.String()
}
```

（import 里补 `"github.com/qs3c/bkcrab/internal/store"`。）

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/rag/ -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/search.go internal/rag/search_test.go
git commit -m "feat(rag): 跨 KB 混合检索归并与工具文本渲染(带来源引用)"
```

---

### Task 13: rag_search 工具

**Files:**
- Create: `internal/agent/tools/rag_search.go`, `internal/agent/tools/rag_search_test.go`

- [ ] **Step 1: 写失败测试**

```go
package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakeRAGSearcher struct{ lastKBs []string }

func (f *fakeRAGSearcher) SearchForAgent(ctx context.Context, ownerID string, kbIDs []string, query string, topN int) (string, error) {
	f.lastKBs = kbIDs
	return "[来源: a.md · chunk#0 · 知识库:手册]\n内容", nil
}

func TestRegisterRAGSearchSkipsWhenNoKBs(t *testing.T) {
	r := newTestRegistry(t) // 本包已有测试构造 registry 的辅助；以现有测试文件为准
	RegisterRAGSearch(r, &fakeRAGSearcher{}, "u1", nil, 0)
	if r.Has("rag_search") { // Registry 若无 Has，用现有测试里探测工具存在性的方式
		t.Fatal("无授权 KB 不应注册工具")
	}
}

func TestRAGSearchToolListsKBsAndSearches(t *testing.T) {
	r := newTestRegistry(t)
	f := &fakeRAGSearcher{}
	kbs := []RAGKBRef{{ID: "kb_1", Name: "产品手册", Description: "产品相关"}}
	RegisterRAGSearch(r, f, "u1", kbs, 0)
	def := r.ToolDef("rag_search") // 同上，按现有测试读取工具描述的方式
	if !strings.Contains(def.Description, "产品手册") {
		t.Fatalf("描述应列出 KB: %s", def.Description)
	}
	out, err := r.Execute(context.Background(), "rag_search", json.RawMessage(`{"query":"如何安装"}`))
	if err != nil || !strings.Contains(out, "[来源:") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(f.lastKBs) != 1 || f.lastKBs[0] != "kb_1" {
		t.Fatalf("默认应查全部授权 KB: %v", f.lastKBs)
	}
	// kb 过滤：按名字选择子集
	_, _ = r.Execute(context.Background(), "rag_search", json.RawMessage(`{"query":"x","kb":"产品手册"}`))
	if len(f.lastKBs) != 1 {
		t.Fatalf("kb 过滤失效: %v", f.lastKBs)
	}
	_, err = r.Execute(context.Background(), "rag_search", json.RawMessage(`{"query":"x","kb":"不存在"}`))
	if err == nil {
		t.Fatal("未知 kb 名应报错并列出可用名单")
	}
}
```

（`newTestRegistry`/`Has`/`ToolDef`/`Execute` 以本包既有测试的实际辅助为准——`registry_forturn_test.go` 里有现成模式，照抄。）

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/agent/tools/ -run TestRAG -v
```

- [ ] **Step 3: 实现**（rag_search.go）

```go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RAGSearcher 是 rag.Service 面向工具层的窄接口，返回已渲染的工具文本。
// 由 rag 包提供适配方法（见 Task 14 的 SearchForAgent），此处不 import rag
// 以避免 tools → rag → store 的依赖扇出。
type RAGSearcher interface {
	SearchForAgent(ctx context.Context, ownerID string, kbIDs []string, query string, topN int) (string, error)
}

// RAGKBRef 是授权给该 agent 的一个 KB 的展示信息（装配时已校验归属）。
type RAGKBRef struct {
	ID          string
	Name        string
	Description string
}

// RegisterRAGSearch 注册 rag_search 只读检索工具。
// kbs 为空时不注册——LLM 不应看到用不了的工具（与 web_search 链同规则）。
func RegisterRAGSearch(r *Registry, svc RAGSearcher, ownerID string, kbs []RAGKBRef, defaultTopN int) {
	if svc == nil || len(kbs) == 0 {
		return
	}
	if defaultTopN <= 0 {
		defaultTopN = 5
	}
	var kbLines []string
	byName := map[string]string{}
	allIDs := make([]string, 0, len(kbs))
	for _, kb := range kbs {
		desc := kb.Name
		if kb.Description != "" {
			desc += ": " + kb.Description
		}
		kbLines = append(kbLines, desc)
		byName[kb.Name] = kb.ID
		allIDs = append(allIDs, kb.ID)
	}
	description := "Search the user's knowledge bases and return the most relevant passages with source citations. " +
		"Read-only. Available knowledge bases: " + strings.Join(kbLines, "; ") + ". " +
		"Use when the question may be answered by these documents."
	r.Register("rag_search", description, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "自然语言检索词。用与文档同语言的关键词效果最好。",
			},
			"kb": map[string]any{
				"type":        "string",
				"description": "可选。只查这个名字的知识库；缺省查全部已授权知识库。",
			},
			"top_n": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("返回条数,默认 %d,最大 20。", defaultTopN),
			},
		},
		"required": []string{"query"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Query string `json:"query"`
			KB    string `json:"kb"`
			TopN  int    `json:"top_n"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		ids := allIDs
		if args.KB != "" {
			id, ok := byName[args.KB]
			if !ok {
				names := make([]string, 0, len(byName))
				for n := range byName {
					names = append(names, n)
				}
				return "", fmt.Errorf("知识库 %q 不存在或未授权,可用: %s", args.KB, strings.Join(names, ", "))
			}
			ids = []string{id}
		}
		topN := args.TopN
		if topN <= 0 {
			topN = defaultTopN
		}
		if topN > 20 {
			topN = 20
		}
		return svc.SearchForAgent(ctx, ownerID, ids, args.Query, topN)
	})
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/agent/tools/ -run TestRAG -v
go test ./internal/agent/tools/ -run TestForTurn -v   # 回归:ForTurn 隔离不受新工具影响
```

- [ ] **Step 5: Commit**

```bash
git add internal/agent/tools/rag_search.go internal/agent/tools/rag_search_test.go
git commit -m "feat(tools): rag_search 只读检索工具(条件注册,描述列授权 KB)"
```

---

### Task 14: manager / gateway 装配

**Files:**
- Modify: `internal/agent/manager.go`（ManagerOption 区 + `buildAgent`）
- Modify: `internal/gateway/gateway.go`（引导构造 rag.Service）、`internal/gateway/userspace.go:677`（managerOpts）
- Create: `internal/rag/agent_adapter.go`（SearchForAgent / ResolveAgentKBs）

- [ ] **Step 1: 实现 rag 侧适配**（agent_adapter.go，先写小单测再实现）

```go
package rag

import (
	"context"
	"log/slog"

	"github.com/qs3c/bkcrab/internal/agent/tools"
)

// SearchForAgent 满足 tools.RAGSearcher：检索并渲染工具文本。
func (s *Service) SearchForAgent(ctx context.Context, ownerID string, kbIDs []string, query string, topN int) (string, error) {
	hits, err := s.Search(ctx, ownerID, kbIDs, query, topN)
	if err != nil {
		return "", err
	}
	return FormatHits(hits), nil
}

// ResolveAgentKBs 把 agent 配置里的 kb id 列表解析成展示引用：
// 校验归属(kb.UserID == ownerID)，悬空/越权引用剔除并打日志。
func (s *Service) ResolveAgentKBs(ctx context.Context, ownerID string, kbIDs []string) []tools.RAGKBRef {
	var out []tools.RAGKBRef
	for _, id := range kbIDs {
		kb, err := s.GetKB(ctx, ownerID, id)
		if err != nil {
			slog.Warn("rag: agent 配置引用的 KB 不可用,已剔除", "kb", id, "owner", ownerID, "err", err)
			continue
		}
		if kb.Status != "active" {
			continue
		}
		out = append(out, tools.RAGKBRef{ID: kb.ID, Name: kb.Name, Description: kb.Description})
	}
	return out
}
```

单测（service_test.go 追加）：越权 id 被剔除、正常 id 保留、`SearchForAgent` 空结果返回文案而非错误。

> 依赖方向检查：`rag` import `agent/tools` 只为 `RAGKBRef` 类型。若 `agent/tools` 已经（直接或间接）import `rag` 导致环，把 `RAGKBRef`/`RAGSearcher` 声明留在 tools（如 Task 13 所写），rag 侧仅实现方法——Go 接口隐式满足，无需双向 import。`ResolveAgentKBs` 返回 `[]tools.RAGKBRef` 若成环，改为在 manager.go 做类型转换（rag 返回自己的 struct，manager 转成 tools.RAGKBRef）。

- [ ] **Step 2: manager 装配**（manager.go）

ManagerOption 区（:87 `WithMeter` 附近）追加：

```go
// WithRAGService 注入平台级 RAG 服务。非 nil 且 agent 配置了 rag.kbs 时,
// buildAgent 会为该 agent 注册 rag_search 工具。
func WithRAGService(svc RAGService) ManagerOption {
	return func(o *managerOpts) { o.ragService = svc }
}

// RAGService 是 manager 需要的 rag.Service 子集(避免 agent → rag 强耦合)。
type RAGService interface {
	SearchForAgent(ctx context.Context, ownerID string, kbIDs []string, query string, topN int) (string, error)
	ResolveAgentKBs(ctx context.Context, ownerID string, kbIDs []string) []tools.RAGKBRef
}
```

`buildAgent`（dataStore 块之后，与 `tools.RegisterCronTools` 同级）追加：

```go
// rag_search:agent 配置了 KB 且平台 RAG 可用才注册;引用在此解析并校验归属。
if m.opts.ragService != nil && len(rc.RAG.KBs) > 0 {
	refs := m.opts.ragService.ResolveAgentKBs(context.Background(), rc.UserID, rc.RAG.KBs)
	tools.RegisterRAGSearch(ag.registry, m.opts.ragService, rc.UserID, refs, rc.RAG.TopN)
}
```

- [ ] **Step 3: gateway 装配**

`gateway.go`：在构造 store/workspace 的引导段（工厂调用附近）追加——系统配置 `cfg.RAG.Available()` 为真时：

```go
vecStore, err := vector.NewMilvus(ctx, cfg.RAG.Milvus.Address, cfg.RAG.Milvus.Username, cfg.RAG.Milvus.Password)
if err != nil {
	slog.Error("rag: milvus 连接失败,RAG 板块禁用", "err", err)
} else {
	objStore := ragObjectsFromConfig(cfg) // 有 S3 配置用 objects.NewS3(同 workspace 的 S3Config),否则 objects.NewLocalFS(<数据目录>/rag-objects)
	g.ragSvc = rag.New(rag.Deps{
		Store: st, Vector: vecStore, Objects: objStore, Cfg: cfg.RAG,
		UserEmbedCfg: userEmbedCfgLookup(st), // 从 configs 表 scope=user 读 "rag.embedding" 键;沿用现有 scope 读取辅助
	})
	g.ragSvc.Start(ctx)
}
```

`userspace.go:677` managerOpts 列表追加：

```go
agent.WithRAGService(g.ragSvc), // nil 安全:WithRAGService 存 nil 时 buildAgent 分支不触发
```

（注意 `WithRAGService` 接口值判 nil 的坑：g.ragSvc 为 `*rag.Service` nil 时接口非 nil——gateway 侧用 `if g.ragSvc != nil { opts = append(opts, agent.WithRAGService(g.ragSvc)) }` 规避。）

- [ ] **Step 4: 编译 + 全量回归**

```bash
go build ./...
go test ./internal/agent/... ./internal/gateway/... ./internal/rag/...
```

- [ ] **Step 5: Commit**

```bash
git add internal/agent/manager.go internal/gateway/ internal/rag/agent_adapter.go internal/rag/service_test.go
git commit -m "feat(agent): RAG 服务装配——WithRAGService + buildAgent 条件注册 rag_search"
```

---

### Task 15: HTTP API

**Files:**
- Create: `internal/setup/handlers_rag.go`, `internal/setup/handlers_rag_test.go`
- Modify: `internal/setup/server.go`（路由块）、`internal/setup/server.go` 的 Server 结构体（加 `rag *rag.Service` 字段与注入）

- [ ] **Step 1: 写失败测试**（照 `handlers_skills_test.go` 的构造模式：authTestRequest 辅助 + 归属校验断言）

```go
package setup

// 用例清单(每个都是独立 test func,构造方式照抄 handlers_skills_test.go):
// 1. TestRAGKBCRUDViaAPI: POST /api/rag/kbs 建库(u1) → GET 列表含它 →
//    PATCH 改名生效 → DELETE 后 GET 404。
// 2. TestRAGKBOwnership: u1 建库,u2 GET/DELETE 该库 → 403/404;
//    super_admin GET → 200。
// 3. TestRAGDocumentUpload: multipart 上传 a.md → 202 返回 doc 行(PENDING);
//    GET documents 列表含它;不支持扩展名 → 400;超 MaxFileMB → 413。
// 4. TestRAGSearchEndpoint: POST /api/rag/kbs/{id}/search {"query":"..."}
//    → 200 JSON hits 数组(用 fake vector 注入的 Server 构造)。
// 5. TestRAGDisabled: Server.rag == nil 时所有 /api/rag/* → 503 且给出
//    "RAG 未配置"的 message。
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/setup/ -run TestRAG -v
```

- [ ] **Step 3: 实现**（handlers_rag.go；错误映射统一走本包现有的 JSON 错误辅助）

```go
package setup

// 路由(server.go 路由块,agents 路由后追加):
//   mux.HandleFunc("GET /api/rag/kbs", auth(s.handleListRAGKBs))
//   mux.HandleFunc("POST /api/rag/kbs", auth(s.handleCreateRAGKB))
//   mux.HandleFunc("GET /api/rag/kbs/{id}", auth(s.handleGetRAGKB))
//   mux.HandleFunc("PATCH /api/rag/kbs/{id}", auth(s.handleUpdateRAGKB))
//   mux.HandleFunc("DELETE /api/rag/kbs/{id}", auth(s.handleDeleteRAGKB))
//   mux.HandleFunc("POST /api/rag/kbs/{id}/documents", auth(s.handleUploadRAGDocument))
//   mux.HandleFunc("GET /api/rag/kbs/{id}/documents", auth(s.handleListRAGDocuments))
//   mux.HandleFunc("DELETE /api/rag/kbs/{id}/documents/{docId}", auth(s.handleDeleteRAGDocument))
//   mux.HandleFunc("POST /api/rag/kbs/{id}/documents/{docId}/reindex", auth(s.handleReindexRAGDocument))
//   mux.HandleFunc("POST /api/rag/kbs/{id}/search", auth(s.handleRAGSearch))
//
// 公共前置:每个 handler 开头
//   if s.rag == nil { http 503 "RAG 未配置(需要 milvus 与 embedding 配置)" }
//   uid := 当前登录用户 id(照本包其它 handler 从 auth 身份取);
//   super_admin 越权:ownerID 传 "" 让 Service 跳过归属校验,
//   普通用户传 uid。
//
// handleUploadRAGDocument:
//   r.ParseMultipartForm(int64(s.rag.MaxFileMB()+1) << 20) — Service 暴露只读配额;
//   file, hdr, err := r.FormFile("file"); Service.UploadDocument(uid, kbID,
//   hdr.Filename, file, hdr.Size);
//   错误映射: ErrQuota→413, ErrForbidden→403, ErrNotFound→404,
//   "不支持的文件类型"→400, 其余→500; 成功 202 + doc JSON。
//
// handleRAGSearch: body {"query": string, "topN": int} → Service.Search →
//   200 {"hits":[...]}(Hit 结构直接 json 序列化,字段加 json tag——回到
//   search.go 给 Hit 加 tag: kbId/kbName/docId/docName/chunkIndex/
//   sectionTitle/pageNum/content/score)。
//
// 全部 handler 按上述写全,风格(JSON 编码、错误辅助、路径参数 r.PathValue)
// 照抄 handlers_skills.go / handlers_agents.go。
```

（`Service` 补两个小方法：`MaxFileMB() int` 返回 `s.cfg.Limits.MaxFileMB`；Hit 加 json tag。Server 结构体加 `rag *rag.Service` 字段，gateway/daemon 构造 setup.Server 处把 `g.ragSvc` 传进来——找到现有 Server 构造点按同风格注入。）

- [ ] **Step 4: 跑测试确认通过 + 全量回归**

```bash
go test ./internal/setup/ -run TestRAG -v
go test ./...
```

- [ ] **Step 5: Commit**

```bash
git add internal/setup/ internal/rag/search.go internal/rag/service.go
git commit -m "feat(api): RAG 知识库/文档/检索 HTTP 端点(归属校验+配额+503 降级)"
```

---

### Task 16: 收尾——文档、验证、合并准备

**Files:**
- Modify: `docs/database.md`（三张新表的 schema 说明，照现有表的文档格式）

- [ ] **Step 1: 更新 docs/database.md**

在表清单中追加 `rag_kbs` / `rag_documents` / `rag_index_tasks` 三节：列名、类型、语义（embedding 快照只读、version 版本化删除、任务表崩溃恢复），格式照抄文中现有表的写法。

- [ ] **Step 2: 全量验证**

```bash
go vet ./...
go build ./...
go test ./...
```

预期：全部通过；输出粘贴到 PR/commit 描述。

- [ ] **Step 3: 手工冒烟（可选但推荐,需本地 Milvus + 任一 OpenAI 兼容 embedding key）**

```
1. bkcrab.json 配 rag.milvus.address 与 rag.embedding.{endpoint,apiKey,model,dims}
2. 启动 bkcrab,登录 → POST /api/rag/kbs 建库
3. 传一篇 md → 轮询 documents 到 DONE
4. POST /api/rag/kbs/{id}/search {"query":"文中的关键词"} → hits 非空
5. agent 配置 rag.kbs=[该库] → 聊天里问文档内容 → 模型调用 rag_search 并引用来源
```

- [ ] **Step 4: Commit**

```bash
git add docs/database.md
git commit -m "docs(database): RAG 三张新表 schema 说明"
```

- [ ] **Step 5: 完成后按 superpowers:finishing-a-development-branch 流程决定合并方式**

---

## 自审记录

- **规格覆盖**：三张表(Task 2)、配置三层(Task 3)、解析/分块/embedding/向量库/对象存储(Task 4-9)、KB 管理+级联删除+配额(Task 10)、流水线先算后删再写+重试+恢复(Task 11)、混合检索+RRF+引用渲染(Task 12)、rag_search 条件注册(Task 13)、装配(Task 14)、HTTP API+503 降级(Task 15)、数据库文档(Task 16)。规格中 Web UI 部分明确移出本计划（见"范围说明"），二期项（HyDE/Rerank/共享 ACL/rag_ingest）不在本计划。
- **占位符**：Task 2 的 documents/tasks CRUD、Task 7 fake、Task 8 Milvus、Task 15 handlers 用"语义清单+完整签名"而非逐行代码——语义均已在测试代码与注释块中完整固定，无 TBD/TODO 字样；Milvus SDK 符号差异已给出以 SDK 为准的调整原则与文档链接。
- **类型一致性**：`store.RAGKBRecord/RAGDocumentRecord/RAGIndexTaskRecord`、`vector.ChunkData/SearchHit/Store`、`rag.Hit/Deps/Service`、`tools.RAGKBRef/RAGSearcher` 在各 Task 间引用一致；Task 11 发现 `getTask` 需要 `GetRAGIndexTask`——已在文中标注回补 Task 2；`RegisterRAGSearch` 签名在 Task 13/14 一致（含 ownerID 参数）。
