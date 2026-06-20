export interface StatusResponse {
  configured: boolean;
  registrationOpen?: boolean;
  running: boolean;
  port: number;
  mode?: string;
  version?: string;
  uptime: string;
  agents: AgentInfo[];
  channels: ChannelInfo[];
  provider: ProviderInfo;
  cronJobs?: number;
  plugins?: number;
  userId?: string;
  isAdmin?: boolean;
  users?: number;
}

export interface RegisterRequest {
  username: string;
  email: string;
  password: string;
  displayName?: string;
}

export async function register(req: RegisterRequest): Promise<MeResponse> {
  const res = await fetch("/api/register", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function getRegistration(): Promise<{ open: boolean }> {
  const res = await apiFetch("/api/admin/registration");
  return res.json();
}

export async function setRegistration(open: boolean): Promise<{ open: boolean }> {
  const res = await apiFetch("/api/admin/registration", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ open }),
  });
  return res.json();
}

export interface AgentInfo {
  id: string;
  name?: string;
  model: string;
  workspace: string;
}

export interface ChannelInfo {
  type: string;
  botUsername: string;
  enabled?: boolean;
  status?: string;
}

export interface ProviderInfo {
  name: string;
  model: string;
  apiBase: string;
  apiKey: string;
}

export interface AgentDetail {
  id: string;
  name?: string;
  description?: string;
  avatarUrl?: string;       // /api/agents/{id}/files/avatar.png — 可能返回 404
  userId?: string;          // 所有者的用户 ID (agents.user_id)
  // role 区分调用者拥有的 agent 和通过公共链接访问的 agent。
  // "viewer" 会阻止 UI 进入配置标签页
  // (自定义 / 技能 / 渠道 / 调度器 / 模型)。后端
  // 在 /api/agents 和 /api/agents/{id} 上始终发送此字段。
  role?: "owner" | "viewer";
  // isPublic: 当为 true 时，任何拥有聊天 URL 的人都可以在
  // 自己的 user_id 下与此 agent 聊天（会话/记忆按聊
  // 天者分区）。所有者可在编辑对话框中修改。默认 false。
  isPublic?: boolean;
  // shareModelConfig: 当为 true 时，使用此 agent 的聊天者将继承
  // 所有者的用户级 + agent 级模型和提供者配置
  //（当前的"回退到所有者密钥"行为）。当为 false（默认值）时，
  // 聊天者仅能看到自己的用户级 + 系统级配置 — 他们需要自带
  // 模型/提供者，否则 agent 对他们不可用。
  shareModelConfig?: boolean;
  model: string;
  workspace?: string;
  maxTokens?: number;
  temperature?: number;
  maxToolIterations?: number;
  thinking?: string;
  // promptMode 是后端当前保存在 agents.defaults 行上的值。
  // 空 / undefined = 无覆盖（运行时回退到 "agent"）。参见
  // AgentUpdatePayload.promptMode 了解允许的值。LLM 看到的内置
  // 工具集由此模式决定 — 设计时没有单独的允许列表字段。
  // 通过插件或 MCP 扩展工具，而非按 agent 切换。
  promptMode?: string;
  // splitReplies 是按 agent 的多气泡覆盖。统一应用于
  // 每个 IM 渠道 — 开启时，agent 可以在气泡之间发出
  // SplitMessageMarker，调度器会遵循它。
  // null / undefined / 类 false 值 = 每次回复单个气泡（默认）。
  splitReplies?: boolean | null;
  // autoPersist 是按 agent 的"自动记住聊天者"切换。
  // 开启时，每 N 轮运行时会触发一次 LLM 驱动的提炼过程，
  // 将提取的事实追加到 USER.md（聊天者资料）和
  // MEMORY.md（长期笔记）中。主要在聊天机器人模式下需要 —
  // 该模式的精选工具允许列表排除了 write_file，因此
  // 这是 agent 跨会话记住聊天者的唯一途径。
  autoPersist?: boolean | null;
  // plugins 是按 agent 的钩子插件启用覆盖：pluginID →
  // enabled。缺失的键回退到系统范围的启用状态
  //（可通过 /api/plugins 查看）。null/undefined 表示
  // "完全没有按 agent 的覆盖"。
  plugins?: Record<string, boolean> | null;
  soul?: string;
  skills?: string[];
  tools?: string[];
}

export interface SkillEnvSpec {
  name: string;
  description?: string;
  required?: boolean;
  secret?: boolean;
}

export interface SkillInfo {
  name: string;
  description: string;
  location: string;
  type: string;
  envSpec?: SkillEnvSpec[];
}

export interface SkillEntryCfg {
  enabled?: boolean;
  apiKey?: string;
  env?: Record<string, string>;
}

// updateSkillEntries 持久化技能的 env/apiKey 补丁。当 agentId
// 有值时，补丁写入 cfg.Skills.AgentEntries[agentId]（按 agent
// 覆盖），否则写入 cfg.Skills.Entries（全局默认）。运行时
// 优先解析 agent 级配置，然后回退到全局。
export async function updateSkillEntries(
  entries: Record<string, SkillEntryCfg>,
  agentId?: string,
) {
  const body = agentId
    ? { skills: { agentEntries: { [agentId]: entries } } }
    : { skills: { entries } };
  const res = await apiFetch("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return res.json();
}

export interface PluginInfo {
  id: string;
  type: string;
  version: string;
  status: string;
  enabled: boolean;
  config?: Record<string, unknown>;
}

export interface CronJobInfo {
  id: string;
  name: string;
  type: string;
  schedule: string;
  agentId: string;
  channel: string;
  chatId: string;
  message: string;
  enabled: boolean;
  lastRun?: string;
  nextRun?: string;
}

export interface ModelCost {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

export interface ModelEntry {
  id: string;
  name: string;
  reasoning: boolean;
  input: string[];
  cost: ModelCost;
  contextWindow: number;
  maxTokens: number;
}

export interface ProviderData {
  apiKey: string;
  apiBase: string;
  apiType?: string;
  authType?: string;
  models?: ModelEntry[];
}

export interface ConfigResponse {
  providers: Record<string, ProviderData>;
  agents: {
    defaults: {
      model: string;
      maxTokens: number;
      temperature: number;
      maxToolIterations: number;
    };
  };
  channels: Record<string, { enabled: boolean; botToken?: string }>;
  storage: { type: string; dsn?: string };
  sandbox?: {
    enabled: boolean;
    backend?: string;
    // 遗留的单插槽镜像字段；只读回退。下面的每个后端
    // 字段在设置时是权威的，因此在 UI 中切换后端会
    // 保留每个后端上次输入的值。
    image?: string;
    dockerImage?: string;
    e2bTemplate?: string;
    boxliteSnapshot?: string;
    e2bKey?: string;
    boxliteUrl?: string;
    boxliteClientId?: string;
    boxliteKey?: string;
    boxlitePrefix?: string;
  };
  wechat?: {
    splitReplies?: boolean;
  };
  hooks: { enabled: boolean; token?: string; path?: string; port?: number };
  cronJobs?: Array<Record<string, unknown>>;
  skills?: {
    entries?: Record<string, SkillEntryCfg>;
    // 按 agent 的覆盖，键为 agentID → skillName → entry。UI
    // 仅在 agent 级 /agents/<id>/skills 页面上展示这些；
    // SkillsLoader.SkillEnvVars 优先解析
    // agentEntries[<agent>][<skill>]，
    // 然后回退到全局 entries 映射。
    agentEntries?: Record<string, Record<string, SkillEntryCfg>>;
  };
// 仪表板渲染继承状态所需的表现提示，
    // 无需客户端重新解析作用域链。systemDefaultModel
    // 是 agents.defaults.model 仅从系统作用域解析
    // 出的值 — 与 agents.defaults.model（合并值）
    // 比较，即可知道调用者是否在用户作用域上进行了覆盖。
  meta?: {
    systemDefaultModel?: string;
  };
}

// 云模式的认证令牌。登录时通过 setAuthToken() 设置；本地模式下为空。
let authToken = "";

export function setAuthToken(token: string) {
  authToken = token;
  if (token) {
    localStorage.setItem("bkclaw_token", token);
  } else {
    localStorage.removeItem("bkclaw_token");
  }
}

export function getAuthToken(): string {
  if (!authToken) {
    authToken = localStorage.getItem("bkclaw_token") || "";
  }
  return authToken;
}

// fetch 的封装，在设置了令牌时注入 Authorization 头，
// 并始终包含用于用户名/密码登录的 cookie 会话。Cookie
// 是 Web UI 的主要凭证；bearer 令牌仅用于手动将令牌
// 放入 localStorage 的编程客户端。
//
// 当页面 URL 携带 ?actAs=<userId> 时，该参数会被镜像到
// 每个 API 请求中，以便 super_admin 打开其他用户的资源
// （例如从管理聊天页面进入 /agents/<id>/chat/<sid>/?actAs=<uid>）
// 时实际读写该用户的作用域。中间件级别的 actAs 锁定
// 使这些请求变为只读。
export async function apiFetch(url: string, init?: RequestInit): Promise<Response> {
  const token = getAuthToken();
  const headers: Record<string, string> = {
    ...(init?.headers as Record<string, string> || {}),
  };
  if (token) {
    headers["Authorization"] = `Bearer ${token}`;
  }
  if (typeof window !== "undefined") {
    const pageActAs = new URLSearchParams(window.location.search).get("actAs");
    if (pageActAs && !/[?&]actAs=/.test(url)) {
      url += (url.includes("?") ? "&" : "?") + "actAs=" + encodeURIComponent(pageActAs);
    }
  }
  return fetch(url, { credentials: "same-origin", ...init, headers });
}

// 登录 + 登出 + 当前用户

export interface MeResponse {
  ok: boolean;
  user?: {
    id: string;
    username: string;
    email: string;
    role: string;
    displayName?: string;
    avatarUrl?: string;
    status: string;
    // -1 = 无限制，0 = 禁止自建，N>0 = 最多拥有 N 个 agent
    agentQuota?: number;
  };
  authMethod?: string;
  actAsUserId?: string;
  readOnly?: boolean;
  // 'self-hosted'（默认）或 'hosted' — 由守护进程上的
  // BKCLAW_DEPLOY 环境变量决定。前端据此限制仅限本地的
  // 便利功能（在 Finder 中打开、未来的 $EDITOR 钩子）。
  deployMode?: "self-hosted" | "hosted";
  error?: string;
}

export async function login(loginField: string, password: string): Promise<MeResponse> {
  const res = await fetch("/api/login", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ login: loginField, password }),
  });
  return res.json();
}

export async function logout(): Promise<void> {
  await apiFetch("/api/logout", { method: "POST" });
  setAuthToken("");
}

export async function getMe(): Promise<MeResponse> {
  const res = await apiFetch("/api/me");
  return res.json();
}

export async function updateMe(req: { displayName: string; avatarUrl: string }) {
  const res = await apiFetch("/api/me", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function changeMyPassword(req: { oldPassword: string; newPassword: string }) {
  const res = await apiFetch("/api/me/password", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

// 引导

export interface OnboardRequest {
  username: string;
  email: string;
  password: string;
  displayName?: string;
  provider?: string;
  apiBase?: string;
  apiKey?: string;
  apiType?: string;
  authType?: string;
  model?: string;
  agentName?: string;
  sandboxEnabled?: boolean;
  sandboxBackend?: string;
  sandboxImage?: string;
  sandboxE2BKey?: string;
  sandboxBoxliteUrl?: string;
  sandboxBoxliteClientId?: string;
  sandboxBoxliteKey?: string;
  sandboxBoxlitePrefix?: string;
}

export async function onboard(req: OnboardRequest): Promise<{ ok: boolean; error?: string }> {
  const res = await fetch("/api/onboard", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

// 用户管理 — 顶层为仅管理员操作（CRUD），嵌套资源
// （apikeys/agents 在 /api/users/{id}/... 下）为管理员或本人。
// /api/admin/* 前缀已移除，改为扁平资源路径；
// 权限在每个处理程序内部强制执行。

export async function adminListUsers() {
  const res = await apiFetch("/api/users");
  return res.json();
}

export async function adminListAgents() {
  const res = await apiFetch("/api/agents?all=true");
  return res.json();
}

export async function adminCreateUser(req: {
  username: string;
  email: string;
  password: string;
  displayName?: string;
  role?: string;
  agentQuota?: number | null;
}) {
  const res = await apiFetch("/api/users", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function adminUpdateUser(
  id: string,
  req: { displayName?: string; role?: string; status?: string; agentQuota?: number | null },
) {
  const res = await apiFetch(`/api/users/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function adminDeleteUser(id: string) {
  const res = await apiFetch(`/api/users/${id}`, { method: "DELETE" });
  return res.json();
}

export async function adminResetPassword(id: string, password: string) {
  const res = await apiFetch(`/api/users/${id}/password`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ password }),
  });
  return res.json();
}

// API 密钥（按用户）

export async function listApikeys() {
  const res = await apiFetch("/api/apikeys");
  return res.json();
}

export type ApikeyType = "admin" | "user" | "agent";

export async function createApikey(req: { name: string; type: ApikeyType; agentIds?: string[] }) {
  const res = await apiFetch("/api/apikeys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function deleteApikey(id: string) {
  const res = await apiFetch(`/api/apikeys/${id}`, { method: "DELETE" });
  return res.json();
}

export async function rotateApikey(id: string) {
  const res = await apiFetch(`/api/apikeys/${id}/rotate`, { method: "POST" });
  return res.json();
}

export async function setApikeyAgents(id: string, agentIds: string[]) {
  const res = await apiFetch(`/api/apikeys/${id}/agents`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ agentIds }),
  });
  return res.json();
}

// 作用域化的提供者和渠道

export type ScopeName = "system" | "user" | "agent";

export interface ProviderRow {
  id: string;
  scope: ScopeName;
  scopeId: string;
  name: string;
  apiBase?: string;
  apiKey?: string;       // 读取时已遮蔽
  apiType?: string;
  authType?: string;
  models?: ModelEntry[];
  updatedAt?: string;
}

export interface ChannelRow {
  id: string;
  scope: ScopeName;
  scopeId: string;
  type: string;
  enabled: boolean;
  botToken?: string;     // 读取时已遮蔽
  appToken?: string;
  credentialKey?: string;
  updatedAt?: string;
}

export async function listProviders(scope?: ScopeName, scopeId?: string) {
  const params = new URLSearchParams();
  if (scope) params.set("scope", scope);
  if (scopeId) params.set("scopeId", scopeId);
  const qs = params.toString();
  const url = "/api/providers" + (qs ? `?${qs}` : "");
  const res = await apiFetch(url);
  return res.json();
}

export async function createProvider(req: {
  scope: ScopeName;
  scopeId: string;
  name: string;
  apiBase?: string;
  apiKey?: string;
  apiType?: string;
  authType?: string;
  models?: ModelEntry[];
}) {
  const res = await apiFetch("/api/providers", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function updateProvider(id: string, req: Partial<ProviderRow>) {
  const res = await apiFetch(`/api/providers/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function deleteProvider(id: string) {
  const res = await apiFetch(`/api/providers/${id}`, { method: "DELETE" });
  return res.json();
}

// testStoredProvider 使用保存的提供者行中的 apiKey 在服务端
// 测试连接，这样编辑对话框无需用户重新粘贴密钥即可验证模型 ID。
// 后端不会将未遮蔽的密钥返回给浏览器，因此这是从编辑模式
// 测试的唯一方式。
//
// 非密钥覆盖（apiBase / apiType / authType）在用户于表单中
// 编辑时会透传 — 保存行的值仅用作回退。否则，仅编辑 URL 然后
// 点击测试会安静地重新 ping 旧的已保存 URL 并报告成功。
export async function testStoredProvider(
  providerId: string,
  model: string,
  overrides?: { apiBase?: string; apiType?: string; authType?: string },
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(`/api/providers/${providerId}/test`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ model, ...(overrides ?? {}) }),
  });
  return res.json();
}

export async function listScopedChannels(scope?: ScopeName, scopeId?: string) {
  const params = new URLSearchParams();
  if (scope) params.set("scope", scope);
  if (scopeId) params.set("scopeId", scopeId);
  const qs = params.toString();
  const url = "/api/scoped-channels" + (qs ? `?${qs}` : "");
  const res = await apiFetch(url);
  return res.json();
}

export async function createScopedChannel(req: {
  scope: ScopeName;
  scopeId: string;
  type: string;
  enabled: boolean;
  botToken?: string;
  appToken?: string;
  credentialKey?: string;
}) {
  const res = await apiFetch("/api/scoped-channels", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function updateScopedChannel(id: string, req: Partial<ChannelRow>) {
  const res = await apiFetch(`/api/scoped-channels/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function deleteScopedChannel(id: string) {
  const res = await apiFetch(`/api/scoped-channels/${id}`, { method: "DELETE" });
  return res.json();
}

// 状态
export async function getStatus(): Promise<StatusResponse> {
  const res = await apiFetch("/api/status");
  return res.json();
}

// 提供者
export async function testProvider(config: { apiBase: string; apiKey: string; model: string; apiType?: string; authType?: string }) {
  const res = await apiFetch("/api/test-provider", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

// 配置 — 持久化的 system_settings 块（仅 super_admin）。
export async function saveConfig(config: Record<string, unknown>) {
  const res = await apiFetch("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

export async function getConfig(): Promise<ConfigResponse> {
  const res = await apiFetch("/api/config");
  return res.json();
}

export async function updateConfig(config: Record<string, unknown>) {
  const res = await apiFetch("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

// 工作区文件列表 — 用于对比某轮的输出，以便聊天
// UI 可以在最终回复下方展示生成的文件。
export interface WorkspaceFile {
  path: string;
  size: number;
  modTime: number;
}

// revealAgentWorkspace 在操作系统的原生文件浏览器中
// 打开此作用域的工作区文件夹（Finder/Explorer/xdg-open）。
// 仅限自托管 — 托管部署返回 403；UI 会隐藏触发按钮，
// 因此调用者通常不会触发此路径。sessionId 或 projectId
// 之一用于限定范围：传 sessionId 针对聊天，projectId 针对
// 项目着陆页，都不传则针对 agent 根目录（管理员浏览器）。
export async function revealAgentWorkspace(
  agentId: string,
  sessionId?: string,
  projectId?: string,
): Promise<{ ok: boolean; path?: string; error?: string }> {
  const params = new URLSearchParams();
  if (sessionId) params.set("sessionId", sessionId);
  if (projectId) params.set("projectId", projectId);
  const qs = params.toString();
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/workspace/reveal${qs ? "?" + qs : ""}`,
    { method: "POST" },
  );
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    return { ok: false, error: (data.error as string) || `HTTP ${res.status}` };
  }
  return { ok: true, path: data.path as string };
}

export async function listAgentFiles(
  agentId: string,
  sessionId?: string,
  projectId?: string,
): Promise<WorkspaceFile[]> {
  // sessionId 限定为单个聊天；projectId（在项目着陆页未选聊天时使用）
  // 限定为整个项目树（其下所有聊天 + 根级共享文件）。调用者传其一 —
  // 都为空表示 agent 级范围（管理员浏览器）。
  const params = new URLSearchParams();
  if (sessionId) params.set("sessionId", sessionId);
  if (projectId) params.set("projectId", projectId);
  const qs = params.toString();
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/files${qs ? "?" + qs : ""}`,
  );
  if (!res.ok) return [];
  const data = await res.json();
  return (data.files || []) as WorkspaceFile[];
}

// 聊天
export interface ChatHistoryMessage {
  role: "user" | "assistant" | "tool";
  content?: string;
  toolCalls?: { id: string; name: string; arguments: string }[];
  name?: string;
  toolCallId?: string;
  // 对于 role==="tool"，此字段携带沙箱标志等；对于
  // role==="assistant"，它可以携带 iterationCapReached / iterationCapValue，
  // 以便聊天 UI 在历史记录重载时为气泡添加标记。
  metadata?: ToolResultMetadata;
  // 设置在原始回合携带图像附件的 user 角色消息上。
  // 聊天 UI 将这些渲染为从历史加载的气泡中的内联缩略图。
  imageUrls?: string[];
  // 用于通过 IM 桥接（Discord、Telegram 等）到达的用户轮次。
  // 聊天面板在每个此类气泡上渲染头像 + 昵称标题，以便 agent
  // 所有者能看到正在对话的人。这些不会传递给 LLM — 它们
  // 存储在 Message.Metadata 上，也会从持久化的 Content 中剥离。
  senderName?: string;
  senderAvatarUrl?: string;
  senderId?: string;
  senderChannel?: string;
}

export interface TodoItem {
  text: string;
  done: boolean;
}

export interface TodoState {
  items: TodoItem[];
  raw: string;
}

// getChatTodo 获取 agent 维护的按会话的 todo.md。
// 当文件尚不存在时（新会话或未使用 todo 约定的回合）
// 返回 {items: [], raw: ""} — 调用者应在此时隐藏面板。
export async function getChatTodo(agentId: string, sessionId: string): Promise<TodoState> {
  if (!agentId || !sessionId) return { items: [], raw: "" };
  const res = await apiFetch(
    `/api/chat/todo?agentId=${encodeURIComponent(agentId)}&sessionId=${encodeURIComponent(sessionId)}`,
  );
  if (!res.ok) return { items: [], raw: "" };
  const data = await res.json().catch(() => ({}));
  return {
    items: Array.isArray(data?.items) ? data.items : [],
    raw: typeof data?.raw === "string" ? data.raw : "",
  };
}

export async function getChatHistory(agentId: string, sessionId: string): Promise<ChatHistoryMessage[]> {
  const res = await apiFetch(`/api/chat/history?agentId=${encodeURIComponent(agentId)}&sessionId=${encodeURIComponent(sessionId)}`);
  if (!res.ok) return [];
  const data = await res.json();
  // 后端包装为 { history: [...] }；旧格式为原始数组。
  if (Array.isArray(data?.history)) return data.history;
  return Array.isArray(data) ? data : [];
}

// ChatHistoryWithCursor 返回相同的历史列表加上此会话
// 最新的 chat_events.seq — 订阅 SSE 所需的恢复游标。
// 在挂载聊天面板时使用；游标被传入
// /api/chat/subscribe?since=N，以便新刷新的页面能拾取
// 服务器上仍在流式传输的进行中回合。
export interface ChatHistoryResult {
  history: ChatHistoryMessage[];
  latestEventSeq: number; // -1 表示尚未记录任何事件
}

export async function getChatHistoryWithCursor(agentId: string, sessionId: string): Promise<ChatHistoryResult> {
  const res = await apiFetch(`/api/chat/history?agentId=${encodeURIComponent(agentId)}&sessionId=${encodeURIComponent(sessionId)}`);
  if (!res.ok) return { history: [], latestEventSeq: -1 };
  const data = await res.json();
  const history: ChatHistoryMessage[] = Array.isArray(data?.history)
    ? data.history
    : Array.isArray(data) ? data : [];
  const seqRaw = data?.latestEventSeq;
  const latestEventSeq = typeof seqRaw === "number" ? seqRaw : -1;
  return { history, latestEventSeq };
}

export interface ChatSessionEntry {
  id: string;
  // channel/accountId/chatId 让侧栏渲染按渠道的图标，
  // 让聊天页面区分"同一 agent 的微信线程 vs 网页线程"。
  // 空渠道表示"漏网回填的遗留行" — 在 UI 侧回退到网页样式。
  channel?: string;
  accountId?: string;
  chatId?: string;
  // projectId 将此聊天归入按(用户, agent)的项目下。
  // 为空 = 散聊（渲染在扁平的聊天区域中）。
  projectId?: string;
  title?: string;
  preview: string;
  thumbnailUrl?: string;
  createdAt?: number;
  updatedAt?: number;
}

export interface ProjectEntry {
  id: string;
  name: string;
  description?: string;
  createdAt?: string;
  updatedAt?: string;
}

export async function listProjects(agentId: string): Promise<ProjectEntry[]> {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/projects`,
  );
  if (!res.ok) return [];
  const data = await res.json();
  return Array.isArray(data?.projects) ? data.projects : [];
}

export async function createProject(
  agentId: string,
  req: { name: string; description?: string },
): Promise<ProjectEntry | { error: string }> {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/projects`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
    },
  );
  return res.json();
}

export async function updateProject(
  agentId: string,
  projectId: string,
  req: { name?: string; description?: string },
): Promise<ProjectEntry | { error: string }> {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/projects/${encodeURIComponent(projectId)}`,
    {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
    },
  );
  return res.json();
}

// deleteProject 返回结构化数据，因为服务器在项目仍
// 拥有聊天时回复 409 — 提供 sessionCount 以便调用者
// 可以渲染有用的提示而非仅仅"删除失败"。
export async function deleteProject(
  agentId: string,
  projectId: string,
): Promise<{ ok?: boolean; error?: string; sessionCount?: number }> {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/projects/${encodeURIComponent(projectId)}`,
    { method: "DELETE" },
  );
  return res.json();
}


// AdminChatSessionEntry 扩展 ChatSessionEntry，增加了渲染跨租户
// 聊天列表所需的 (user, agent) 所有权信息 — agent 名称 + 所有者
// 显示字段，在服务端拼接以便客户端无需逐 agent 请求。
// 由 GET /api/admin/chats 提供（仅 super_admin）。
export interface AdminChatSessionEntry extends ChatSessionEntry {
  agentId: string;
  agentName?: string;
  userId: string;
  ownerUsername?: string;
  ownerDisplayName?: string;
  ownerEmail?: string;
}

export async function adminListChats(): Promise<AdminChatSessionEntry[]> {
  const res = await apiFetch("/api/admin/chats");
  if (!res.ok) return [];
  const data = await res.json();
  return Array.isArray(data?.sessions) ? data.sessions : [];
}

export async function getChatSessions(agentId: string): Promise<ChatSessionEntry[]> {
  const res = await apiFetch(`/api/chat/sessions?agentId=${encodeURIComponent(agentId)}`);
  if (!res.ok) return [];
  const data = await res.json();
  // 后端将列表包装为 { sessions: [...] }。也兼容原始数组
  // 格式，以防仍有较旧的部署。
  if (Array.isArray(data?.sessions)) return data.sessions;
  return Array.isArray(data) ? data : [];
}

export async function renameChatSession(agentId: string, sessionId: string, title: string) {
  const res = await apiFetch(`/api/chat/sessions/${encodeURIComponent(sessionId)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ agentId, title }),
  });
  return res.json();
}

export async function deleteChatSession(agentId: string, sessionId: string) {
  const res = await apiFetch(
    `/api/chat/sessions/${encodeURIComponent(sessionId)}?agentId=${encodeURIComponent(agentId)}`,
    { method: "DELETE" },
  );
  return res.json();
}

// moveChatSessionToProject 将聊天重新分配到项目（或当 projectId 为 ""
// 时将其脱离回散聊列表）。支撑侧栏的拖放交互。成功时返回
// { ok }；失败时返回 { error, code? } — code="destination_exists"
// 表示目标工作区目录已有文件（防御性 409）。
export async function moveChatSessionToProject(
  agentId: string,
  sessionId: string,
  projectId: string,
): Promise<{ ok?: boolean; error?: string; code?: string }> {
  const res = await apiFetch(
    `/api/chat/sessions/${encodeURIComponent(sessionId)}/project`,
    {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ agentId, projectId }),
    },
  );
  return res.json();
}

export async function sendChat(agentId: string, sessionId: string, message: string): Promise<{ response: string }> {
  const res = await apiFetch("/api/chat", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ agentId, sessionId, message }),
  });
  return res.json();
}

// steerChat 将消息缓冲到会话中正在进行的轮次中。
// 当服务器将其合并到正在运行的轮次时返回 true（200），
// 当没有活跃轮次时返回 false（409）— 调用者应回退到
// 正常的 sendChatStream。仅在意外/传输错误时抛出异常。
export async function steerChat(
  agentId: string,
  sessionId: string,
  message: string,
  projectId?: string,
): Promise<boolean> {
  const res = await apiFetch("/api/chat/steer", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      agentId,
      sessionId,
      projectId: projectId || undefined,
      message,
    }),
  });
  if (res.status === 409) return false;
  if (!res.ok) throw new Error(`steer failed: ${res.status}`);
  const data = await res.json().catch(() => ({}));
  return data?.buffered === true;
}

export interface ToolResultMetadata {
  sandbox?: boolean;
  // 标记在按轮工具迭代上限触发时后端发出的强制最终交付助手消息上。
  // 让 UI 显示一个小标记，以便用户知道答案是在预算限制下生成的，
  // 可能不完整。
  iterationCapReached?: boolean;
  iterationCapValue?: number;
  // 标记在由计划模式（编辑器切换）生成的助手消息上。
  // 该气泡是一个计划，而非执行结果 — UI 显示不同的
  // 标记，以便用户知道需要审阅并回复"go"（或编辑）。
  planMode?: boolean;
}

export interface ChatStreamEvent {
  type:
    | "content"
    | "content_delta"
    | "tool_call"
    | "tool_result"
    | "steer"
    | "error"
    | "done"
    | "subagent_progress";
  // 由 chat_events 分配的按会话单调序列号。让
  // 聊天页面去重在活跃 POST 流和并行
  // /api/chat/subscribe SSE 连接上到达的事件。-1 表示
  // "未分配"（遗留/预持久化代码路径）。
  //
  // content_delta 故意不持久化（每轮会生成 100+
  // 行但没有回放价值 — 后面的 content 事件包含
  // 完整的最终文本）。所以 content_delta 总是以
  // seq=-1 到达；面板必须不去重直接接受它。
  seq?: number;
  data?: {
    content?: string;
    // delta 是 content_delta 事件追加到正在进行的
    // 助手气泡上的增量文本。
    delta?: string;
    id?: string;
    name?: string;
    arguments?: string;
    result?: string;
    message?: string;
    metadata?: ToolResultMetadata;
    // subagent_progress 载荷 — 仅在 type === "subagent_progress" 时填充。
    iteration?: number;
    max?: number;
    phase?: "thinking" | "running" | "final-delivery" | "done";
    tools?: string[];
    // usage 载荷 — 仅在 type === "done" 时填充。汇报本轮结束后的
    // 上下文占用，供聊天页面渲染「已用上下文百分比」指示器。
    // usedTokens 优先取 provider 报告的真实输入侧 token，缺失时回退到
    // chars/4 估算；triggerTokens 是后端自动压缩的触发阈值（token 数）。
    usage?: ContextUsage;
  };
}

// ContextUsage 镜像后端 done 事件的 usage 载荷。
export interface ContextUsage {
  usedTokens: number;
  contextWindow: number;
  triggerTokens: number;
}

export async function sendChatStream(
  agentId: string,
  sessionId: string,
  message: string,
  onEvent: (evt: ChatStreamEvent) => void,
  signal?: AbortSignal,
  imageUrls?: string[],
  // projectId（设置时）是"此聊天属于项目 X"的提示，
  // 在任何会话行存在之前通过 URL 传递（?project=<pid>）。
  // 服务器在首次 SaveSession 时标记它；后续轮次忽略
  // 它（以行为准）。
  projectId?: string,
  // params 是后端作为 bus.InboundMessage.Params 转发的自由格式
  // 数据。agent 循环直接读取已识别的键（planMode 等）；
  // 未识别的键通过 renderClientParams 放入"Client
  // Parameters"系统消息。
  params?: Record<string, unknown>,
): Promise<void> {
  const res = await apiFetch("/api/chat/stream", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      agentId,
      sessionId,
      projectId: projectId || undefined,
      message,
      imageUrls: imageUrls ?? [],
      params: params && Object.keys(params).length > 0 ? params : undefined,
    }),
    signal,
  });
  if (!res.ok) {
    let msg = `stream failed: ${res.status}`;
    try {
      const data = await res.json();
      if (data?.error) msg = String(data.error);
    } catch { /* 非 JSON 请求体 — 保持状态回退 */ }
    throw new Error(msg);
  }
  if (!res.body) throw new Error("stream failed: no body");

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  // 读取器循环在服务器显式发送的 {type:"done"} 事件或
  // 干净的流结束（getReader 的 done 标志）时退出。我们
  // 在 "done" 时提前拆卸，以避免最终刷新之后可能排队的
  // 任何尾部字节被重新解析并显示为虚假错误。
  let finished = false;
  while (!finished) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });

    const lines = buffer.split("\n");
    buffer = lines.pop() || "";

    for (const line of lines) {
      if (!line.startsWith("data: ")) continue;
      try {
        const evt = JSON.parse(line.slice(6)) as ChatStreamEvent;
        onEvent(evt);
        if (evt.type === "done") {
          finished = true;
        }
      } catch { /* 跳过格式错误的帧 */ }
    }
  }
  try { await reader.cancel(); } catch { /* 忽略 */ }
}

export interface UploadedFile {
  path: string;
  size: number;
}

export async function uploadAgentFiles(
  agentId: string,
  sessionId: string,
  files: File[],
): Promise<UploadedFile[]> {
  const fd = new FormData();
  for (const f of files) fd.append("file", f, f.name);
  const qs = sessionId ? `?sessionId=${encodeURIComponent(sessionId)}` : "";
  const res = await apiFetch(`/api/agents/${encodeURIComponent(agentId)}/files${qs}`, {
    method: "POST",
    body: fd,
  });
  if (!res.ok) throw new Error(`upload failed: ${res.status}`);
  const data = await res.json();
  return (data.files || []) as UploadedFile[];
}

// Agent 列表
export async function getAgents(): Promise<AgentDetail[]> {
  const res = await apiFetch("/api/agents");
  if (!res.ok) {
    // 401 等返回 JSON 错误信封 — 抛出以便调用者回退到
    // [] 而非在非数组上 .map 崩溃。
    throw new Error(`getAgents failed: ${res.status}`);
  }
  const data = await res.json();
  // 后端返回 { agents: [...] }。也兼容原始数组，以防
  // 仍有较旧的处理程序。
  if (Array.isArray(data?.agents)) return data.agents as AgentDetail[];
  return Array.isArray(data) ? (data as AgentDetail[]) : [];
}

// 单个 agent 详情。回退使用与 /api/agents/{id} 其余部分
// 相同的权限规则 — 所有者或 super_admin 可获取。由
// 聊天标题使用，当 agent 不在调用者自己的列表中时解析名称
//（管理员查看其他用户的 agent）。
export async function getAgent(id: string): Promise<AgentDetail | null> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(id)}`);
  if (!res.ok) return null;
  const data = await res.json();
  return (data?.agent as AgentDetail) || null;
}

// getAgentStatus 返回原始 HTTP 状态和 agent，以便
// 调用者区分 403（禁止 — 非所有者，非公共）与 404
//（无此 agent）与成功。普通的 getAgent() 将所有失败
// 合并为 null，聊天页面无法区分。
export async function getAgentStatus(
  id: string,
): Promise<{ status: number; agent: AgentDetail | null }> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(id)}`);
  if (!res.ok) return { status: res.status, agent: null };
  const data = await res.json();
  return { status: res.status, agent: (data?.agent as AgentDetail) || null };
}

// AgentRegisteredTool 是 /api/agents/{id}/tools/registered 返回的
// 每个工具的结构：name（白名单使用的规范标识符）、
// description（选择器 UI 的一句话说明）和 source（工具来源：
// builtin / mcp / plugin）。后端保证稳定排序，以便仪表板渲染
// 是确定性的。
export interface AgentRegisteredTool {
  name: string;
  description: string;
  source: "builtin" | "mcp" | "plugin" | string;
}

// listAgentRegisteredTools 获取 agent 的实时工具注册表。
// 驱动工具标签页的白名单复选框选择器，以便操作者点击而
// 非从记忆中输入名称。认证失败或 agent 未加载时返回 null
//（后端在该情况下返回 404）。
export async function listAgentRegisteredTools(
  id: string,
): Promise<AgentRegisteredTool[] | null> {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(id)}/tools/registered`,
  );
  if (!res.ok) return null;
  const data = await res.json();
  return (data?.tools as AgentRegisteredTool[]) || [];
}

export async function createAgent(agent: Partial<AgentDetail>) {
  const res = await apiFetch("/api/agents", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(agent),
  });
  return res.json();
}

export interface AgentSkillsConfig {
  disabled?: string[];
  alwaysLoad?: string[];
}

// 后端接受 model / soul / skills / providers 进行更新。
// AgentDetail.skills 是扁平 string[]（遗留），但按 agent 技能
// 配置实际是 { disabled, alwaysLoad } — 使用明确的载荷类型
// 以避免两种形状在类型系统中冲突。
export interface AgentUpdatePayload {
  name?: string;
  description?: string;
  model?: string;
  soul?: string;
  skills?: AgentSkillsConfig;
  // 整体映射替换：省略以保持 providers 不变，发送 {} 以
  // 清除，或发送完整的期望映射以替换。
  providers?: Record<string, ProviderData>;
  // 切换"拥有链接的人都可以聊天"的门槛。省略以保持当前值不变。
  isPublic?: boolean;
  // 切换使用此 agent 的聊天者是否继承所有者的
  // 模型 + 提供者配置。省略以保持不变。
  shareModelConfig?: boolean;
  // PromptMode 选择框架系统提示的参与程度："agent"（完整，默认）、
  // "chatbot"（精简 — 去掉任务委派/工具使用规范/工作区更新，以
  // 保持伴侣/角色扮演人设）、"customize"（仅有日期锚点 + 引导
  // 文件 — 作者通过 SOUL.md / IDENTITY.md 自行编写整个系统提示）。
  // 传 "" 以清除。
  promptMode?: "" | "agent" | "chatbot" | "customize";
  // 按 agent 的多气泡覆盖（应用于所有 IM 渠道）。
  // 三态：省略以保持已保存的值不变；传 true/false 以
  // 显式设置；传 splitRepliesReset: true 以删除覆盖，
  // 从而应用默认行为（单气泡）。
  splitReplies?: boolean;
  splitRepliesReset?: boolean;
  // 按agent的自动持久化覆盖。与 splitReplies 相同的三态语义。
  // 为 true 时，每 N 轮运行时会发起一个小型 LLM 调用，将对话
  // 提炼到 USER.md（聊天者资料）和 MEMORY.md（长期事实）中 —
  // 参见 Agent.autoPersist。
  autoPersist?: boolean;
  autoPersistReset?: boolean;
  // 按 agent 的插件启用覆盖（补丁语义 — 未在映射中的键
  // 保持不变）。传 pluginsReset:true 以清除所有按 agent 的
  // 覆盖并回退到系统范围的启用状态。
  plugins?: Record<string, boolean>;
  pluginsReset?: boolean;
}

export async function updateAgent(id: string, agent: AgentUpdatePayload) {
  const res = await apiFetch(`/api/agents/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(agent),
  });
  return res.json();
}

// HookPlugin 是 /api/plugins/hook 返回的元数据结构 —
// 此安装上可用的钩子型插件的只读列表。
// 操作者在上下文页面选择要按 agent 启用的插件。
export interface HookPlugin {
  id: string;
  name?: string;
  description?: string;
  version?: string;
}

export async function listHookPlugins(): Promise<HookPlugin[]> {
  try {
    const res = await apiFetch("/api/plugins/hook");
    if (!res.ok) return [];
    return (await res.json()) as HookPlugin[];
  } catch {
    return [];
  }
}

export interface AgentFileConfig {
  model?: string;
  maxTokens?: number;
  temperature?: number;
  maxToolIterations?: number;
  workspace?: string;
  skills?: AgentSkillsConfig;
  providers?: Record<string, ProviderData>;
}

// 获取单个 agent 的原始 agent.json（仅按 agent 覆盖 —
// 不是合并/解析后的配置）。用于按 agent 的模型和技能管理页面。
export async function getAgentConfig(id: string): Promise<AgentFileConfig> {
  const res = await apiFetch(`/api/agents/${id}/config`);
  return res.json();
}

export async function deleteAgent(id: string) {
  const res = await apiFetch(`/api/agents/${id}`, {
    method: "DELETE",
  });
  return res.json();
}

// 技能
export async function getSkills(): Promise<SkillInfo[]> {
  const res = await apiFetch("/api/skills");
  return res.json();
}

export async function deleteSkill(name: string) {
  const res = await apiFetch(`/api/skills/${name}`, {
    method: "DELETE",
  });
  return res.json();
}

// 按 agent 的技能：列出安装在 agent 自己的 home/skills 目录中的技能。
// 按 agent 的技能会覆盖同名的全局技能。
export async function getAgentSkills(agentId: string): Promise<SkillInfo[]> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(agentId)}/skills`);
  return res.json();
}

export async function deleteAgentSkill(agentId: string, name: string) {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/skills/${encodeURIComponent(name)}`,
    { method: "DELETE" },
  );
  return res.json();
}

// 搜索结果使用 skills.sh 的格式；clawhub 有不同的格式，但管理
// UI 仅连接 skills.sh（主注册表）。需要 clawhub 的调用者通过
// source="clawhub" 的 installSkill 访问。
export interface SkillSearchResult {
  id: string;       // "<owner>/<repo>/<skillId>"
  skillId: string;  // 文件夹名称 — 也是传给 installSkill 的 slug
  name: string;
  source: string;   // "<owner>/<repo>"
  installs: number;
}

export async function searchSkills(query: string): Promise<SkillSearchResult[]> {
  if (!query.trim()) return [];
  const res = await apiFetch(`/api/skills/search?source=skillssh&q=${encodeURIComponent(query)}`);
  if (!res.ok) return [];
  const data = await res.json();
  return (data.results || []) as SkillSearchResult[];
}

export interface InstallSkillRequest {
  name: string;
  source?: "skillssh" | "clawhub" | "github" | "auto";
  repo?: string;
  agent?: string;  // 省略表示全局安装（仅管理员）
}

export interface InstallSkillResponse {
  ok: boolean;
  source?: string;
  name?: string;
  version?: string;
  installedAt?: string;
  files?: number;
  error?: string;
}

export async function installSkill(req: InstallSkillRequest): Promise<InstallSkillResponse> {
  const res = await apiFetch("/api/skills/install", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

// uploadSkill 从用户提供的 .zip 文件安装技能。zip 会在
// 后端解压到 <agent>/skills/<name>/ 目录（或当 agentId 为空时
// 解压到全局技能目录 — 仅管理员）。name 覆盖推断的文件夹名称；
// 留 undefined 让服务器自行选择（常见顶层目录 → 回退到不含扩展名的文件名）。
export async function uploadSkill(
  file: File,
  agentId?: string,
  name?: string,
): Promise<InstallSkillResponse> {
  const fd = new FormData();
  fd.append("file", file, file.name);
  if (name) fd.append("name", name);
  const qs = agentId ? `?agent=${encodeURIComponent(agentId)}` : "";
  const res = await apiFetch(`/api/skills/upload${qs}`, {
    method: "POST",
    body: fd,
  });
  return res.json();
}

// --- 工具（提供者支持的能力：web_search、image_gen、tts 等）---

export interface ToolProviderCatalog {
  name: string;
  label: string;
  needsKey: boolean;
  needsUrl: boolean;
  models: string[];
}

export interface ToolCategoryCatalog {
  name: string;
  label: string;
  providers: ToolProviderCatalog[];
}

export interface ToolProviderSettings {
  apiKey?: string;
  endpoint?: string;
  options?: Record<string, string>;
}

export interface ToolCategorySettings {
  primary?: string;
  fallbacks?: string[];
  autoFallback?: boolean;
}

export interface ToolsConfig {
  categories: ToolCategoryCatalog[];
  toolProviders: Record<string, ToolProviderSettings>;
  tools: Record<string, ToolCategorySettings>;
}

export async function getTools(): Promise<ToolsConfig> {
  const res = await apiFetch("/api/tools");
  return res.json();
}

export async function saveTools(payload: {
  toolProviders: Record<string, ToolProviderSettings>;
  tools: Record<string, ToolCategorySettings>;
}): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch("/api/tools", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  return res.json();
}

// 插件
export async function getPlugins(): Promise<PluginInfo[]> {
  const res = await apiFetch("/api/plugins");
  return res.json();
}

export async function updatePlugin(id: string, data: Partial<PluginInfo>) {
  const res = await apiFetch(`/api/plugins/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  });
  return res.json();
}

// 渠道
export async function getChannels(): Promise<ChannelInfo[]> {
  const res = await apiFetch("/api/channels");
  return res.json();
}

// 定时任务
export async function getCronJobs(): Promise<CronJobInfo[]> {
  const res = await apiFetch("/api/cron");
  return res.json();
}

export async function createCronJob(job: Partial<CronJobInfo>) {
  const res = await apiFetch("/api/cron", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(job),
  });
  return res.json();
}

export async function updateCronJob(id: string, job: Partial<CronJobInfo>) {
  const res = await apiFetch(`/api/cron/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(job),
  });
  return res.json();
}

export async function deleteCronJob(id: string) {
  const res = await apiFetch(`/api/cron/${id}`, {
    method: "DELETE",
  });
  return res.json();
}

// --- 管理 API：API 密钥 ---

// APIKey 是 GET /v1/admin/apikeys 返回的条目。key 字段
// 在服务器端对除创建/轮换响应外的所有人进行了遮蔽，
// 创建/轮换响应在单独的 key 字段下返回新签发的明文密钥。
export interface APIKey {
  id: string;
  name: string;
  key: string; // 列表响应中已遮蔽（例如 "fc_abcd****wxyz"）
  createdAt: string;
}

// 辅助函数：从非 OK 响应中提取服务器提供的 {error} 消息，
// 以便调用者显示真实原因（认证失败、重复 ID 等），
// 而非在 .apikey 为 undefined 时崩溃。
async function readError(res: Response, fallback: string): Promise<string> {
  try {
    const body = await res.json();
    if (body && typeof body.error === "string") return body.error;
  } catch {}
  return `${fallback} (HTTP ${res.status})`;
}

export async function listAPIKeys(): Promise<APIKey[]> {
  const res = await apiFetch("/v1/admin/apikeys");
  if (!res.ok) return [];
  const data = await res.json();
  return data.apikeys || [];
}

export async function createAPIKey(id: string, name: string): Promise<{ apikey: APIKey; key: string }> {
  const res = await apiFetch("/v1/admin/apikeys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ id, name }),
  });
  if (!res.ok) throw new Error(await readError(res, "create API key failed"));
  const data = await res.json();
  if (!data.apikey || !data.key) throw new Error("malformed response from server");
  return data;
}

export async function deleteAPIKey(id: string): Promise<void> {
  const res = await apiFetch(`/v1/admin/apikeys/${id}`, { method: "DELETE" });
  if (!res.ok) throw new Error(await readError(res, "delete API key failed"));
}

export async function rotateAPIKey(id: string): Promise<string> {
  const res = await apiFetch(`/v1/admin/apikeys/${id}/rotate`, { method: "POST" });
  if (!res.ok) throw new Error(await readError(res, "rotate API key failed"));
  const data = await res.json();
  if (!data.key) throw new Error("malformed response from server");
  return data.key;
}

// --- 管理 API：agent ↔ apikey 绑定 ---

// agent ID 到 apikey ID 的映射。空值表示 agent 仅限管理员访问。
export type AgentBindings = Record<string, string>;

export async function listAgentBindings(): Promise<AgentBindings> {
  const res = await apiFetch("/api/agent-bindings");
  if (!res.ok) return {};
  const data = await res.json();
  return data.bindings || {};
}

// 传 apiKeyId="" 以解绑（agent 恢复为仅管理员访问）。
export async function bindAgent(agentId: string, apiKeyId: string): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/binding`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ apiKeyId }),
  });
  return res.json();
}

// --- 按 agent 的 IM 渠道（Telegram 等）---

export interface AgentChannel {
  type: string;        // "telegram"
  accountId: string;   // Telegram 的 bot 用户名
  botUsername?: string;
  botToken: string;    // 服务器端已遮蔽
  enabled: boolean;
  updatedAt?: string;
}

// AgentCronJob 映射 store.CronJobRecord。由 GET
// /api/agents/{id}/cron 返回 — 涵盖 agent 通过
// create_cron_job 自行安排的作业以及通过其他路径
// （配置、未来管理 UI）植入的作业。lastRun / nextRun
// 是 RFC3339 字符串或不存在。
export interface AgentCronJob {
  id: string;
  agentId: string;
  name: string;
  type: string;        // "cron" | "interval" | "once"
  schedule: string;
  message: string;
  channel: string;
  chatId: string;
  accountId?: string;
  timezone: string;
  enabled: boolean;
  lastRun?: string;
  nextRun?: string;
  createdAt: string;
}

export async function listAgentCronJobs(agentId: string): Promise<AgentCronJob[]> {
  const res = await apiFetch(`/api/agents/${agentId}/cron`);
  if (!res.ok) return [];
  const data = await res.json();
  return data.jobs || [];
}

export async function deleteAgentCronJob(
  agentId: string,
  jobId: string,
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/cron/${encodeURIComponent(jobId)}`,
    { method: "DELETE" },
  );
  return res.json();
}

export async function toggleAgentCronJob(
  agentId: string,
  jobId: string,
  enabled: boolean,
): Promise<{ ok: boolean; job?: AgentCronJob; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/cron/${encodeURIComponent(jobId)}`,
    {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled }),
    },
  );
  return res.json();
}

export async function listAgentChannels(agentId: string): Promise<AgentChannel[]> {
  const res = await apiFetch(`/api/agents/${agentId}/channels`);
  if (!res.ok) return [];
  const data = await res.json();
  return data.channels || [];
}

export async function connectAgentTelegram(
  agentId: string,
  botToken: string,
): Promise<{ ok: boolean; botUsername?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/telegram`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ botToken }),
  });
  return res.json();
}

export async function connectAgentDiscord(
  agentId: string,
  botToken: string,
): Promise<{ ok: boolean; botUsername?: string; botUserId?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/discord`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ botToken }),
  });
  return res.json();
}

export async function connectAgentSlack(
  agentId: string,
  botToken: string,
  appToken: string,
): Promise<{ ok: boolean; teamName?: string; teamId?: string; botUserId?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/slack`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ botToken, appToken }),
  });
  return res.json();
}

export async function startAgentWeChatLogin(
  agentId: string,
): Promise<{ sessionId?: string; qrCode?: string; qrCodeImg?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/wechat/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  });
  return res.json();
}

export async function pollAgentWeChatLoginStatus(
  agentId: string,
  sessionId: string,
): Promise<{
  status?: "wait" | "scaned" | "confirmed" | "expired";
  connected?: boolean;
  accountId?: string;
  error?: string;
}> {
  const res = await apiFetch(
    `/api/agents/${agentId}/channels/wechat/login/status?session=${encodeURIComponent(sessionId)}`,
  );
  return res.json();
}

export async function connectAgentLINE(
  agentId: string,
  channelToken: string,
  channelSecret: string,
): Promise<{
  ok: boolean;
  botUserId?: string;
  botName?: string;
  basicId?: string;
  webhookUrl?: string;
  error?: string;
}> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/line`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ channelToken, channelSecret }),
  });
  return res.json();
}

export async function connectAgentFeishu(
  agentId: string,
  appId: string,
  appSecret: string,
  verificationToken: string,
  encryptKey: string,
  useLongConn: boolean,
): Promise<{
  ok: boolean;
  appId?: string;
  botName?: string;
  botOpenId?: string;
  webhookUrl?: string;
  useLongConn?: boolean;
  error?: string;
}> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/feishu`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      appId,
      appSecret,
      verificationToken,
      encryptKey,
      useLongConn,
    }),
  });
  return res.json();
}

export async function disconnectAgentChannel(
  agentId: string,
  type: string,
  accountId: string,
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/channels/${encodeURIComponent(type)}/${encodeURIComponent(accountId)}`,
    { method: "DELETE" },
  );
  return res.json();
}

// ---------- 管理：令牌用量 ----------

export type TokenUsageRange = "24h" | "7d" | "30d";

export interface TokenUsageTotals {
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheCreationTokens: number;
  requestCount: number;
}

export interface TokenUsageRank {
  key: string;
  tokens: number;
  inputTokens: number;
  outputTokens: number;
  requestCount: number;
}

export interface TokenUsageReport {
  range: TokenUsageRange;
  totals: TokenUsageTotals;
  topAgents: TokenUsageRank[];
  topUsers: TokenUsageRank[];
}

export async function adminGetTokenUsage(
  range: TokenUsageRange = "7d",
  limit = 10,
): Promise<TokenUsageReport> {
  const res = await apiFetch(`/api/usage?range=${range}&limit=${limit}`);
  return res.json();
}

export interface AgentTokenUsage {
  range: TokenUsageRange;
  agentId: string;
  sessions: TokenUsageRank[];
}

// 按 agent 的会话级用量，在 agent 设置对话框中展示。
// 服务端由所有者控制；公共 agent 的聊天查看者会收到 403。
export async function getAgentTokenUsage(
  agentId: string,
  range: TokenUsageRange = "7d",
  limit = 50,
): Promise<AgentTokenUsage> {
  const res = await apiFetch(
    `/api/agents/${agentId}/usage?range=${range}&limit=${limit}`,
  );
  return res.json();
}
