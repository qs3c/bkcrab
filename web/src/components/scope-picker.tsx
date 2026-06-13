"use client";

import { useEffect, useState } from "react";
import { apiFetch, getMe, type ScopeName } from "@/lib/api";

interface AgentRef {
  id: string;
  name: string;
}
interface UserRef {
  id: string;
  username: string;
  email: string;
}

interface ScopePickerProps {
  scope: ScopeName;
  scopeId: string;
  onChange: (scope: ScopeName, scopeId: string) => void;
}

// ScopePicker 是供应商和渠道管理页的共享"系统 / 用户 / 智能体"
// 选择器。可用范围取决于调用者角色：
//
//   super_admin：全部三个（系统、每个用户、每个智能体）
//   user：       仅自己的用户范围和自己拥有的智能体
export function ScopePicker({ scope, scopeId, onChange }: ScopePickerProps) {
  const [role, setRole] = useState<string>("");
  const [meId, setMeId] = useState<string>("");
  const [users, setUsers] = useState<UserRef[]>([]);
  const [agents, setAgents] = useState<AgentRef[]>([]);

  useEffect(() => {
    let aborted = false;
    (async () => {
      const me = await getMe();
      if (aborted || !me.user) return;
      setRole(me.user.role);
      setMeId(me.user.id);

      // 拉取智能体列表 —— 每个调用者都能看到自己的智能体。
      const ag = await apiFetch("/api/agents");
      const aj = await ag.json();
      if (!aborted && aj.agents) setAgents(aj.agents);

      // super_admin 还可以枚举用户（用于选择用户范围）。
      if (me.user.role === "super_admin") {
        const u = await apiFetch("/api/users");
        const uj = await u.json();
        if (!aborted && uj.users) setUsers(uj.users);
      }
    })();
    return () => {
      aborted = true;
    };
  }, []);

  const isAdmin = role === "super_admin";

  function handleScopeChange(next: ScopeName) {
    if (next === "system") onChange("system", "");
    else if (next === "user") onChange("user", isAdmin && users[0]?.id ? users[0].id : meId);
    else onChange("agent", agents[0]?.id ?? "");
  }

  return (
    <div className="flex flex-wrap items-center gap-2 text-sm">
      <span className="text-zinc-400">范围：</span>
      <select
        value={scope}
        onChange={(e) => handleScopeChange(e.target.value as ScopeName)}
        className="rounded border border-zinc-700 bg-zinc-950 px-2 py-1"
      >
        {isAdmin && <option value="system">系统</option>}
        <option value="user">用户</option>
        <option value="agent">智能体</option>
      </select>
      {scope === "user" && isAdmin && (
        <select
          value={scopeId}
          onChange={(e) => onChange("user", e.target.value)}
          className="rounded border border-zinc-700 bg-zinc-950 px-2 py-1"
        >
          {users.map((u) => (
            <option key={u.id} value={u.id}>
              {u.username} ({u.email})
            </option>
          ))}
        </select>
      )}
      {scope === "user" && !isAdmin && (
        <span className="rounded bg-zinc-800 px-2 py-1 text-xs text-zinc-300">你</span>
      )}
      {scope === "agent" && (
        <select
          value={scopeId}
          onChange={(e) => onChange("agent", e.target.value)}
          className="rounded border border-zinc-700 bg-zinc-950 px-2 py-1"
        >
          {agents.map((a) => (
            <option key={a.id} value={a.id}>
              {a.id}
            </option>
          ))}
        </select>
      )}
    </div>
  );
}
