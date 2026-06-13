"use client";

import * as React from "react";
import { Bot, Check, Copy, Loader2, Save } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { Skeleton } from "@/components/ui/skeleton";
import { apiFetch, getAgent, updateAgent, type AgentDetail } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

// AgentProfilePanel 是设置对话框中的"资料"标签页 —
// 与管理页 /agents/page.tsx 中的编辑智能体对话框拥有相同字段
// （头像、名称、描述、公开开关），仅对智能体所有者开放编辑。
// 访问者（浏览中的 super_admin 或公开链接用户）只能看到只读字段。
// 面板通过 useAgentIdFromURL 从 URL 读取 agentId，无需对话框组件层层传递。

export default function AgentProfilePanel() {
  const agentId = useAgentIdFromURL();
  const [agent, setAgent] = React.useState<AgentDetail | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [saved, setSaved] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  // 表单状态 — 与 `agent` 独立，以便用户可以通过刷新来撤销修改，
  // 也让保存按钮可以对比后再写入。
  const [name, setName] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [isPublic, setIsPublic] = React.useState(false);
  const [avatar, setAvatar] = React.useState<File | null>(null);
  const [avatarPreview, setAvatarPreview] = React.useState<string | null>(null);
  const [avatarBust, setAvatarBust] = React.useState<number>(0);
  const [linkCopied, setLinkCopied] = React.useState(false);
  const fileInputRef = React.useRef<HTMLInputElement>(null);

  const refresh = React.useCallback(() => {
    if (!agentId) return;
    setLoading(true);
    getAgent(agentId)
      .then((a) => {
        if (!a) {
          setAgent(null);
          return;
        }
        setAgent(a);
        setName(a.name || "");
        setDescription(a.description || "");
        setIsPublic(!!a.isPublic);
      })
      .catch(() => {})
      .finally(() => setLoading(false));
  }, [agentId]);

  React.useEffect(() => {
    refresh();
  }, [refresh]);

  // 在文件更换或面板卸载时撤销我们创建的 blob URL —
  // 否则页面每换一次附件就泄漏一个 URL，虽然大多无害但 eslint
  // 在长时间会话中会标记。
  React.useEffect(() => {
    return () => {
      if (avatarPreview) URL.revokeObjectURL(avatarPreview);
    };
  }, [avatarPreview]);

  const isOwner = agent?.role === "owner";
  const dirty =
    !!agent &&
    (name.trim() !== (agent.name || "") ||
      description.trim() !== (agent.description || "") ||
      isPublic !== !!agent.isPublic ||
      avatar !== null);

  const onPickAvatar = (e: React.ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0] ?? null;
    setAvatar(f);
    if (avatarPreview) URL.revokeObjectURL(avatarPreview);
    setAvatarPreview(f ? URL.createObjectURL(f) : null);
  };

  const uploadAvatar = async (file: File) => {
    const fd = new FormData();
    fd.append("file", file, "avatar.png");
    await apiFetch(`/api/agents/${agentId}/files`, { method: "POST", body: fd });
    setAvatarBust(Date.now());
  };

  const onSave = async () => {
    if (!agentId || !agent || !isOwner) return;
    if (!name.trim()) {
      setError("名称为必填项");
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const resp = await updateAgent(agentId, {
        name: name.trim(),
        description: description.trim(),
        isPublic,
      });
      if (resp && (resp.ok === false || resp.error)) {
        setError(resp.error || "更新智能体失败");
        return;
      }
      if (avatar) {
        try {
          await uploadAvatar(avatar);
        } catch {
// 非致命：文本字段已保存，仅头像上传失败。保留已保存脉冲，
           // 让用户知道主要写入已成功；下次保存时可重试图片。
        }
        setAvatar(null);
        if (avatarPreview) URL.revokeObjectURL(avatarPreview);
        setAvatarPreview(null);
      }
      setSaved(true);
      setTimeout(() => setSaved(false), 1500);
      refresh();
    } finally {
      setSaving(false);
    }
  };

  if (loading) {
    return (
      <div className="p-6 max-w-3xl space-y-4">
        <Skeleton className="h-8 w-32" />
        <Skeleton className="h-20 w-full" />
        <Skeleton className="h-12 w-full" />
        <Skeleton className="h-24 w-full" />
      </div>
    );
  }

  if (!agent) {
    return (
      <div className="p-6 max-w-3xl">
        <p className="text-sm text-muted-foreground">未找到智能体。</p>
      </div>
    );
  }

  // 头像 src：可编辑预览优先，其次是上传后的带版本号 URL
  // （使缓存图片刷新），最后是规范头像路由。
  const avatarSrc =
    avatarPreview ||
    `/api/agents/${agent.id}/files/avatar.png${avatarBust ? `?v=${avatarBust}` : ""}`;

  return (
    <div className="p-6 max-w-3xl space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">资料</h2>
          <p className="text-sm text-muted-foreground mt-1">
            {isOwner
              ? "更新智能体的名称、描述和头像。"
              : "只读：只有智能体所有者可以编辑这些字段。"}
          </p>
        </div>
        {isOwner && (
          <Button
            onClick={onSave}
            disabled={saving || !dirty || !name.trim()}
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
        )}
      </div>

      {error && (
        <div className="rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive">
          {error}
        </div>
      )}

      <div className="rounded-lg border border-border bg-card p-5 space-y-5">
        {/* 头像和名称在同一行，与管理页的编辑对话框一致。 */}
        <div className="flex items-start gap-4">
          <button
            type="button"
            onClick={() => isOwner && fileInputRef.current?.click()}
            disabled={!isOwner}
            className="group relative flex size-20 shrink-0 items-center justify-center overflow-hidden rounded-xl border border-dashed bg-muted/40 transition hover:bg-muted disabled:cursor-not-allowed"
            aria-label="上传头像"
          >
            <AgentAvatarImg src={avatarSrc} />
            <input
              ref={fileInputRef}
              type="file"
              accept="image/*"
              className="hidden"
              onChange={onPickAvatar}
              disabled={!isOwner}
            />
          </button>
          <div className="flex-1 space-y-2">
            <Label htmlFor="agent-profile-name">名称</Label>
            <Input
              id="agent-profile-name"
              value={name}
              onChange={(e) => {
                setName(e.target.value);
                setError(null);
              }}
              placeholder="我的助手"
              disabled={!isOwner}
            />
            <p className="text-xs text-muted-foreground">
              ID：{" "}
              <code className="rounded bg-muted px-1 py-0.5 font-mono text-[11px]">
                {agent.id}
              </code>
            </p>
          </div>
        </div>

        <div className="space-y-2">
          <Label htmlFor="agent-profile-desc">描述</Label>
          <Textarea
            id="agent-profile-desc"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="这个智能体用于什么？"
            rows={3}
            disabled={!isOwner}
          />
        </div>
      </div>

      <div className="space-y-3 rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="space-y-1">
            <Label htmlFor="agent-profile-public" className="text-sm font-medium">
              公开访问
            </Label>
            <p className="text-xs text-muted-foreground">
              {isPublic
                ? "任何获得链接的人都可以聊天，其对话历史仅本人可见。"
                : "只有你可以使用此智能体。"}
            </p>
          </div>
          <Switch
            id="agent-profile-public"
            checked={isPublic}
            onCheckedChange={(v) => {
              setIsPublic(!!v);
              setLinkCopied(false);
            }}
            disabled={!isOwner}
          />
        </div>
        {isPublic && agent && (
          <div className="flex gap-2">
            <Input
              readOnly
              value={
                typeof window !== "undefined"
                  ? `${window.location.origin}/agents/${agent.id}/chat/`
                  : `/agents/${agent.id}/chat/`
              }
              onFocus={(e) => e.currentTarget.select()}
              className="font-mono text-xs"
            />
            <Button
              type="button"
              variant="outline"
              onClick={async () => {
                if (!agent) return;
                const url = `${window.location.origin}/agents/${agent.id}/chat/`;
                try {
                  await navigator.clipboard.writeText(url);
                  setLinkCopied(true);
                  setTimeout(() => setLinkCopied(false), 2000);
                } catch {
                  // 剪贴板被阻止 — 用户仍可手动选中输入框
                }
              }}
            >
              {linkCopied ? (
                <>
                  <Check className="h-4 w-4 mr-1.5" />
                  已复制
                </>
              ) : (
                <>
                  <Copy className="h-4 w-4 mr-1.5" />
                  复制
                </>
              )}
            </Button>
          </div>
        )}
      </div>
    </div>
  );
}

// AgentAvatarImg 渲染头像并在无图时回退为 Bot 图标，
// 避免未上传 avatar.png 的智能体显示碎图图标。
// 与 team-switcher 的 AgentAvatar 镜像，但接受纯 src 以便
// 编辑时替换为本地 blob URL。
function AgentAvatarImg({ src }: { src: string }) {
  const [failed, setFailed] = React.useState(false);
  React.useEffect(() => {
    setFailed(false);
  }, [src]);
  if (failed) {
    return (
      <div className="flex h-full w-full items-center justify-center bg-primary/10 dark:bg-primary/15">
        <Bot className="h-9 w-9 text-primary" />
      </div>
    );
  }
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={src}
      alt=""
      className="h-full w-full object-cover"
      onError={() => setFailed(true)}
    />
  );
}
