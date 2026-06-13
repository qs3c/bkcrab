"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
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
import { Switch } from "@/components/ui/switch";
import { Brain, Plus, Pencil, Trash2, Check, Cpu, Loader2, Share2 } from "lucide-react";
import {
  getAgent,
  getConfig,
  listProviders,
  createProvider,
  updateProvider,
  deleteProvider,
  testProvider,
  testStoredProvider,
  updateAgent,
  type ModelEntry,
  type ProviderRow,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

// 每个智能体的模型页面——与管理员 /models 页面相同的 UI/UX，
// 但限定到单个智能体。读取/写入智能体范围的 Provider 行
//（`scope=agent&scopeId=<agentId>`）及智能体自身的模型覆盖。
//
// 运行时优先级（参见 internal/gateway/userspace.go）：
//   - 智能体范围的 Provider 按名称遮蔽系统 Provider。
//   - 智能体范围的 `agents.defaults.model` 覆盖系统默认值。
// 此处空覆盖 => 继承系统默认值。

// `models` 是选择预设时预填充到表单中的常见模型 ID。
// 用户可以保留、编辑或移除它们。空列表表示"无合理默认值"
//（custom/openrouter/ollama 变化太大，不适合提供内置建议）。
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
  id: string;          // 配置行 ID — PUT/DELETE 必需
  name: string;
  apiBase: string;
  apiKey: string;      // 未遮罩的草稿（仅在编辑时设置）
  maskedKey: string;   // 服务端返回的遮罩密钥，用于显示
  apiType: string;
  authType: string;
  models: ModelEntry[];
  // 继承来源。仅 "agent" 行可在此页面编辑；
  // "user" 和 "system" 行是运行时解析链的只读视图。
  // 不同作用域可以有两个同名行（低作用域遮蔽高作用域）——
  // 通过 id 查找避免了旧版按名称查找的冲突问题。
  scope: "agent" | "user" | "system";
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
// 使对话框打开时预填充常见模型而非空列表。
function presetModelRows(preset: string): ModelEntry[] {
  const ids = PROVIDER_PRESETS[preset]?.models || [];
  return ids.map((id) => ({ ...emptyModel(), id, name: id }));
}

export default function AgentModelsPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);

  const [providers, setProviders] = useState<ProviderEntry[]>([]);
  const [model, setModel] = useState("");
  const [systemDefault, setSystemDefault] = useState("");
  const [systemProviders, setSystemProviders] = useState<string[]>([]);
  // 默认为 true，使开关在 fetchAll 解析前的短暂窗口内反映开启状态。
  // 后端将缺少的键视为开启（见 agentShareModelConfig in handlers_agents.go）
  // ——保持这两处对齐。
  const [shareModelConfig, setShareModelConfig] = useState(true);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  // 对话框状态——与管理员页面完全一致。
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editingName, setEditingName] = useState<string | null>(null);
  // editingId：合并视图中，不同作用域的两行可以共享 `name`
  //（例如智能体的 "openai" 覆盖 + 系统的 "openai"）。
  // 编辑/测试的查找必须使用 id。
  const [editingId, setEditingId] = useState<string | null>(null);
  const [formPreset, setFormPreset] = useState("openrouter");
  const [formName, setFormName] = useState("");
  const [formApiBase, setFormApiBase] = useState("");
  const [formApiKey, setFormApiKey] = useState("");
  const [formApiType, setFormApi] = useState("openai-chat");
  const [formAuthType, setFormAuthType] = useState("api-key");
  const [formModels, setFormModels] = useState<ModelEntry[]>([]);
  type ModelTestResult = { status: "idle" | "testing" | "success" | "error"; error?: string };
  const [modelTests, setModelTests] = useState<Record<number, ModelTestResult>>({});
  const [batchTesting, setBatchTesting] = useState(false);

  const cleanModelRows = formModels
    .map((m, idx) => ({ idx, id: m.id.trim() }))
    .filter((t) => t.id);
  const allModelsPassed =
    cleanModelRows.length === 0 ||
    cleanModelRows.every((t) => modelTests[t.idx]?.status === "success");

  // 下拉列表展示智能体在运行时将解析到的所有作用域中的模型——
  // 智能体覆盖遮蔽用户，用户覆盖遮蔽系统。我们在
  // `provider/modelId` 上去重，以免同名覆盖显示两次；
  // 低作用域行优先（agent > user > system），因为它是
  // 实际会被选择的。
  const allModelOptions: { value: string; label: string }[] = useMemo(() => {
    const seen = new Set<string>();
    const order: ProviderEntry["scope"][] = ["agent", "user", "system"];
    const out: { value: string; label: string }[] = [];
    for (const sc of order) {
      for (const p of providers) {
        if (p.scope !== sc) continue;
        for (const m of p.models) {
          const value = `${p.name}/${m.id}`;
          if (seen.has(value)) continue;
          seen.add(value);
          out.push({ value, label: `${p.name}/${m.name || m.id}` });
        }
      }
    }
    return out;
  }, [providers]);

  const fetchAll = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    try {
      // 需要智能体的所有者来获取用户作用域的继承行。
      // 并行拉取智能体记录 + 三种 Provider 作用域。用户作用域
      // 调用仅在 agentRec 解析后才绑定到所有者；在此惰性获取
      // 用户列表使结构扁平，避免尴尬的两阶段获取。
      const [agentRec, agentScopeRes, sysScopeRes, cfg] = await Promise.all([
        getAgent(agentId).catch(() => null),
        listProviders("agent", agentId).catch(() => null),
        listProviders("system", "").catch(() => null),
        // /api/config 可能对非管理员返回 403；如果如此，我们只是丢失
        // "继承系统默认值: X" 提示，这没什么影响。
        getConfig().catch(() => null),
      ]);
      const ownerId = agentRec?.userId || "";
      // 用户作用域继承仅在知道所有者时适用；匿名穿透意味着
      // 没有用户层（罕见——引导后不应存在无所有者的智能体）。
      const userScopeRes = ownerId
        ? await listProviders("user", ownerId).catch(() => null)
        : null;

      const toRows = (res: { providers?: ProviderRow[] } | null): ProviderRow[] =>
        res && Array.isArray(res.providers) ? (res.providers as ProviderRow[]) : [];
      const toEntry = (r: ProviderRow, sc: ProviderEntry["scope"]): ProviderEntry => ({
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
      const merged: ProviderEntry[] = [
        ...toRows(agentScopeRes).map((r) => toEntry(r, "agent")),
        ...toRows(userScopeRes).map((r) => toEntry(r, "user")),
        ...toRows(sysScopeRes).map((r) => toEntry(r, "system")),
      ];
      setProviders(merged);
      setSystemDefault(cfg?.agents?.defaults?.model || "");
      setSystemProviders(toRows(sysScopeRes).map((r) => r.name));
      // 智能体自身的模型覆盖已在服务端由 handleGetAgent → agentScopeModel
      // 解析（scope=agent, name=agents.defaults 的配置行）。从 agentRec
      // 读取可保持此页面与应用其余部分同步——`cfg.agents.list` 是每智能体
      // 覆盖移出合并配置之前的过时 TS 类型；Go 端不会填充它。
      setModel(agentRec?.model || "");
      // 后端总是发出确定的布尔值（见 agentShareModelConfig）；
      // ?? 防护页面在二进制升级落地前被访问时的过时结构。
      setShareModelConfig(agentRec?.shareModelConfig ?? true);
    } finally {
      setLoading(false);
    }
  }, [agentId]);

  useEffect(() => {
    fetchAll();
  }, [fetchAll]);

  const flashSaved = () => {
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  };

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
    setModelTests(
      provider.models
        ? Object.fromEntries(
            provider.models.map((_m, idx) => [idx, { status: "success" as const }]),
          )
        : {},
    );
    setDialogOpen(true);
  };

  // 预设切换被视为"给我该 Provider 的全新起点"——与覆盖
  // apiBase/apiType 相同，它也会用预设的已知模型 ID 刷新
  // 模型列表。编辑模式（openEditDialog）直接加载存储的模型
  // 且不会经过此路径，因此用户保存的配置永远不会被覆盖。
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

  const handleSaveProvider = async () => {
    if (!agentId) return;
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
          apiKey: formApiKey || undefined,
          apiType: formApiType,
          authType: formAuthType,
          models: cleanedModels,
        });
      } else {
        await createProvider({
          scope: "agent",
          scopeId: agentId,
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
    await fetchAll();
  };

  const handleDeleteProvider = async (row: ProviderEntry) => {
    setSaving(true);
    try {
      await deleteProvider(row.id);
      // 如果当前模型来自此 Provider，覆盖现在悬空了——清除它
      // 以便智能体在运行时通过链回退。
      if (model.startsWith(`${row.name}/`)) {
        await updateAgent(agentId, { model: "" });
      }
      flashSaved();
    } finally {
      setSaving(false);
    }
    await fetchAll();
  };

  const handleModelChange = async (value: string) => {
    setModel(value);
    setSaving(true);
    try {
      // 空字符串表示"清除覆盖 → 继承系统默认值"。
      await updateAgent(agentId, { model: value });
      flashSaved();
    } finally {
      setSaving(false);
    }
  };

  const handleClearOverride = async () => {
    setModel("");
    setSaving(true);
    try {
      await updateAgent(agentId, { model: "" });
      flashSaved();
    } finally {
      setSaving(false);
    }
  };

  // 乐观更新——立即翻转 UI，然后持久化。失败时回滚。
  // 服务端的 invalidateAgent 会丢弃所有惰性挂载此智能体的
  // UserSpace，使聊天者在下一条消息时看到新的门控，
  // 无需进程重启。
  const handleShareToggle = async (next: boolean) => {
    const prev = shareModelConfig;
    setShareModelConfig(next);
    setSaving(true);
    try {
      await updateAgent(agentId, { shareModelConfig: next });
      flashSaved();
    } catch {
      setShareModelConfig(prev);
    } finally {
      setSaving(false);
    }
  };

  if (loading) {
    return (
      <div className="p-6 space-y-6 max-w-5xl mx-auto">
        <Skeleton className="h-10 w-48" />
        <Skeleton className="h-32 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  const inheriting = !model.trim();

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">模型</h2>
          <p className="text-sm text-muted-foreground mt-1">
            以下范围的大模型服务商和当前模型：{" "}
            <strong>{agentName || "此智能体"}</strong>。智能体级设置会覆盖系统默认值。
          </p>
        </div>
        <div className="flex items-center gap-2">
          {saved && (
            <span className="inline-flex items-center gap-1.5 text-xs text-emerald-600 dark:text-emerald-400 mr-2">
              <Check className="h-3.5 w-3.5" /> 已保存
            </span>
          )}
          <Button variant="outline" onClick={openAddDialog} disabled={saving}>
            <Plus className="h-4 w-4 mr-2" />
            添加服务商
          </Button>
        </div>
      </div>

      {/* 与聊天者共享模型配置 */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3 min-w-0">
            <Share2 className="h-4 w-4 text-primary mt-0.5 shrink-0" />
            <div className="min-w-0">
              <h3 className="font-medium">与聊天者共享模型配置</h3>
              <p className="text-sm text-muted-foreground mt-1">
                {shareModelConfig ? (
                  <>
                    使用此智能体的聊天者： <strong>{agentName || "此智能体"}</strong>{" "}
                    将继承你的模型和服务商凭据，他们的消息会消耗你的令牌。
                  </>
                ) : (
                  <>
                    仅你使用此配置。聊天者需要在以下位置配置自己的模型和服务商： <em>用户 → 模型</em>，否则智能体将回退到系统默认值。
                  </>
                )}
              </p>
            </div>
          </div>
          <Switch
            checked={shareModelConfig}
            onCheckedChange={handleShareToggle}
            disabled={saving}
            aria-label="与聊天者共享模型配置"
          />
        </div>
      </div>

      {/* 当前模型 */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-center justify-between gap-2 mb-3">
          <div className="flex items-center gap-2">
            <Cpu className="h-4 w-4 text-primary" />
            <h3 className="font-medium">当前模型</h3>
            {inheriting ? (
              <Badge variant="outline" className="text-[10px]">
                继承中
              </Badge>
            ) : (
              <Badge className="bg-primary/10 text-primary hover:bg-primary/10 text-[10px]">
                覆盖
              </Badge>
            )}
          </div>
          {!inheriting && (
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
          <Select
            value={model}
            onValueChange={(v: string | null) => v && handleModelChange(v)}
            disabled={saving}
          >
            <SelectTrigger className="font-mono text-sm max-w-md">
              <SelectValue placeholder={inheriting ? `继承（${systemDefault || "无系统默认值"}）` : "选择模型"} />
            </SelectTrigger>
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
            value={model}
            onChange={(e) => setModel(e.target.value)}
            onBlur={() => handleModelChange(model)}
            placeholder={systemDefault ? `继承（${systemDefault}）` : "请在下方添加包含模型的服务商"}
            className="font-mono text-sm max-w-md"
          />
        )}
        <p className="text-xs text-muted-foreground mt-2">
          {inheriting ? (
            <>
              使用系统默认值
              {systemDefault ? (
                <>
                  : <code className="text-[11px]">{systemDefault}</code>
                </>
              ) : (
                <> （尚未配置）</>
              )}
              。在上方选择模型，为以下对象覆盖默认值：{" "}
              <strong>{agentName || "此智能体"}</strong>。
            </>
          ) : (
            <>
              覆盖设置作用于 <strong>{agentName || "此智能体"}</strong>{" "}
              。格式为 <code className="text-[11px]">provider/modelId</code>。
              {systemDefault && (
                <>
                  {" "}
                  清除后将回退到{" "}
                  <code className="text-[11px]">{systemDefault}</code>.
                </>
              )}
            </>
          )}
        </p>
      </div>

      {/* 服务商表格 */}
      {providers.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-amber-500/10 mb-4">
              <Brain className="h-7 w-7 text-amber-500" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">
              暂无可用服务商
            </p>
            <p className="text-xs text-muted-foreground/60 mb-4 max-w-md text-center">
              尚未配置智能体、用户或系统级服务商。请在此添加服务商，为此智能体提供凭据，或在顶层“模型”页面配置共享服务商。
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
                const editable = provider.scope === "agent";
                const sourceLabel =
                  provider.scope === "agent"
                    ? "我的（智能体）"
                    : provider.scope === "user"
                    ? "继承自所有者"
                    : "继承自管理员";
                return (
                <TableRow key={`${provider.scope}:${provider.id}`}>
                  <TableCell className="font-medium">
                    <div className="flex items-center gap-2">
                      {provider.name}
                      {editable && systemProviders.includes(provider.name) && (
                        <Badge variant="outline" className="text-[10px]">
                          覆盖系统配置
                        </Badge>
                      )}
                    </div>
                  </TableCell>
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
                      <Badge variant="outline" className="text-muted-foreground" title="只读：此配置归所有者或管理员管理">
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
              配置作用于以下范围的大模型服务商：{" "}
              <strong>{agentName || "此智能体"}</strong>。使用与系统服务商相同的名称可覆盖它。
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
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

            <div className="space-y-1.5">
              <Label>API 基础地址</Label>
              <Input
                value={formApiBase}
                onChange={(e) => setFormApiBase(e.target.value)}
                placeholder="https://api.openai.com/v1"
                className="font-mono text-sm"
              />
            </div>

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
