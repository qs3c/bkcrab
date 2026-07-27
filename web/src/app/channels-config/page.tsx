"use client";

import { useEffect, useState, useCallback } from "react";
import {
  listScopedChannels,
  createScopedChannel,
  updateScopedChannel,
  deleteScopedChannel,
  type ChannelRow,
  type ScopeName,
} from "@/lib/api";
import { ScopePicker } from "@/components/scope-picker";

const CHANNEL_TYPES = ["telegram", "discord", "slack"];
const CHANNEL_LABELS: Record<string, string> = {
  telegram: "Telegram",
  discord: "Discord",
  slack: "Slack",
};
const SCOPE_LABELS: Record<string, string> = {
  system: "系统",
  user: "用户",
  agent: "智能体",
};

export default function ChannelsConfigPage() {
  const [scope, setScope] = useState<ScopeName>("system");
  const [scopeId, setScopeId] = useState<string>("");
  const [rows, setRows] = useState<ChannelRow[]>([]);
  const [error, setError] = useState("");
  const [draft, setDraft] = useState({
    type: "telegram",
    enabled: true,
    botToken: "",
    appToken: "",
  });

  const refresh = useCallback(async () => {
    const r = await listScopedChannels(scope, scopeId);
    setError("");
    if (r.channels) setRows(r.channels);
    if (r.error) setError(r.error);
  }, [scope, scopeId]);

  useEffect(() => {
    if (scope !== "system" && !scopeId) return;
    let cancelled = false;
    listScopedChannels(scope, scopeId).then((r) => {
      if (cancelled) return;
      setError("");
      if (r.channels) setRows(r.channels);
      if (r.error) setError(r.error);
    });
    return () => {
      cancelled = true;
    };
  }, [scope, scopeId]);

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    if (!draft.botToken && draft.type !== "slack") return;
    const res = await createScopedChannel({ scope, scopeId, ...draft });
    if (res.error) {
      setError(res.error);
      return;
    }
    setDraft({ type: "telegram", enabled: true, botToken: "", appToken: "" });
    refresh();
  }

  async function handleToggle(row: ChannelRow, enabled: boolean) {
    const res = await updateScopedChannel(row.id, { enabled });
    if (res.error) setError(res.error);
    refresh();
  }

  async function handleDelete(row: ChannelRow) {
    if (!confirm(`确定删除${SCOPE_LABELS[row.scope] || row.scope}/${row.scopeId || "全局"}范围内的${CHANNEL_LABELS[row.type] || row.type}吗？`)) return;
    const res = await deleteScopedChannel(row.id);
    if (res.error) setError(res.error);
    refresh();
  }

  async function handleRotateToken(row: ChannelRow, token: string) {
    if (!token) return;
    const res = await updateScopedChannel(row.id, { botToken: token });
    if (res.error) setError(res.error);
    refresh();
  }

  return (
    <div className="p-8 text-zinc-100">
      <h1 className="mb-2 text-2xl font-bold">渠道</h1>
      <p className="mb-6 text-sm text-zinc-500">
        可在任意范围添加 Telegram、Discord 或 Slack 机器人。内层范围中设置 <code>enabled=false</code> 后，可对该用户或智能体隐藏外层范围的渠道。
      </p>

      <div className="mb-6">
        <ScopePicker scope={scope} scopeId={scopeId} onChange={(s, id) => { setScope(s); setScopeId(id); }} />
      </div>

      <form onSubmit={handleCreate} className="mb-6 space-y-3 rounded-lg border border-zinc-800 bg-zinc-900 p-4">
        <h2 className="font-semibold">添加 渠道</h2>
        <div className="grid grid-cols-2 gap-3">
          <select value={draft.type} onChange={(e) => setDraft({ ...draft, type: e.target.value })} className="rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm">
            {CHANNEL_TYPES.map((t) => <option key={t} value={t}>{CHANNEL_LABELS[t] || t}</option>)}
          </select>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={draft.enabled} onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })} />
            已启用
          </label>
        </div>
        <input type="password" value={draft.botToken} onChange={(e) => setDraft({ ...draft, botToken: e.target.value })} placeholder="机器人令牌" className="w-full rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm" />
        {draft.type === "slack" && (
          <input type="password" value={draft.appToken} onChange={(e) => setDraft({ ...draft, appToken: e.target.value })} placeholder="应用令牌（Slack Socket 模式）" className="w-full rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm" />
        )}
        <button type="submit" className="rounded bg-violet-600 px-4 py-2 text-sm">保存</button>
      </form>

      {error && <p className="mb-4 text-sm text-red-400">{error}</p>}

      <table className="w-full text-sm">
        <thead className="text-left text-zinc-400">
          <tr>
            <th className="py-2">类型</th>
            <th>机器人令牌</th>
            <th>已启用</th>
            <th>凭据键</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.id} className="border-t border-zinc-800">
              <td className="py-3 font-medium">{CHANNEL_LABELS[row.type] || row.type}</td>
              <td>
                <input
                  type="password"
                  placeholder={row.botToken || "****"}
                  onBlur={(e) => handleRotateToken(row, e.target.value)}
                  className="w-full rounded border border-zinc-700 bg-zinc-950 px-2 py-1 text-xs"
                />
              </td>
              <td>
                <input type="checkbox" checked={row.enabled} onChange={(e) => handleToggle(row, e.target.checked)} />
              </td>
              <td className="font-mono text-xs text-zinc-500">{row.credentialKey}</td>
              <td className="text-right">
                <button onClick={() => handleDelete(row)} className="text-xs text-red-400 hover:underline">删除</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
