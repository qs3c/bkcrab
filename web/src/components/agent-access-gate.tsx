"use client";

import { useEffect, useState } from "react";
import { usePathname } from "next/navigation";
import { Bot } from "lucide-react";
import { getAgentStatus } from "@/lib/api";

// 直接从 URL 中提取智能体 ID。在 output:'export' 模式下，
// /agents/agt_xxx/chat/ 提供的 HTML 实际上是预构建的
// /agents/default/chat/index.html（参见服务器的静态回退）；
// 该打包中的 useParams() 在初始渲染时解析为 {id: "default"}，
// 即使是真正的所有者也会导致访问探测失败。在客户端读取
// pathname 是明确无误的。
function agentIdFromPath(pathname: string | null | undefined): string {
  if (!pathname) return "";
  // 匹配 /agents/<id>(/...)? — 对前缀严格匹配，避免从其他路由
  // 中误提取 ID。
  const m = pathname.match(/^\/agents\/([^/]+)/);
  return m ? m[1] : "";
}

// AgentAccessGate 在挂载时探测 /api/agents/{id}，然后：
  //   - 200：渲染子组件（调用者是所有者 / super_admin / 公开链接
  //     访客 / apikey ACL 授权者）
  //   - 401：重定向到 /login（通常在 apiFetch 层处理）
  //   - 403/404 或其他失败：显示"无权访问"界面，覆盖整个
  //     视口（包括侧边栏），防止非所有者通过输入 URL 窥探智能体
  //     的名称 / 会话 / 管理标签页。
  //
  // 放在 [id]/layout 层级，使每个嵌套路由（聊天、自定义、
  // 技能等）都继承同一道门。智能体 ID 从 URL 通过 useParams()
  // 读取 — 在 output:'export' 下从 layout 的 `params` 传递不生效，
  // 因为 params 在构建时解析为 generateStaticParams 返回的值
  // （"default"），而非 URL 中的值。
export default function AgentAccessGate({
  children,
}: {
  children: React.ReactNode;
}) {
  const pathname = usePathname();
  const agentId = agentIdFromPath(pathname);
  const [state, setState] = useState<"checking" | "ok" | "denied">("checking");

  useEffect(() => {
    // "default" ID 是预构建的静态导出占位符，不是真正的智能体 —
    // 跳过探测，直接渲染子组件。真正的 /agents/default/* 路由
    // 是 super_admin 的本地模式仪表板，已有自身的服务端门控。
    if (!agentId || agentId === "default") {
      setState("ok");
      return;
    }
    let aborted = false;
    setState("checking");
    getAgentStatus(agentId)
      .then(({ status, agent }) => {
        if (aborted) return;
        if (status === 200 && agent) {
          setState("ok");
          return;
        }
        setState("denied");
      })
      .catch(() => {
        if (!aborted) setState("denied");
      });
    return () => {
      aborted = true;
    };
  }, [agentId]);

  if (state === "checking") {
    // 探测期间的整视口占位 — z-50 使其覆盖 AppShell 侧边栏，
    // 防止非所有者在 403 尚未到达时短暂看到聊天 UI / 管理标签页。
    return (
      <div className="fixed inset-0 z-50 flex items-center justify-center bg-background">
        <div className="h-2 w-2 animate-pulse rounded-full bg-muted-foreground/40" />
      </div>
    );
  }

  if (state === "denied") {
    return (
      <div className="fixed inset-0 z-50 flex items-center justify-center bg-background p-6">
        <div className="max-w-md text-center space-y-4">
          <div className="mx-auto flex h-14 w-14 items-center justify-center rounded-2xl bg-muted/60">
            <Bot className="h-7 w-7 text-muted-foreground" />
          </div>
          <h2 className="text-lg font-semibold">无权访问此智能体</h2>
          <p className="text-sm text-muted-foreground">
            此智能体仅对所有者开放，或该链接已失效。所有者公开分享后，此对话链接会自动可用。
          </p>
        </div>
      </div>
    );
  }

  return <>{children}</>;
}
