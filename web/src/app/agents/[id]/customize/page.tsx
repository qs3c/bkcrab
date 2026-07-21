"use client";

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Save, Check, Loader2, RotateCcw } from "lucide-react";
import { apiFetch } from "@/lib/api";

import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

const CUSTOMIZE_FILES = [
  { name: "SOUL.md", label: "Soul" },
  { name: "IDENTITY.md", label: "Identity" },
  { name: "USER.md", label: "用户" },
  { name: "TOOLS.md", label: "工具" },
  { name: "BOOTSTRAP.md", label: "Bootstrap" },
  { name: "HEARTBEAT.md", label: "Heartbeat" },
  { name: "MEMORY.md", label: "Memory" },
  { name: "AGENTS.md", label: "智能体" },
];

// FileState 镜像后端 GET 响应：`content` 是实际加载的内容，
// `source` 表示来源，`baseContent`（仅在 source==="db" 且有不同
// 所有者行可还原时设置）是用户还原时会回退到的内容。
//
//   - "db":      调用者自己的每用户覆盖行（仅 USER.md /
//                MEMORY.md）——与所有者内容不同。
//   - "owner":   智能体所有者的行，规范的"共享模板"
//                ——身份文件（SOUL/IDENTITY/BOOTSTRAP/...）
//                始终以此呈现，每用户文件回退到此。
//   - "fs":      旧版文件系统默认值。保留用于向后兼容。
//   - "default": 调用者和所有者行均不存在；标签页为空。
type FileSource = "db" | "owner" | "fs" | "default";
type FileState = { content: string; source: FileSource; baseContent?: string };

async function fetchCustomizeFiles(agentId: string): Promise<Record<string, FileState>> {
  const entries = await Promise.all(
    CUSTOMIZE_FILES.map(async (file) => {
      try {
        const res = await apiFetch(`/api/agents/${agentId}/system-files/${file.name}`);
        if (res.ok) {
          const data = await res.json();
          return [
            file.name,
            {
              content: data.content || "",
              source: (data.source || "default") as FileSource,
              baseContent: data.baseContent,
            },
          ] as [string, FileState];
        }
      } catch {}
      return [
        file.name,
        { content: "", source: "default" as FileSource },
      ] as [string, FileState];
    }),
  );
  return Object.fromEntries(entries);
}

export default function AgentCustomizePage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const [activeTab, setActiveTab] = useState("SOUL.md");
  const [files, setFiles] = useState<Record<string, FileState>>({});
  const [loadedAgentId, setLoadedAgentId] = useState("");
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  const loadAll = async () => {
    setFiles(await fetchCustomizeFiles(agentId));
    setLoadedAgentId(agentId);
  };

  useEffect(() => {
    let cancelled = false;
    fetchCustomizeFiles(agentId).then((nextFiles) => {
      if (cancelled) return;
      setFiles(nextFiles);
      setLoadedAgentId(agentId);
    });
    return () => {
      cancelled = true;
    };
  }, [agentId]);

  const loading = loadedAgentId !== agentId;
  const active = files[activeTab];

  const handleSave = async () => {
    setSaving(true);
    try {
      await apiFetch(`/api/agents/${agentId}/system-files/${activeTab}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ content: active?.content || "" }),
      });
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
      // 重新加载以使保存后 source/baseContent 保持准确。
      loadAll();
    } catch {}
    setSaving(false);
  };

  // 还原会删除 DB 覆盖，使运行时回退到智能体定义随附的
// 文件系统基础版本。仅在 source==="db" 且有 baseContent
// 时有意义（否则标签页只会变空）。
  const handleRevert = async () => {
    if (!active || active.source !== "db") return;
    if (!confirm(`确定将 ${activeTab} 还原为仓库基础版本吗？当前编辑内容将被丢弃。`)) return;
    setSaving(true);
    try {
      await apiFetch(`/api/agents/${agentId}/system-files/${activeTab}`, {
        method: "DELETE",
      });
      await loadAll();
    } catch {}
    setSaving(false);
  };

  if (loading) {
    return (
      <div className="p-6 space-y-4">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-96 w-full" />
      </div>
    );
  }

  const sourceBadge = (source: FileSource | undefined) => {
    if (source === "db") {
      return (
        <span className="text-xs px-2 py-0.5 rounded-md border border-amber-500/30 text-amber-600">
          已编辑
        </span>
      );
    }
    if (source === "fs") {
      return (
        <span className="text-xs px-2 py-0.5 rounded-md border border-emerald-500/30 text-emerald-600">
          来自仓库
        </span>
      );
    }
    return null;
  };

  return (
    <div className="p-6 max-w-4xl mx-auto">
      <div className="flex items-center justify-between mb-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">自定义</h2>
          <p className="text-sm text-muted-foreground mt-1">
            以下智能体的人格、记忆和行为文件： <strong>{agentName}</strong>
          </p>
        </div>
        <div className="flex gap-2">
          {active?.source === "db" && (
            <Button
              onClick={handleRevert}
              disabled={saving}
              variant="outline"
              title={
                active.baseContent
                  ? "放弃修改并还原为仓库中提供的文件"
                  : "放弃修改（此文件没有仓库基础版本，标签页将变为空白）"
              }
            >
              <RotateCcw className="h-4 w-4 mr-2" /> 还原
            </Button>
          )}
          <Button
            onClick={handleSave}
            disabled={saving}
            variant={saved ? "outline" : "default"}
            className={saved ? "border-emerald-500/30 text-emerald-600" : ""}
          >
            {saved ? (
              <><Check className="h-4 w-4 mr-2" /> 已保存</>
            ) : saving ? (
              <><Loader2 className="h-4 w-4 mr-2 animate-spin" /> 正在保存...</>
            ) : (
              <><Save className="h-4 w-4 mr-2" /> 保存</>
            )}
          </Button>
        </div>
      </div>

      {/* 标签页 */}
      <div className="flex gap-1 border-b border-border mb-4 overflow-x-auto">
        {CUSTOMIZE_FILES.map((f) => (
          <button
            key={f.name}
            onClick={() => setActiveTab(f.name)}
            className={`px-3 py-2 text-sm font-medium whitespace-nowrap border-b-2 transition-colors flex items-center gap-2 ${
              activeTab === f.name
                ? "border-primary text-primary"
                : "border-transparent text-muted-foreground hover:text-foreground"
            }`}
          >
            {f.label}
            {files[f.name]?.source === "db" && (
              <span className="size-1.5 rounded-full bg-amber-500" />
            )}
          </button>
        ))}
      </div>

      {/* 活动标签页状态行——仅在有可操作信息时显示
          （覆盖激活 / 来自仓库）。"default" 情况
          （空 + 无仓库基础）保持静默。 */}
      {(active?.source === "db" || active?.source === "fs") && (
        <div className="flex items-center gap-2 mb-2 text-xs text-muted-foreground">
          {sourceBadge(active?.source)}
          {active?.source === "db" && active.baseContent && (
            <span>覆盖已启用，仓库基础版本为 {active.baseContent.length} 个字符。</span>
          )}
          {active?.source === "fs" && (
            <span>加载来源： <code>{`<智能体 home>/${activeTab}`}</code>。编辑后会创建智能体专属覆盖版本。</span>
          )}
        </div>
      )}

      {/* 编辑器 */}
      <textarea
        value={active?.content || ""}
        onChange={(e) =>
          setFiles((prev) => ({
            ...prev,
            [activeTab]: { ...(prev[activeTab] || { source: "default" }), content: e.target.value },
          }))
        }
        spellCheck={false}
        className="w-full rounded-lg border border-border bg-card px-4 py-3 font-mono text-sm leading-relaxed outline-none focus:ring-1 focus:ring-primary/30 resize-none"
        // 限制高度使编辑器在设置对话框（85vh 模态框）内保持合理尺寸——
        // 之前用的 `calc(100vh - 240px)` 会让文本区域几乎占满整个对话框。
        // clamp 值也保证独立 /customize/ 页面可用：在高等屏幕上仍会增长，
        // 但不会"填满整个视口"。
        style={{ height: "min(55vh, 480px)", minHeight: 280 }}
        placeholder={`# ${activeTab}\n\nWrite your content here...`}
      />
    </div>
  );
}
