"use client";

import { useParams, usePathname } from "next/navigation";

// 静态导出只生成 /agents/default/... 路径，因此当应用通过 Go spaHandler 回退
// 为非默认智能体提供服务时，useParams() 总是返回 "default"。
// 改为从 *响应式* pathname 中解析真实 id——usePathname() 在每次客户端
// 导航时都会更新，因此当用户切换智能体时，调用方会立即看到新 id
// （否则后台请求将继续针对旧 id 发出，聊天面板会显示错误的历史记录）。
export function useAgentIdFromURL(): string {
  const pathname = usePathname();
  const params = useParams<{ id: string }>();
  const m = pathname?.match(/\/agents\/([^/]+)\//);
  if (m) return m[1];
  return params?.id ?? "default";
}