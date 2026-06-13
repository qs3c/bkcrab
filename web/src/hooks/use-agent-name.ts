"use client";

import { useEffect, useState } from "react";
import { getAgents } from "@/lib/api";

// useAgentName 将智能体 id 解析为显示名称。当智能体列表正在加载
// 或 id 不在列表中时，返回 id 本身，以避免页面框架在空状态和已解析
// 状态之间闪烁。传入空字符串可跳过请求。
export function useAgentName(agentId: string): string {
  const [name, setName] = useState<string>(agentId);
  useEffect(() => {
    if (!agentId) {
      setName("");
      return;
    }
    let aborted = false;
    setName(agentId);
    getAgents()
      .then((list) => {
        if (aborted) return;
        const me = list.find((a) => a.id === agentId);
        if (me?.name) setName(me.name);
      })
      .catch(() => {
        // 保留 id 作为回退名称
      });
    return () => {
      aborted = true;
    };
  }, [agentId]);
  return name;
}