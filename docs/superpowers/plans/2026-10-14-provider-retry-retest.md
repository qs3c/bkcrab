# BKCrab Provider 重试与复测实施计划

**目标：** 基于 d9bcc0b 修复请求重试，验证已合入的空 error 修复，构建独立评测产物并启动用户授权的复测。
**设计：** Provider 共用 HTTP 开启响应函数，最多三次，500ms/1s 退避；只重试 429/502/503/504 与暂时性网络错误，尊重父 context，不重放已经返回的响应。无需新增依赖。
**执行：** 当前会话直接执行，用户已授权修复、构建和启动；不修改原 checkout 未提交内容，不部署生产服务。

- [ ] internal/provider/http_retry.go：共享请求 helper；OpenAI/Anthropic 的 Chat、ChatStream 接入。
- [ ] internal/provider/http_retry_test.go：真实本地 HTTP 502→成功、400 不重试、上限、取消、正文一致；流已开始不重试。
- [ ] 运行 internal/agent 已有 TestFailuresRemindWithoutDisablingTools，确认 nil error 文本失败不会 panic；无需重复修复上游 #3。
- [ ] 应用 integrations/bkcrab-eval.patch，测试入口，构建二进制，保存源码 diff、版本与产物哈希。
- [ ] 任务去重为 9 题。一个复测批次含两个有独立评分的执行组：3 题 BKCrab/DSH 配对、6 题仅 BKCrab，合计 12 次，避免平台原有矩形矩阵把未安排的 DSH 题误报 missing。
- [ ] 保留原模型、资源、1800 秒预算、守护辅助标记；使用新平台中继，独立保存配置、来源和启动记录，确认进入模型执行。
