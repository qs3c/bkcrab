"use client";

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Brain, Plus, Pencil, Trash2, Check, Cpu, Loader2 } from "lucide-react";
import {
  getAgent,
  getConfig,
  updateConfig,
  getMe,
  testProvider,
  testStoredProvider,
  listProviders,
  createProvider,
  updateProvider,
  deleteProvider,
  type ModelEntry,
  type ProviderRow,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

// 保持这些映射与 onboard 的 ProviderStep 同步，确保两个流程
// 外观和行为一致——相同的预设集、相同标签、相同的
// SelectValue 渲染子元素模式。
// `models` 是选中预设时预填到表单中的常见模型 ID。
// 用户可以保留、编辑或移除它们。空列表表示"无合理默认值"
//（自定义 / openrouter / ollama 的模型差异太大，不适合
// 内置建议）。
const PROVIDER_PRESETS: Record<
  string,
  { apiBase: string; apiType: string; authType: string; models: string[] }
> = {
  openai: { apiBase: "https://api.openai.com/v1", apiType: "openai-chat", authType: "bearer-token", models: ["gpt-5.5"] },
  openrouter: { apiBase: "https://openrouter.ai/api/v1", apiType: "openai-chat", authType: "bearer-token", models: [] },
  anthropic: { apiBase: "https://api.anthropic.com", apiType: "anthropic-messages", authType: "api-key", models: ["claude-opus-4.7", "claude-sonnet-4.7", "claude-haiku-4.5"] },
  deepseek: { apiBase: "https://api.deepseek.com", apiType: "openai-chat", authType: "bearer-token", models: ["deepseek-v4-pro", "deepseek-v4-flash"] },
  ollama: { apiBase: "http://localhost:11434/v1", apiType: "openai-chat", authType: "bearer-token", models: [] },
  custom: { apiBase: "", apiType: "openai-chat", authType: "bearer-token", models: [] },
};

const PROVIDER_LABELS: Record<string, string> = {
  openai: "OpenAI",
  openrouter: "OpenRouter",
  anthropic: "Anthropic",
  deepseek: "DeepSeek",
  ollama: "Ollama",
  custom: "自定义",
};

const API_TYPE_LABELS: Record<string, string> = {
  "openai-chat": "OpenAI 聊天补全",
  "anthropic-messages": "Anthropic 消息",
};

const AUTH_TYPE_LABELS: Record<string, string> = {
  "bearer-token": "Bearer 令牌",
  "api-key": "API 密钥请求头",
};

interface ProviderEntry {
  id: string;          // configs 行 ID——PUT/DELETE 操作必需
  name: string;
  apiBase: string;
  apiKey: string;      // 未遮蔽的草稿值（仅在编辑时设置）
  maskedKey: string;   // 服务端返回的遮蔽密钥，用于显示
  apiType: string;
  authType: string;
  models: ModelEntry[];
  // scope 用于区分行与普通用户可查看但不可修改的继承（系统）行。
  // "system" 和 "agent" 行显示继承标记且禁用编辑/删除；"user" 行
  // 是调用者自己的，完全可编辑。"agent" 仅在页面挂载于智能体上下文时
  // 出现（对话者查看所有者启用了 shareModelConfig 的共享智能体）。
  scope: "system" | "user" | "agent";
}

function emptyModel(): ModelEntry {
  return {
    id: "",
    name: "",
    reasoning: false,
    input: ["text"],
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: 200000,
    maxTokens: 8192,
  };
}

// presetModelRows 为预设声明的 ID 生成可直接编辑的 ModelEntry 行，
// 使对话框打开时已填入常见模型而非空列表。
function presetModelRows(preset: string): ModelEntry[] {
  const ids = PROVIDER_PRESETS[preset]?.models || [];
  return ids.map((id) => ({ ...emptyModel(), id, name: id }));
}

export default function ModelsPage() {
  // 智能体上下文从 URL 自动检测。独立的 /models 页面不在
  // 任何 /agents/<id>/ 路径下，因此钩子返回 "default"，我们
  // 渲染普通用户范围视图。当此组件挂载在智能体设置对话框中
  // （对话者查看共享智能体）时，URL 为 /agents/<id>/...，我们
  // 在此获取 id——继承链随后包含智能体范围的模型和服务商
  // （当所有者启用了 shareModelConfig 时）。
  const urlAgentId = useAgentIdFromURL();
  const inAgentContext = urlAgentId !== "default" && urlAgentId !== "";
  const [agentName, setAgentName] = useState("");
  const [agentScopeModel, setAgentScopeModel] = useState("");
  const [agentShares, setAgentShares] = useState(false);

  const [providers, setProviders] = useState<ProviderEntry[]>([]);
  const [model, setModel] = useState("");
  // 仅系统级的解析结果，来自 /api/config?meta。对 super_admin 来说
  // 始终等于 `model`（他们就是系统）；对普通用户来说，这是清除
  // 其用户级覆盖后将继承的值。仅用于"继承中/覆盖"标记和说明文字。
  const [systemDefault, setSystemDefault] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  // 调用者身份决定此页面读写的范围：
  //   - super_admin → 系统范围（所有用户共享）
  //   - 普通用户 → 用户范围（仅自己可见）
  // 无 UI 切换——需要私有服务商的管理员应在管理角色外配置；
  // 普通用户无法从此处查看系统服务商，因为后端对非管理员
  // 直接拒绝读取请求。
  const [me, setMe] = useState<{ id: string; role: string } | null>(null);
  const isSuperAdmin = me?.role === "super_admin";
  const writeScope: "system" | "user" = isSuperAdmin ? "system" : "user";
  const writeScopeId = isSuperAdmin ? "" : (me?.id || "");

  // 对话框状态
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editingName, setEditingName] = useState<string | null>(null);
  // editingId 用于在管理员的"共享"服务商与用户的"私有"覆盖恰好
  // 同名时消除歧义。find() 辅助函数始终按 id 匹配；
  // editingName 仅用于显示。
  const [editingId, setEditingId] = useState<string | null>(null);
  const [formPreset, setFormPreset] = useState("openrouter");
  const [formName, setFormName] = useState("");
  const [formApiBase, setFormApiBase] = useState("");
  const [formApiKey, setFormApiKey] = useState("");
  const [formApiType, setFormApi] = useState("openai-chat");
  const [formAuthType, setFormAuthType] = useState("api-key");
  const [formModels, setFormModels] = useState<ModelEntry[]>([]);
  // 按模型索引存储的逐模型测试结果。我们测试每个已配置的模型，
  // 让用户看到服务商实际公开哪些模型 ID——单独"ping 基础 URL"
  // 的检查会掩盖单个模型 ID 中的拼写错误。
  type ModelTestResult = { status: "idle" | "testing" | "success" | "error"; error?: string };
  const [modelTests, setModelTests] = useState<Record<number, ModelTestResult>>({});
  const [batchTesting, setBatchTesting] = useState(false);

  // 添加/更新的前提是每个非空模型都有绿色测试结果。空模型行
  // 会被忽略（保存时过滤掉）；"未配置模型"的服务商也允许通过
  // （少见但合法——例如在目录已知之前先播种）。
  const cleanModelRows = formModels
    .map((m, idx) => ({ idx, id: m.id.trim() }))
    .filter((t) => t.id);
  const allModelsPassed =
    cleanModelRows.length === 0 ||
    cleanModelRows.every((t) => modelTests[t.idx]?.status === "success");

  // 收集所有服务商/模型选项用于默认模型下拉菜单。
  // 按 provider/modelId 去重，从内层到外层范围遍历，
  // 使更具体的行在两个同名行（如智能体范围的 "openai" 覆盖
  // 覆盖系统范围的 "openai"）中胜出标签。
  // providers[] 已由 fetchConfig 按 智能体→用户→系统 排序，
  // 因此自然遍历顺序是正确的。
  const allModelOptions: { value: string; label: string }[] = (() => {
    const seen = new Set<string>();
    const out: { value: string; label: string }[] = [];
    for (const p of providers) {
      for (const m of p.models) {
        const value = `${p.name}/${m.id}`;
        if (seen.has(value)) continue;
        seen.add(value);
        out.push({ value, label: `${p.name}/${m.name || m.id}` });
      }
    }
    return out;
  })();

// 服务商存储在 configs 表中，仅通过专用的 /api/providers 端点
  // 读写——POST /api/config 会静默忽略 providers 映射。
  // agents.defaults.model 从合并后的 /api/config 响应中读取
  // （因此会叠加系统+用户层），但通过同一端点写回。保持两次写入
  // 分离，以防空的默认模型字段意外清除服务商行，以及服务商变更
  // 意外清除默认模型。
  const fetchConfig = async (
    asAdmin: boolean,
    userId: string,
  ) => {
    setLoading(true);
    try {
      // 管理员：系统范围是权威数据源。
      // 普通用户：系统（继承）+ 自己的用户范围行。
      // 智能体上下文（对话者查看共享智能体）：还需要拉取智能体
      //   记录 + 智能体范围的服务商。后者依赖于服务端的
      //   shareModelConfig=true 门控；403 仅表示共享已关闭，
      //   此时渲染普通用户范围视图。
      const [cfg, sysRes, userRes, agentRec, agentRes] = await Promise.all([
        getConfig().catch(() => null),
        listProviders("system", "").catch(() => null),
        asAdmin ? Promise.resolve(null) : listProviders("user", userId).catch(() => null),
        inAgentContext ? getAgent(urlAgentId).catch(() => null) : Promise.resolve(null),
        inAgentContext ? listProviders("agent", urlAgentId).catch(() => null) : Promise.resolve(null),
      ]);
      const sysRows: ProviderRow[] = (sysRes && Array.isArray(sysRes.providers))
        ? (sysRes.providers as ProviderRow[])
        : [];
      const userRows: ProviderRow[] = (userRes && Array.isArray(userRes.providers))
        ? (userRes.providers as ProviderRow[])
        : [];
      const agentRows: ProviderRow[] = (agentRes && Array.isArray(agentRes.providers))
        ? (agentRes.providers as ProviderRow[])
        : [];
      const toEntry = (r: ProviderRow, sc: "system" | "user" | "agent"): ProviderEntry => ({
        id: r.id,
        name: r.name,
        apiBase: r.apiBase || "",
        apiKey: "",
        maskedKey: r.apiKey || "",
        apiType: r.apiType || "openai-chat",
        authType: r.authType || "bearer-token",
        models: r.models || [],
        scope: sc,
      });
      // 顺序：智能体（最具体）→ 用户 → 系统。只读的"智能体"行
      // 仅出现在非所有者查看共享智能体时（所有者使用专用的
      // AgentModelsPage 来编辑智能体范围）；管理员跳过它们，
      // 因为管理员的独立 /models 页面仅针对系统范围。
      const entries: ProviderEntry[] = asAdmin
        ? sysRows.map((r) => toEntry(r, "system"))
        : [
            ...agentRows.map((r) => toEntry(r, "agent")),
            ...userRows.map((r) => toEntry(r, "user")),
            ...sysRows.map((r) => toEntry(r, "system")),
          ];
      setProviders(entries);
      setModel(cfg?.agents?.defaults?.model || "");
      setSystemDefault(cfg?.meta?.systemDefaultModel || "");
      const ag = (agentRec as { agent?: { name?: string; model?: string; shareModelConfig?: boolean } } | null)?.agent;
      setAgentName(ag?.name || "");
      setAgentScopeModel(ag?.model || "");
      setAgentShares(!!ag?.shareModelConfig);
    } finally {
      setLoading(false);
    }
  };

  // 先解析身份，再获取配置——管理员获取系统范围数据，
  // 普通用户获取合并结果（系统继承 + 自己的用户范围行）。
  useEffect(() => {
    getMe().then((m) => {
      if (!m?.user) return;
      const meRec = { id: m.user.id, role: m.user.role };
      setMe(meRec);
      fetchConfig(meRec.role === "super_admin", meRec.id);
    });
  }, []);

  const openAddDialog = () => {
    setEditingName(null);
    setEditingId(null);
    setFormPreset("openai");
    setFormName("openai");
    setFormApiBase(PROVIDER_PRESETS["openai"].apiBase);
    setFormApi(PROVIDER_PRESETS["openai"].apiType);
    setFormAuthType(PROVIDER_PRESETS["openai"].authType);
    setFormApiKey("");
    setFormModels(presetModelRows("openai"));
    setModelTests({});
    setDialogOpen(true);
  };

  const openEditDialog = (provider: ProviderEntry) => {
    setEditingName(provider.name);
    setEditingId(provider.id);
    const preset = Object.keys(PROVIDER_PRESETS).includes(provider.name) ? provider.name : "custom";
    setFormPreset(preset);
    setFormName(provider.name);
    setFormApiBase(provider.apiBase);
    setFormApi(provider.apiType);
    setFormAuthType(provider.authType || "bearer-token");
    setFormApiKey("");
    // 已保存的服务商可能包含在我们交付完整 ModelEntry 结构
    // 之前持久化的模型（缺少 cost 块、缺少 input 数组）。
    // 将 emptyModel() 的默认值合并到保存数据之下，确保对话框
    // 始终有结构完整的对象可编辑。
    setFormModels(
      (provider.models || []).map((m) => {
        const base = emptyModel();
        return {
          ...base,
          ...m,
          cost: { ...base.cost, ...(m.cost || {}) },
          input: m.input && m.input.length > 0 ? [...m.input] : base.input,
        };
      }),
    );
    // 编辑会话中将每个模型预标记为"成功"，避免用户必须重新测试
    // 每个现有模型才能修改显示名称。编辑模型 ID、点击测试或
    // 移除/重新添加行会按预期重置其状态。
    setModelTests(
      provider.models
        ? Object.fromEntries(
            provider.models.map((_m, idx) => [idx, { status: "success" as const }]),
          )
        : {},
    );
    setDialogOpen(true);
  };

  // 与 onboard 的 handleProviderChange 保持一致：从预设预填
  // API 地址 + API 类型，保持服务商名称可编辑（自动设为预设键，
  // 但用户可重命名，如 "openai" → "production"），重置测试状态。
  // 切换预设被视为"给出该服务商的全新起点"——与覆盖 apiBase/apiType
  // 一样，它也会用预设的已知模型 ID 刷新模型列表。编辑模式
  // （openEditDialog）直接加载已保存的模型，不经过此路径，
  // 因此用户保存的配置不会被覆盖。
  const handlePresetChange = (preset: string) => {
    setFormPreset(preset);
    const cfg = PROVIDER_PRESETS[preset];
    if (cfg) {
      setFormApiBase(cfg.apiBase);
      setFormApi(cfg.apiType);
      setFormAuthType(cfg.authType);
    }
    setFormName(preset === "custom" ? "" : preset);
    setFormModels(presetModelRows(preset));
    setModelTests({});
  };

  // 逐个测试每个已配置的模型。针对每个模型 ID
  // 发送一次 /chat/completions（或 /v1/messages）请求，
  // 使任何单个模型的拼写错误都能独立暴露——单独以 model=""
  // "ping" 可能返回 200，但仍会让错误的模型 ID 静默漏过。
  //
  // 编辑已有行时，用户通常没有重新输入 API 密钥（字段为空 +
  // 仅显示遮蔽值），因此我们走服务端已保存行进行测试。
  // 如果他们确实输入了新密钥，我们优先使用它并走内联测试路径，
  // 以便在保存前验证新密钥。
  const handleTestConnection = async () => {
    const targets = formModels
      .map((m, idx) => ({ idx, id: m.id.trim() }))
      .filter((t) => t.id);
    if (targets.length === 0) return;
    const editingRow = editingId
      ? providers.find((p) => p.id === editingId)
      : undefined;
    const useStoredKey = !!editingRow && !formApiKey.trim();
    setBatchTesting(true);
    setModelTests((prev) => {
      const next = { ...prev };
      for (const t of targets) next[t.idx] = { status: "testing" };
      return next;
    });
    await Promise.all(
      targets.map(async ({ idx, id }) => {
        try {
          const result = useStoredKey && editingRow
            ? await testStoredProvider(editingRow.id, id, {
                apiBase: formApiBase,
                apiType: formApiType,
                authType: formAuthType,
              })
            : await testProvider({
                apiBase: formApiBase,
                apiKey: formApiKey,
                model: id,
                apiType: formApiType,
                authType: formAuthType,
              });
          setModelTests((prev) => ({
            ...prev,
            [idx]: result.ok
              ? { status: "success" }
              : { status: "error", error: result.error || "连接失败" },
          }));
        } catch {
          setModelTests((prev) => ({
            ...prev,
            [idx]: { status: "error", error: "连接失败" },
          }));
        }
      }),
    );
    setBatchTesting(false);
  };

  const handleAddModel = () => {
    setFormModels((prev) => [...prev, emptyModel()]);
  };

  const handleUpdateModel = (index: number, field: string, value: unknown) => {
    setFormModels((prev) => {
      const updated = [...prev];
      const m = { ...updated[index], cost: { ...updated[index].cost }, input: [...updated[index].input] };
      if (field === "id") m.id = value as string;
      else if (field === "name") m.name = value as string;
      else if (field === "reasoning") m.reasoning = value as boolean;
      else if (field === "contextWindow") m.contextWindow = Number(value) || 0;
      else if (field === "maxTokens") m.maxTokens = Number(value) || 0;
      updated[index] = m;
      return updated;
    });
    if (field === "id") {
      // 编辑模型 ID 会使该行之前的测试结果失效——清除标记，
      // 避免拼写错误后仍显示过期的"已连接"标记产生误导。
      setModelTests((prev) => {
        if (prev[index] === undefined) return prev;
        const { [index]: _drop, ...rest } = prev;
        void _drop;
        return rest;
      });
    }
  };

  const handleRemoveModel = (index: number) => {
    setFormModels((prev) => prev.filter((_, i) => i !== index));
    // 重新索引 modelTests：移除索引之后的行下移 1 位。
    setModelTests((prev) => {
      const next: Record<number, ModelTestResult> = {};
      for (const [k, v] of Object.entries(prev)) {
        const i = Number(k);
        if (i === index) continue;
        next[i > index ? i - 1 : i] = v;
      }
      return next;
    });
  };

  const flashSaved = () => {
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  };

  const handleSaveProvider = async () => {
    const name = formName.toLowerCase().trim().replace(/\s+/g, "-");
    if (!name) return;
    const cleanedModels = formModels.filter((m) => m.id.trim());
    const editingRow = editingId
      ? providers.find((p) => p.id === editingId)
      : undefined;

    setSaving(true);
    try {
      if (editingRow) {
        await updateProvider(editingRow.id, {
          apiBase: formApiBase,
          // 编辑时空密钥表示"保留现有值"；后端将空值/遮蔽哨兵值
          // 视为无操作。
          apiKey: formApiKey || undefined,
          apiType: formApiType,
          authType: formAuthType,
          models: cleanedModels,
        });
      } else {
        await createProvider({
          scope: writeScope,
          scopeId: writeScopeId,
          name,
          apiBase: formApiBase,
          apiKey: formApiKey,
          apiType: formApiType,
          authType: formAuthType,
          models: cleanedModels,
        });
      }
      flashSaved();
    } finally {
      setSaving(false);
    }
    setDialogOpen(false);
    await fetchConfig(isSuperAdmin, me?.id || "");
  };

  const handleDeleteProvider = async (row: ProviderEntry) => {
    setSaving(true);
    try {
      await deleteProvider(row.id);
      flashSaved();
    } finally {
      setSaving(false);
    }
    await fetchConfig(isSuperAdmin, me?.id || "");
  };

  // 顶部的保存按钮持久化默认模型设置。空值是合法意图
  // （"清除默认值"）——后端的 AgentDefaults.Model 上的
  // `omitempty` 会从保存行中删除该键而不干扰兄弟字段，
  // 因此发送空值是安全的。
  const handleSaveAll = async () => {
    setSaving(true);
    try {
      await updateConfig({ agents: { defaults: { model: model.trim() } } });
      flashSaved();
      await fetchConfig(isSuperAdmin, me?.id || "");
    } finally {
      setSaving(false);
    }
  };

  const handleDefaultModelChange = async (value: string) => {
    setModel(value);
    if (!value.trim()) return;
    setSaving(true);
    try {
      await updateConfig({ agents: { defaults: { model: value.trim() } } });
      flashSaved();
      // 刷新以使"继承中/覆盖"标记反映新状态。
      await fetchConfig(isSuperAdmin, me?.id || "");
    } finally {
      setSaving(false);
    }
  };

  // 清除用户范围的 agents.defaults.model 覆盖，使智能体运行时
  // 回退到系统默认值。写入空字符串只会将 "" 存到用户范围，
  // 这在合并时仍会优先——因此我们发送 null/undefined，
  // 后端将其视为"删除行"。
  const handleClearOverride = async () => {
    setSaving(true);
    try {
      await updateConfig({ agents: { defaults: { model: "" } } });
      flashSaved();
      await fetchConfig(isSuperAdmin, me?.id || "");
    } finally {
      setSaving(false);
    }
  };

  if (loading) {
    return (
      <div className="p-6 space-y-6 max-w-5xl mx-auto">
        <Skeleton className="h-10 w-48" />
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {[1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-48" />
          ))}
        </div>
      </div>
    );
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">模型</h2>
          <p className="text-sm text-muted-foreground mt-1">
            {inAgentContext ? (
              <>
                你的模型和服务商，作用于{" "}
                <strong>{agentName || "此智能体"}</strong>。你的覆盖设置优先于智能体默认值。
              </>
            ) : (
              <>管理大模型服务商和默认模型</>
            )}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" onClick={openAddDialog}>
            <Plus className="h-4 w-4 mr-2" />
            添加服务商
          </Button>
          <Button
            onClick={handleSaveAll}
            disabled={saving}
            variant={saved ? "outline" : "default"}
            className={saved ? "border-emerald-500/30 text-emerald-600 dark:text-emerald-400" : ""}
          >
            {saved ? (
              <>
                <Check className="h-4 w-4 mr-2" />
                已保存
              </>
            ) : (
              saving ? "正在保存..." : "保存"
            )}
          </Button>
        </div>
      </div>

{/* 默认模型——对非管理员我们与智能体模型页面一样展示继承状态，
            使用户能看到免费继承的值和自己覆盖了什么。对 super_admin
            来说，他们是系统权威来源，标记只会是噪音。
            在智能体上下文中，继承链变为 对话者用户→智能体范围→系统，
            因此共享开启时在占位符和说明文字中显示智能体模型。 */}
      {(() => {
        const inheriting = !isSuperAdmin && !model.trim();
        const overridden = !isSuperAdmin && !inheriting;
// 对话者没有覆盖时运行时实际使用的模型。EnsureAgent
          // 优先选择智能体范围（仅当所有者启用了共享时）；
          // 否则回退到系统默认值。
        const effectiveFallback = inAgentContext && agentShares && agentScopeModel
          ? agentScopeModel
          : systemDefault;
        const fallbackSource = inAgentContext && agentShares && agentScopeModel
          ? "agent"
          : "system";
        return (
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-center justify-between gap-2 mb-3">
          <div className="flex items-center gap-2">
            <Cpu className="h-4 w-4 text-primary" />
            <h3 className="font-medium">
              {inAgentContext ? "当前模型" : "默认模型"}
            </h3>
            {!isSuperAdmin && (inheriting ? (
              <Badge variant="outline" className="text-[10px]">继承中</Badge>
            ) : (
              <Badge className="bg-primary/10 text-primary hover:bg-primary/10 text-[10px]">覆盖</Badge>
            ))}
          </div>
          {overridden && (
            <Button
              variant="ghost"
              size="sm"
              className="h-7 text-xs"
              onClick={handleClearOverride}
              disabled={saving}
            >
              清除覆盖
            </Button>
          )}
        </div>
        {allModelOptions.length > 0 ? (
          <Select value={inheriting ? "" : model} onValueChange={(v: string | null) => v && handleDefaultModelChange(v)}>
            <SelectTrigger className="font-mono text-sm max-w-md">
              <SelectValue placeholder={inheriting ? `继承（${effectiveFallback || "无默认值"}）` : "选择模型"} />
            </SelectTrigger>
{/* 默认 Select 的 `w-(--anchor-width)` 将弹出层锁定为
                  触发器的 max-w-md。像 openrouter/xiaomi/mimo-v2-flash
                  这样的长 ID 会被截断。改为允许内容自适应宽度，
                  同时保持最小宽度等于触发器。 */}
            <SelectContent className="!w-auto !min-w-[var(--anchor-width)] !overflow-x-visible">
              {allModelOptions.map((opt) => (
                <SelectItem key={opt.value} value={opt.value}>
                  <span className="font-mono text-sm whitespace-nowrap">{opt.value}</span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        ) : (
          <Input
            value={inheriting ? "" : model}
            onChange={(e) => setModel(e.target.value)}
            placeholder={inheriting ? (effectiveFallback ? `继承（${effectiveFallback}）` : "例如 openai/gpt-4o") : "例如 openai/gpt-4o"}
            className="font-mono text-sm max-w-md"
          />
        )}
        <p className="text-xs text-muted-foreground mt-2">
          {isSuperAdmin ? (
            <>除非在智能体配置中覆盖，否则智能体将使用此设置。</>
          ) : inheriting ? (
            <>
              {fallbackSource === "agent" ? (
                <>
                  <strong>{agentName || "此智能体"}</strong> 使用{" "}
                  <code className="text-[11px]">{effectiveFallback}</code> （继承自智能体）。可在上方选择模型，仅为你自己覆盖此设置。
                </>
              ) : (
                <>
                  使用系统默认值
                  {effectiveFallback ? (
                    <>: <code className="text-[11px]">{effectiveFallback}</code></>
                  ) : (
                    <> （尚未配置）</>
                  )}
                  。在上方选择模型以覆盖默认值
                  {inAgentContext ? <> 用于此智能体。</> : <> 仅用于你的智能体。</>}
                </>
              )}
            </>
          ) : (
            <>
              覆盖设置作用于 {inAgentContext ? <strong>你</strong> : <>你的智能体</>}。格式为{" "}
              <code className="text-[11px]">provider/modelId</code>。
            </>
          )}
        </p>
      </div>
        );
      })()}

      {/* 服务商网格 */}
      {providers.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-amber-500/10 mb-4">
              <Brain className="h-7 w-7 text-amber-500" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">尚未配置服务商</p>
            <p className="text-xs text-muted-foreground/60 mb-4">
              添加大模型服务商以开始使用
            </p>
            <Button variant="outline" size="sm" onClick={openAddDialog}>
              <Plus className="h-4 w-4 mr-2" />
              添加服务商
            </Button>
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-border bg-card overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>名称</TableHead>
                <TableHead>API 基础地址</TableHead>
                <TableHead>API 密钥</TableHead>
                <TableHead>模型</TableHead>
                <TableHead>来源</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {providers.map((provider) => {
                // 行的可编辑性取决于其是否在调用者自己的范围内。
                // 对 super_admin 来说是 "system"；对其他人是 "user"。
                // 为共享智能体上的对话者展示的 "agent" 行始终只读
                // （所有者拥有它们）。
                const editable = isSuperAdmin
                  ? provider.scope === "system"
                  : provider.scope === "user";
                const sourceLabel =
                  provider.scope === "agent"
                    ? "继承自智能体"
                    : editable
                      ? "我的"
                      : "已继承";
                const sourceTitle =
                  provider.scope === "agent"
                    ? "由所有者在此智能体上配置，并共享给对话用户。"
                    : editable
                      ? ""
                      : "由管理员配置，并共享给所有用户。";
                return (
                <TableRow key={`${provider.scope}:${provider.id}`}>
                  <TableCell className="font-medium">{provider.name}</TableCell>
                  <TableCell>
                    <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">
                      {provider.apiBase || "—"}
                    </code>
                  </TableCell>
                  <TableCell>
                    <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">
                      {provider.maskedKey || "—"}
                    </code>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {provider.models.length}
                  </TableCell>
                  <TableCell>
                    {editable ? (
                      <Badge
                        variant="outline"
                        className="bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 border-emerald-500/20"
                      >
                        {sourceLabel}
                      </Badge>
                    ) : (
                      <Badge variant="outline" className="text-muted-foreground" title={sourceTitle}>
                        {sourceLabel}
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button
                        size="icon"
                        variant="ghost"
                        onClick={() => openEditDialog(provider)}
                        title={editable ? "编辑" : "只读：继承的配置"}
                        disabled={!editable}
                      >
                        <Pencil className="size-4" />
                      </Button>
                      <Button
                        size="icon"
                        variant="ghost"
                        className="text-destructive hover:text-destructive"
                        onClick={() => handleDeleteProvider(provider)}
                        title={editable ? "移除" : "只读：继承的配置"}
                        disabled={!editable}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              );})}
            </TableBody>
          </Table>
        </div>
      )}

      {/* 添加/编辑服务商对话框 */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-2xl max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>
              {editingName ? "编辑 服务商" : "添加服务商"}
            </DialogTitle>
            <DialogDescription>
              配置大模型服务商连接和模型
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            {/* 服务商 + 服务商名称（与 onboard 的两列网格对应）。 */}
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>服务商</Label>
                <Select
                  value={formPreset}
                  onValueChange={(v: string | null) => v && handlePresetChange(v)}
                  disabled={!!editingName}
                >
                  <SelectTrigger className="w-full">
                    <SelectValue>
                      {(v: unknown) => PROVIDER_LABELS[v as string] ?? (v as string) ?? ""}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {Object.keys(PROVIDER_PRESETS).map((p) => (
                      <SelectItem key={p} value={p}>
                        {PROVIDER_LABELS[p] ?? p}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>服务商名称</Label>
                <Input
                  value={formName}
                  onChange={(e) => setFormName(e.target.value)}
                  placeholder="openai"
                  className="font-mono text-sm"
                  disabled={!!editingName}
                />
              </div>
            </div>

            {/* API 基础地址 */}
            <div className="space-y-1.5">
              <Label>API 基础地址</Label>
              <Input
                value={formApiBase}
                onChange={(e) => setFormApiBase(e.target.value)}
                placeholder="https://api.openai.com/v1"
                className="font-mono text-sm"
              />
            </div>

            {/* API 密钥——编辑时我们从不接收未遮蔽的密钥，
                因此显示遮蔽密钥作为占位符，让操作者看到已配置了密钥。
                留空可让用户输入替换值；字段为空时测试连接
                会回退到已保存的密钥。 */}
            <div className="space-y-1.5">
              <Label>API 密钥</Label>
              <Input
                type={editingName && !formApiKey ? "text" : "password"}
                value={formApiKey}
                onChange={(e) => setFormApiKey(e.target.value)}
                placeholder={
                  editingName
                    ? (() => {
                        const row = providers.find((p) => p.id === editingId);
                        return row?.maskedKey || "sk-…";
                      })()
                    : "sk-…"
                }
                className="font-mono text-sm placeholder:text-muted-foreground/70"
              />
              {editingName && (
                <p className="text-[11px] text-muted-foreground/60">
                  留空可保留现有密钥。 连接测试会使用已保存的密钥。
                </p>
              )}
            </div>

            {/* API 类型与认证类型 */}
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>API 类型</Label>
                <Select value={formApiType} onValueChange={(v: string | null) => v && setFormApi(v)}>
                  <SelectTrigger className="w-full">
                    <SelectValue>
                      {(v: unknown) => API_TYPE_LABELS[v as string] ?? (v as string) ?? ""}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="openai-chat">OpenAI 聊天补全</SelectItem>
                    <SelectItem value="anthropic-messages">Anthropic 消息</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>认证类型</Label>
                <Select value={formAuthType} onValueChange={(v: string | null) => v && setFormAuthType(v)}>
                  <SelectTrigger className="w-full">
                    <SelectValue>
                      {(v: unknown) => AUTH_TYPE_LABELS[v as string] ?? (v as string) ?? ""}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="bearer-token">Bearer 令牌</SelectItem>
                    <SelectItem value="api-key">API 密钥请求头</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>

            {/* 模型部分 */}
            <div className="space-y-3 pt-2 border-t border-border">
              <div className="flex items-center justify-between">
                <Label className="text-base">模型</Label>
                <Button variant="outline" size="sm" onClick={handleAddModel}>
                  <Plus className="h-3 w-3 mr-1.5" />
                  添加模型
                </Button>
              </div>

              {formModels.length === 0 && (
                <p className="text-sm text-muted-foreground/60 text-center py-4">
                  尚未配置模型。请添加此服务商可用的模型。
                </p>
              )}

              {formModels.map((m, idx) => {
                const t = modelTests[idx];
                return (
                <div key={idx} className="rounded-lg border border-border bg-muted/30 p-4 space-y-3">
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex items-center gap-2 min-w-0">
                      <span className="text-sm font-medium text-muted-foreground">
                        模型 {idx + 1}
                      </span>
                      {t?.status === "testing" && (
                        <Badge variant="outline" className="text-[10px]">
                          <Loader2 className="mr-1 size-3 animate-spin" /> 正在测试
                        </Badge>
                      )}
                      {t?.status === "success" && (
                        <Badge className="bg-emerald-500/15 text-emerald-700 hover:bg-emerald-500/15 text-[10px]">
                          <Check className="mr-1 size-3" /> 已连接
                        </Badge>
                      )}
                      {t?.status === "error" && (
                        <Badge variant="outline" className="border-destructive/40 text-destructive text-[10px]" title={t.error}>
                          失败
                        </Badge>
                      )}
                    </div>
                    <Button
                      variant="ghost"
                      size="sm"
                      className="h-7 text-xs text-destructive hover:text-destructive"
                      onClick={() => handleRemoveModel(idx)}
                    >
                      <Trash2 className="h-3 w-3 mr-1" />
                      移除
                    </Button>
                  </div>
                  <div className="grid grid-cols-2 gap-3">
                    <div className="space-y-1">
                      <Label className="text-xs">模型 ID</Label>
                      <Input
                        value={m.id}
                        onChange={(e) => handleUpdateModel(idx, "id", e.target.value)}
                        placeholder="例如 gpt-4o"
                        className="font-mono text-xs h-8"
                      />
                    </div>
                    <div className="space-y-1">
                      <Label className="text-xs">显示名称</Label>
                      <Input
                        value={m.name}
                        onChange={(e) => handleUpdateModel(idx, "name", e.target.value)}
                        placeholder="例如 GPT-4o"
                        className="text-xs h-8"
                      />
                    </div>
                  </div>
                </div>
                );
              })}

              {/* 测试连接会对每个已配置的模型逐一运行，因此任何单个
                  模型 ID 中的拼写错误会按行暴露，而非隐藏在单一的
                  绿色通过/失败背后。始终可见——添加/更新的前提是
                  每个模型都通过测试。 */}
              <div className="flex flex-col gap-2 pt-2">
                <div className="flex items-center gap-3">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={handleTestConnection}
                    disabled={
                      batchTesting ||
                      !formApiBase ||
                      cleanModelRows.length === 0
                    }
                  >
                    {batchTesting ? (
                      <>
                        <Loader2 className="mr-1 size-4 animate-spin" /> 正在测试
                      </>
                    ) : (
                      "测试连接"
                    )}
                  </Button>
                  <span className="text-xs text-muted-foreground">
                    {cleanModelRows.length === 0
                      ? "请至少添加一个带 ID 的模型，然后进行测试。"
                      : "测试上方所有模型，结果会显示在对应行旁。"}
                  </span>
                </div>
                {Object.values(modelTests).some((t) => t.status === "error") && (
                  <ul className="space-y-0.5">
                    {formModels.map((m, idx) => {
                      const t = modelTests[idx];
                      if (!t || t.status !== "error" || !m.id.trim()) return null;
                      return (
                        <li key={idx} className="text-xs text-destructive break-all">
                          <code className="font-mono">{m.id}</code>: {t.error}
                        </li>
                      );
                    })}
                  </ul>
                )}
              </div>
            </div>
          </div>
          <DialogFooter className="flex flex-col gap-2 sm:flex-row sm:items-center">
            {!allModelsPassed && (
              <span className="text-xs text-muted-foreground sm:mr-auto">
                请先测试所有模型，全部通过后才能添加或更新。
              </span>
            )}
            <Button variant="outline" onClick={() => setDialogOpen(false)}>
              取消
            </Button>
            <Button
              onClick={handleSaveProvider}
              disabled={!formName.trim() || saving || !allModelsPassed}
            >
              {editingName ? "更新" : "添加"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
