# 异步记忆提取触发机制重构 — 设计规格

> 状态:已定稿,待实现
> 日期:2026-06-17
> 背景讨论:见 `memoryextractiondesigndiscussion.md`(10 轮问答的原始记录)
> 关联代码:`internal/agent/loop.go`、`internal/agent/memory.go`、`internal/store/database.go`、`internal/store/database_mysql.go`、`internal/session/`

---

## 1. 背景与问题

记忆提取(`AutoPersistMemory`,LLM 把对话蒸馏进 `MEMORY.md` / `USER.md`)目前在每个 turn 结束时按"用户消息计数"触发。现状代码:

- [`runPostTurn`](../../../internal/agent/loop.go) loop.go:2459–2482:调用 `CountChatterUserMessages(agent_id, chatter_user_id)` 统计该 chatter 在该 agent 下**所有 session** 的 `session_messages` 中 `role='user'` 的总行数 `n`,当 `n % EveryNTurns == 0`(默认 5)时 `go AutoPersistMemory(...)`,输入是**当前 turn 所在 session 的内存工作集 `messages`**。

在 turn 可并发(同一 `(agent, chatter)` 的不同 session 同时跑)的前提下,该机制有五个缺陷:

| # | 问题 | 成因 |
|---|------|------|
| 1 | **重复触发** | 两个并发 turn 都先插入 user 行、各自结束时都读到同一个命中倍数,提取被触发两次且输入重叠 |
| 2 | **漏触发** | 并发插入使计数从 4 跳到 6,倍数点没有任何 turn 在结束时观测到,本轮永久丢失 |
| 3 | **计数 ≠ turn** | steer 注入、群聊 `InjectGroupMessage`、`goal_context` 续跑都追加 `role='user'` 行,计数早与"完整 turn 数"脱钩(群聊旁听场景下偏差可达数量级) |
| 4 | **输入与计数口径不一致** | 计数跨 session,提取输入只取当前 session 最后若干条内存消息;其他 session 贡献了计数内容却从不被提取,同一 session 连续触发时窗口重叠 |
| 5 | **受压缩干扰** | 输入取自内存工作集,压缩后开头是 `[Conversation Summary]` 合成消息,提取的是摘要而非原文 |

## 2. 设计目标

1. 触发单位是**完整执行完毕的 turn**,每累计 **5 个未提取的已完成 turn** 触发一次,范围限定在同一 `(agent_id, chatter_user_id)`(MEMORY.md / USER.md 是 chatter 维度的),可跨 session。
2. **不要求连续性**(用户在「问题3」明确澄清):turn 1 还在跑而 turn 2–6 已完成时,允许先提取 2–6;turn 1 后续完成后,再随 7–10 一起进入下一批。因此**不需要 frontier / 全局序 / 僵尸 turn 清扫**。
3. **并发安全**:同一批 turn 恰好被认领一次(不重复、不丢失),不依赖任何内存状态(免疫重启、UserSpace 失效、多副本部署)。
4. 提取输入改为从 `session_messages` 归档表按 turn 精确读取,与内存工作集、压缩状态解耦。
5. 为未来「压缩前的 session 内提取」预留路径,且与全局节拍**天然去重**。
6. 三后端(MySQL / PostgreSQL / SQLite)全部正确;MySQL 是运行时默认库([factory.go:17](../../../internal/store/factory.go))。

### 非目标(YAGNI)

- 不引入 `agent_turns`、`memory_extractions` 等新表(见 §11 被否决备选)。
- 不实现批次级失败状态持久化与延迟重试;失败用补偿重置处理(§7)。
- 不在本次实现压缩前提取的具体逻辑,只保证列设计能无改动支撑它(§8)。

## 3. 数据模型变更

只在 `session_messages` 上加两列,**不新建表**:

```sql
-- 语义
turn_status   : '' = 非锚点(steer / 群聊注入 / goal_context / 历史存量行)
                'running' = 锚点,turn 进行中
                'done'    = 锚点,turn 已完成
extraction_id : NULL = 未被任何提取认领;非 NULL = 已被某次提取(uuid)认领
```

`turn_status` **不用布尔、不用 `isSteer`**,改用「白名单 + 默认值」语义(理由见 §9)。

### 三后端建表 / 迁移(需改三处)

1. **SQLite / PostgreSQL 新装**:`migrationSQL` 的 `CREATE TABLE session_messages` 增加两列(`database.go:1310` 附近)。
2. **MySQL 新装**:`mysqlMigrationSQL()` 的 `CREATE TABLE session_messages` 增加两列(`database_mysql.go:153`),用 `VARCHAR`/合适类型:
   ```sql
   turn_status   VARCHAR(16) NOT NULL DEFAULT '',
   extraction_id VARCHAR(64) NULL
   ```
3. **存量库迁移**:新增幂等 `ALTER TABLE session_messages ADD COLUMN ...`,照搬现有 `migrateSessionMessagesAddOrigin`(`database.go:252`)的 `tableHasColumn` 判定 + 按 `d.dialect` 选列类型的套路;在 `Migrate()` 流程里挂上。历史行默认 `turn_status=''`、`extraction_id=NULL`,**无需数据回填**。

### 索引(部分索引在 MySQL 上降级)

认领查询的过滤条件是 `agent_id=? AND chatter_user_id=? AND turn_status='done' AND extraction_id IS NULL`。索引按已有模式([database.go:201-213](../../../internal/store/database.go) 的 chatter 索引)处理:

```sql
-- SQLite / PostgreSQL:部分索引,只收录待提取行
CREATE INDEX IF NOT EXISTS idx_sm_pending
  ON session_messages (agent_id, chatter_user_id)
  WHERE turn_status = 'done' AND extraction_id IS NULL;

-- MySQL:不支持部分索引,降级为普通复合索引
CREATE INDEX IF NOT EXISTS idx_sm_pending
  ON session_messages (agent_id, chatter_user_id, turn_status);
```

通过 `execDDL`(`database_mysql.go:45`,已处理 MySQL 无 `IF NOT EXISTS` 的情况)执行。

## 4. Turn 生命周期挂接

### 4.1 Turn 起点:写锚点 + 拿回 seq

turn 起点在 [loop.go:1847](../../../internal/agent/loop.go)(`HandleMessage`)、[loop.go:2551](../../../internal/agent/loop.go)(`HandleMessageStream`),以及 1618 处的 `sess.Append(userMsg)`。这些位置的 user message 是**唯一的锚点**,需要:

1. 写入 `session_messages` 时带 `turn_status='running'`;
2. **拿回数据库分配的 seq**,在本次 turn 的局部变量里持有(turn = 一次函数调用的生命周期,放局部变量正合适)。

**实现方式**:复用 `AppendSessionEvent`(`database.go:2217`)已验证的跨后端模式 —— `BeginTx → SELECT COALESCE(MAX(seq),-1)+1 → INSERT(带 seq 与 turn_status='running') → Commit`,返回 seq。新增一个专用 store 方法(如 `AppendTurnAnchor(...) (seq int64, err error)`),**不改动遍布各处的 `Session.Append` 签名**——只在三个 turn 起点改调这个新方法。

> 普通(非锚点)的 `Session.Append` / `AppendSessionMessage` 保持单语句插入、`turn_status` 默认 `''`、不返回 seq,行为不变。

### 4.2 Turn 终点:翻成 done

在 `runPostTurn`(loop.go:2400)内,用起点持有的 `(session_key, seq)` 主键精确 UPDATE:

```sql
UPDATE session_messages SET turn_status='done'
WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?;
```

拿主键改而非"查 session 里 running 的那行",避免上次崩溃残留的僵尸 `running` 行被认错(§9 末尾)。崩溃 / provider 报错的 turn 自然停在 `running`,**不阻塞任何人**(无连续性约束),其内容残缺、不被计数也不被提取,合理丢弃。

## 5. 触发与原子认领

### 5.1 触发条件

每个 turn 完成(`runPostTurn`)时评估一次:

```
COUNT(turn_status='done' AND extraction_id IS NULL
      在 agent_id=? AND chatter_user_id=? 范围内) >= N      (N = EveryNTurns,默认 5)
```

无 frontier、无顺序约束。计数只在 turn 完成事件中增长,跨过 N 的那次必然触发;即便偶发错过(DB 抖动),计数停在 ≥N,下一个 turn 完成时补上。

### 5.2 原子认领事务(防重复触发的唯一支点)

「数够 N → 置位认领」必须在**一个写事务**内完成:

```
BEGIN (写事务)
  rows := SELECT 主键 FROM session_messages
          WHERE agent_id=? AND chatter_user_id=?
            AND turn_status='done' AND extraction_id IS NULL
          ORDER BY created_at, seq
          LIMIT 15                      -- 3N 封顶,防积压撑爆 prompt
  IF len(rows) < N  → ROLLBACK; 退出(静默)
  uuid := new()
  UPDATE session_messages SET extraction_id = uuid WHERE 主键 IN (rows)
COMMIT
→ 成功后才 go runExtraction(uuid, rows)
```

两个并发 `runPostTurn` 同时数到 N:第二个事务里这批行的 `extraction_id` 已非 NULL,数不够,静默退出。**重复触发在事务层根除**。

### 5.3 三后端事务实现

| 后端 | 加锁方式 |
|------|---------|
| PostgreSQL | `SELECT ... FOR UPDATE`(在事务内对候选行加行锁) |
| MySQL (InnoDB) | `SELECT ... FOR UPDATE` |
| SQLite (modernc) | `BeginTx` + UPDATE 取写锁;依赖现有 WAL + `busy_timeout(5000)`(factory.go:36-40)序列化写入。**实现注意**:deferred 事务里 SELECT→UPDATE 的读升级在高并发下可能 `SQLITE_BUSY`;若复现,给 SQLite DSN 加 `_txlock=immediate`(modernc 支持)使事务开局即取写锁。`AppendSessionEvent` 已在用 `BeginTx`,沿用同一约定。 |

按 `d.dialect` 分支(代码库已大量如此),封装成一个 store 方法,例如 `ClaimCadenceBatch(ctx, agentID, chatterUID string, n, cap int) (extractionID string, rows []TurnRef, err error)`。

## 6. 提取输入:从归档按 turn 回放

`AutoPersistMemory`(memory.go:272)改为接受**已认领的 turn 引用列表**而非内存 `messages`:

1. 每个 turn 的消息范围 = 同 session 内 `[锚点.seq, 下一个锚点.seq)`(无下一个锚点则到该 session 当前最大 seq)。session 内 turn 串行,该推导可靠,steer / 注入行自然落在所属区间内。
2. 按 `(session_key, seq BETWEEN ...)` 从 `session_messages` 读原文;过滤合成行(`origin != OriginUser` 视情况)、截断 `tool` 角色行(沿用现有每条 ~300 字符的截断)。
3. 按 session 分组拼 prompt(`### Session <title/key>` 分节),跨 session 批次对 LLM 仍可读。
4. `LoadMemory` / `SaveMemoryWithScan` 的增量追加逻辑不变。

这同时修掉问题 4 与 5:输入恰好等于被计数的那 N 个 turn,无重叠无遗漏,且完全不受工作集压缩影响(归档表 append-only,压缩从不触碰)。

## 7. 失败处理:extraction_id + 补偿重置

提取是异步 LLM 调用,会失败。采用**补偿重置**(用户已确认):

- 认领成功即写 `extraction_id = uuid`(行被标记为"已认领,提取中")。
- `runExtraction` 成功:行保持已认领,`extraction_id` 即审计记录(哪批 turn 被哪次提取消费)。
- `runExtraction` 失败(捕获到的错误):把这批行的 `extraction_id` **重置回 NULL**,它们回到待提取状态,下次任意 turn 完成触发时自动重新覆盖。
- **进程崩溃**(claim 后、提取完成前 daemon 挂掉):这批行停在 `extraction_id=uuid` 但永不完成 → 静默丢失一批。**可接受**——丢一批记忆是良性的(用户判断),而"写重"会污染 MEMORY.md。这是不引入 `memory_extractions` 表所放弃的能力(无批次状态、无崩溃后延迟重试),已知情接受。

降级:`dataStore == nil`(无持久化单机模式)整套机制关闭,与现状一致;锚点 / 认领写失败只记日志,**绝不阻塞聊天主路径**。

## 8. 压缩前提取(预留路径,本次不实现)

未来在 `CompactMessages`(调用点 [loop.go:1851](../../../internal/agent/loop.go) / [2554](../../../internal/agent/loop.go) / [slash.go:255](../../../internal/agent/slash.go))真正压缩前插入一次 session 内提取。同一套列即可支撑,无需改 schema:

```
BEGIN (写事务)
  rows := SELECT ... WHERE agent_id=? AND session_key=?
            AND turn_status='done' AND extraction_id IS NULL
          ORDER BY seq
  IF len(rows) == 0 → ROLLBACK; skip
  UPDATE ... SET extraction_id = uuid WHERE 主键 IN (rows)
COMMIT  → 异步提取(不阻塞压缩)
```

要点:**按 session 过滤、不看 frontier、不设 ≥N 门槛**(压缩即将销毁原文,哪怕 1 个未提取 turn 也值得抢救);被它消费的 turn `extraction_id` 已置位,全局节拍自然跳过 → 与节拍提取**零额外协调地去重**。群聊 session 含多 chatter 时按 `chatter_user_id` 分组,各写各自的 MEMORY.md。

## 9. 锚点识别:为什么是白名单而非 isSteer

`session_messages` 里 `role='user'` 的行有多种来源,只有真正开启 turn 的那条是锚点:

| 来源 | 是否锚点 | 入口 |
|------|---------|------|
| 用户正常发言 | ✅ | `HandleMessage` / `HandleMessageStream` 的 `sess.Append(userMsg)` |
| steer 插话 | ❌ | `PushSteerIfActive` |
| 群聊旁听注入(其他真人/bot 的话) | ❌ | `InjectGroupMessage`(loop.go:583,裸 `sess.Append`) |
| goal_context 续跑 | ❌ | 合成注入 |

`isSteer` 是黑名单,只标得住 steer,标不住群聊注入和 goal_context,且每新增一种注入路径就得补一个标记字段,漏一种就切错 turn 区间。改用白名单:`turn_status` 默认 `''`,**全代码库只有三个 turn 起点**显式写 `'running'`,其余路径什么都不做、默认就是非锚点。安全性靠默认值兜底,而非靠每个开发者记得打标记。

> turn 终点用主键 UPDATE(§4.2)而非"查 running 行":若上次崩溃在某 session 留下僵尸 `running`,按行查会命中两行、可能把僵尸行错标 `done` 混进提取;按主键改不存在认错行的可能。

## 10. 落地改动清单(供实现计划参考)

1. **`internal/store/database.go`**
   - `migrationSQL`:`session_messages` CREATE TABLE 加两列。
   - 新增 `migrateSessionMessagesAddTurnColumns`(幂等 ALTER,三后端列类型分支),挂入 `Migrate()`。
   - 索引 `idx_sm_pending`(SQLite/PG 部分索引、MySQL 普通索引)。
   - 新增 `AppendTurnAnchor(...) (seq int64, err error)`(仿 `AppendSessionEvent`)。
   - 新增 `FinishTurn(...)`(主键 UPDATE turn_status='done')。
   - 新增 `ClaimCadenceBatch(...)`(单事务认领,三后端锁分支)。
   - 新增 `ResetExtraction(uuid)`(失败补偿重置)与 `LoadTurnMessages(rows)`(按区间回放)。
   - 删除 `CountChatterUserMessages` 及其接口方法。
2. **`internal/store/database_mysql.go`**:`mysqlMigrationSQL()` 的 `session_messages` 加两列。
3. **`internal/store/store.go`**:`Store` 接口同步增删上述方法签名。
4. **`internal/session/store_adapter.go` / `manager.go`**:暴露 turn 起点写锚点的入口(供 loop 调用拿回 seq),普通 `Append` 不变。
5. **`internal/agent/loop.go`**:三个 turn 起点改调锚点写入并持有 seq;`runPostTurn` 把"计数取模门"换成"标记 done → `ClaimCadenceBatch` → 异步提取"。
6. **`internal/agent/memory.go`**:`AutoPersistMemory` 改为按 turn 引用从归档回放输入;`runExtraction` 失败时调 `ResetExtraction`。
7. **`internal/agent/compaction.go`**:本次不改;预留 §8 的接入点。

## 11. 被否决的备选

- **双表(`agent_turns` + `memory_extractions`)**:turn 提升为一等持久实体 + 批次台账。正确性最强,但用户在「问题2/3」选择了加列方案,工程量小得多;去掉连续性约束后双表的主要收益(全局序、frontier)也不再需要。
- **标量 checkpoint(`last_extracted_turn_seq`)+ CAS**:最简,但表达不了压缩提取在序列中"打洞"的已提取集合,与 §8 冲突。逐行 `extraction_id` 标记可以。
- **内存级互斥(per-chatter mutex / sync.Once)**:解决不了重启丢状态和多副本部署,放弃。
- **后台轮询触发**:不需要;计数只在 turn 完成时变化,事件驱动评估已完备。
- **`memory_extractions` 表(批次状态 + 延迟重试)**:用户接受"丢一批记忆是良性的",用补偿重置代替,放弃此表。列设计未来要加表也无需改动。

## 12. 测试策略

- **单元 / 集成(store 层)**:仿 `mysql_integration_test.go`、`session_messages_origin_test.go`,覆盖锚点写入返回 seq、`FinishTurn`、`ClaimCadenceBatch` 的"够 N 才认领 / 不够 ROLLBACK / 并发只认领一次"、补偿重置。三后端各跑一遍(MySQL 走集成测试 tag)。
- **并发回归**:模拟同一 `(agent, chatter)` 多 session 并发完成 turn,断言总提取批次数与 turn 数一致、无重复无遗漏(覆盖问题 1/2)。
- **白名单回归**:构造含 steer / 群聊注入 / goal_context 的 session,断言只有真锚点计入、turn 区间切分正确(覆盖问题 3)。
- **输入解耦**:压缩后断言提取读到的是归档原文而非摘要(覆盖问题 5)。

---

**一句话总结**:在 `session_messages` 加 `turn_status`(白名单默认值,`''｜running｜done`)与 `extraction_id`(NULL=未提取)两列,turn 起点写锚点并拿回 seq、终点标 done,触发与认领在单个写事务内完成、`extraction_id` 逐行标记既防重又为压缩提取去重,提取输入从归档表按 turn 区间回放;三后端用代码库已有的 dialect 分支与部分索引降级套路落地。
