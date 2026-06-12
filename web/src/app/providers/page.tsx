"use client";

import { useEffect, useState, useCallback } from "react";
import {
  listProviders,
  createProvider,
  updateProvider,
  deleteProvider,
  type ProviderRow,
  type ScopeName,
} from "@/lib/api";
import { ScopePicker } from "@/components/scope-picker";

const SCOPE_LABELS: Record<string, string> = {
  system: "系统",
  user: "用户",
  agent: "智能体",
};
const API_TYPE_LABELS: Record<string, string> = {
  "openai-chat": "OpenAI 聊天补全",
  "anthropic-messages": "Anthropic 消息",
};

export default function ProvidersPage() {
  const [scope, setScope] = useState<ScopeName>("system");
  const [scopeId, setScopeId] = useState<string>("");
  const [rows, setRows] = useState<ProviderRow[]>([]);
  const [error, setError] = useState("");
  const [draft, setDraft] = useState({
    name: "",
    apiBase: "",
    apiKey: "",
    apiType: "openai-chat",
    authType: "bearer-token",
  });

  const refresh = useCallback(async () => {
    setError("");
    const r = await listProviders(scope, scopeId);
    if (r.providers) setRows(r.providers);
    if (r.error) setError(r.error);
  }, [scope, scopeId]);

  useEffect(() => {
    if (scope === "system" || scopeId) refresh();
  }, [scope, scopeId, refresh]);

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    if (!draft.name) return;
    const res = await createProvider({ scope, scopeId, ...draft });
    if (res.error) {
      setError(res.error);
      return;
    }
    setDraft({ name: "", apiBase: "", apiKey: "", apiType: "openai-chat", authType: "bearer-token" });
    refresh();
  }

  async function handleEdit(row: ProviderRow, patch: Partial<ProviderRow>) {
    const res = await updateProvider(row.id, patch);
    if (res.error) setError(res.error);
    refresh();
  }

  async function handleDelete(row: ProviderRow) {
    if (!confirm(`确定删除${SCOPE_LABELS[row.scope] || row.scope}/${row.scopeId || "全局"}范围内的服务商 ${row.name} 吗？`)) return;
    const res = await deleteProvider(row.id);
    if (res.error) setError(res.error);
    refresh();
  }

  return (
    <div className="p-8 text-zinc-100">
      <h1 className="mb-2 text-2xl font-bold">大模型服务商</h1>
      <p className="mb-6 text-sm text-zinc-500">
        系统级服务商由所有用户共享； 用户级服务商会覆盖同名系统配置； 智能体级服务商会覆盖前两者。
      </p>

      <div className="mb-6">
        <ScopePicker scope={scope} scopeId={scopeId} onChange={(s, id) => { setScope(s); setScopeId(id); }} />
      </div>

      <form onSubmit={handleCreate} className="mb-6 space-y-3 rounded-lg border border-zinc-800 bg-zinc-900 p-4">
        <h2 className="font-semibold">添加服务商</h2>
        <div className="grid grid-cols-2 gap-3">
          <input value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} placeholder="名称（例如 openai）" className="rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm" />
          <input value={draft.apiBase} onChange={(e) => setDraft({ ...draft, apiBase: e.target.value })} placeholder="API 基础地址" className="rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm" />
        </div>
        <input type="password" value={draft.apiKey} onChange={(e) => setDraft({ ...draft, apiKey: e.target.value })} placeholder="API 密钥" className="w-full rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm" />
        <div className="grid grid-cols-2 gap-3">
          <select value={draft.apiType} onChange={(e) => setDraft({ ...draft, apiType: e.target.value })} className="rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm">
            <option value="openai-chat">OpenAI 聊天补全</option>
            <option value="anthropic-messages">Anthropic 消息</option>
          </select>
          <select value={draft.authType} onChange={(e) => setDraft({ ...draft, authType: e.target.value })} className="rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm">
            <option value="bearer-token">Bearer 令牌</option>
            <option value="api-key">API 密钥请求头</option>
          </select>
        </div>
        <button type="submit" className="rounded bg-violet-600 px-4 py-2 text-sm">保存</button>
      </form>

      {error && <p className="mb-4 text-sm text-red-400">{error}</p>}

      <table className="w-full text-sm">
        <thead className="text-left text-zinc-400">
          <tr>
            <th className="py-2">名称</th>
            <th>API 基础地址</th>
            <th>密钥</th>
            <th>类型</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.id} className="border-t border-zinc-800">
              <td className="py-3 font-medium">{row.name}</td>
              <td>
                <input
                  defaultValue={row.apiBase}
                  onBlur={(e) => e.target.value !== row.apiBase && handleEdit(row, { apiBase: e.target.value })}
                  className="w-full rounded border border-zinc-700 bg-zinc-950 px-2 py-1 text-xs"
                />
              </td>
              <td>
                <input
                  type="password"
                  placeholder={row.apiKey || "****"}
                  onBlur={(e) => e.target.value && handleEdit(row, { apiKey: e.target.value })}
                  className="w-full rounded border border-zinc-700 bg-zinc-950 px-2 py-1 text-xs"
                />
              </td>
              <td className="text-xs text-zinc-500">
                {(row.apiType && API_TYPE_LABELS[row.apiType]) || row.apiType || "未设置"}
              </td>
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
