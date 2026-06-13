"use client";

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { X, Plus } from "lucide-react";
import {
  updateSkillEntries,
  type SkillInfo,
  type SkillEnvSpec,
} from "@/lib/api";
import { useAgentName } from "@/hooks/use-agent-name";

// SkillEntryView 是掩码后的 GET /api/config 响应中单个技能条目的
// 结构。apiKey 和 env 值返回为 "***"，以便 UI 显示某项已配置而
// 不泄露密钥。
export interface SkillEntryView {
  enabled?: boolean;
  apiKey?: string;
  env?: Record<string, string>;
}

export function looksLikeSecret(name: string): boolean {
  const upper = name.toUpperCase();
  return ["KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"].some((m) =>
    upper.includes(m),
  );
}

// ConfigureSkillDialog 为声明的每个环境变量（来自技能的 SKILL.md
// frontmatter envSpec）渲染一个输入框，并提供一个后门用于添加技能
// 作者未声明的任意变量。保存时通过 updateSkillEntries POST；
// 当提供 `agentId` 时，补丁写入每个智能体的覆盖映射
//（cfg.Skills.AgentEntries[agentId][skillName]），否则写入全局映射
//（cfg.Skills.Entries[skillName]）。运行时优先解析智能体范围，然后
// 回退到全局。
export function ConfigureSkillDialog({
  skill,
  existing,
  agentId,
  onClose,
  onSaved,
}: {
  skill: SkillInfo | null;
  existing?: SkillEntryView;
  agentId?: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [env, setEnv] = useState<Record<string, string>>({});
  const [customRows, setCustomRows] = useState<{ name: string; value: string }[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const agentName = useAgentName(agentId || "");

  const declaredSpec: SkillEnvSpec[] = skill?.envSpec || [];
  const declaredNames = new Set(declaredSpec.map((s) => s.name));

  useEffect(() => {
    if (!skill) return;
    const initialEnv: Record<string, string> = {};
    for (const spec of declaredSpec) {
      initialEnv[spec.name] = existing?.env?.[spec.name] || "";
    }
    setEnv(initialEnv);
    const customs: { name: string; value: string }[] = [];
    if (existing?.env) {
      for (const [k, v] of Object.entries(existing.env)) {
        if (!declaredNames.has(k)) customs.push({ name: k, value: v });
      }
    }
    setCustomRows(customs);
    setError(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [skill]);

  if (!skill) return null;

  const updateEnv = (name: string, value: string) => {
    setEnv((prev) => ({ ...prev, [name]: value }));
  };

  const addCustomRow = () => setCustomRows((prev) => [...prev, { name: "", value: "" }]);
  const updateCustomRow = (idx: number, patch: Partial<{ name: string; value: string }>) =>
    setCustomRows((prev) => prev.map((r, i) => (i === idx ? { ...r, ...patch } : r)));
  const removeCustomRow = (idx: number) =>
    setCustomRows((prev) => prev.filter((_, i) => i !== idx));

  const handleSave = async () => {
    setSaving(true);
    setError(null);
    const merged: Record<string, string> = { ...env };
    for (const row of customRows) {
      if (!row.name.trim()) continue;
      merged[row.name.trim()] = row.value;
    }
    try {
      const resp = await updateSkillEntries(
        { [skill.name]: { enabled: true, env: merged } },
        agentId,
      );
      if (resp && resp.ok === false) {
        setError(resp.error || "保存失败");
        setSaving(false);
        return;
      }
      onSaved();
    } catch (e) {
      setError(e instanceof Error ? e.message : "保存失败");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={!!skill} onOpenChange={(v) => !v && onClose()}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>配置 {skill.name}</DialogTitle>
          <DialogDescription>
            {agentId ? (
              <>
                以下智能体的专属覆盖设置： <strong>{agentName}</strong>.
                此处字段留空时将使用全局值。
                其他智能体不受影响。
              </>
            ) : (
              <>
                全局默认值。除非智能体设置了专属覆盖值，否则所有运行此技能的智能体都会使用它。
              </>
            )}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-2">
          {declaredSpec.length === 0 && customRows.length === 0 && (
            <p className="text-sm text-muted-foreground/70">
              此技能未在 <code>SKILL.md</code> 的前置元数据中声明环境变量。
              如果技能需要读取变量，请在下方添加。
            </p>
          )}

          {declaredSpec.map((spec) => {
            const isSecret = spec.secret ?? looksLikeSecret(spec.name);
            const placeholder =
              isSecret && existing?.env?.[spec.name]?.includes("****")
                ? existing.env[spec.name]
                : isSecret
                ? "<未设置>"
                : "";
            return (
              <div key={spec.name} className="space-y-1.5">
                <Label className="font-mono text-xs flex items-center gap-2">
                  {spec.name}
                  {spec.required && (
                    <span className="text-[9px] uppercase tracking-wider text-amber-500">
                      必填
                    </span>
                  )}
                  {!spec.required && (
                    <span className="text-[9px] uppercase tracking-wider text-muted-foreground/60">
                      可选
                    </span>
                  )}
                </Label>
                <Input
                  type={isSecret ? "password" : "text"}
                  value={env[spec.name] || ""}
                  placeholder={placeholder}
                  onChange={(e) => updateEnv(spec.name, e.target.value)}
                  className="font-mono text-xs"
                />
                {spec.description && (
                  <p className="text-[11px] text-muted-foreground/70">
                    {spec.description}
                  </p>
                )}
              </div>
            );
          })}

          {customRows.length > 0 && (
            <div className="space-y-2 pt-2 border-t border-border/60">
              <Label className="text-xs uppercase tracking-wider text-muted-foreground/70">
                自定义环境变量
              </Label>
              {customRows.map((row, idx) => (
                <div key={idx} className="flex items-center gap-2">
                  <Input
                    placeholder="VAR_NAME"
                    value={row.name}
                    onChange={(e) => updateCustomRow(idx, { name: e.target.value })}
                    className="font-mono text-xs flex-1"
                  />
                  <Input
                    type={looksLikeSecret(row.name) ? "password" : "text"}
                    placeholder="value"
                    value={row.value}
                    onChange={(e) => updateCustomRow(idx, { value: e.target.value })}
                    className="font-mono text-xs flex-1"
                  />
                  <Button
                    variant="ghost"
                    size="icon"
                    className="h-8 w-8"
                    onClick={() => removeCustomRow(idx)}
                  >
                    <X className="h-3.5 w-3.5" />
                  </Button>
                </div>
              ))}
            </div>
          )}

          <Button
            variant="ghost"
            size="sm"
            className="text-xs"
            onClick={addCustomRow}
          >
            <Plus className="h-3 w-3 mr-1.5" />
            添加自定义环境变量
          </Button>

          {error && <p className="text-xs text-destructive">{error}</p>}
        </div>

        <div className="flex justify-end gap-2">
          <Button variant="outline" onClick={onClose}>
            取消
          </Button>
          <Button onClick={handleSave} disabled={saving}>
            {saving ? "正在保存…" : "保存"}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
