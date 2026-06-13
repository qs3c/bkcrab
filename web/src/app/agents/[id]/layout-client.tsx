"use client";

import { usePathname } from "next/navigation";
import AgentAccessGate from "@/components/agent-access-gate";
import { ChatScreen } from "@/components/chat-screen";

// AgentLayoutClient 拥有 /agents/<id>/{chat,project} 下所有内容的
// 唯一 ChatScreen 实例。此前每个聊天路由段（chat/、chat/[session]、
// project/[pid]）都渲染各自的 <ChatScreen/>，因此在侧边栏链接之间
// 导航时会卸载并重新挂载整个聊天界面——丢失滚动位置、清空消息、
// 中断 SSE，并堆积订阅-重放洪流耗尽浏览器连接池。在此处挂载
// ChatScreen 可在侧边栏导航期间保持一个实例存活；ChatScreen 从
// usePathname() 读取 sessionId/projectId 并原地响应 URL 变化。
//
// chat/、chat/[session]/、project/[pid]/ 下的 page.tsx 文件仍然存在
// （Next 需要它们来路由 + 静态导出），但每个都返回 null——所有可见
// UI 都在 ChatScreen 中。
//
// 不属于聊天界面的同级路由（customize/、models/、channels/ 等）
// 渲染各自的 page.tsx——对于这些路由，下方的 gate 仅渲染 {children}，
// 因此 ChatScreen 不会在它们下方。
function isChatRoute(pathname: string, agentId: string): boolean {
  if (!agentId) return false;
  const base = `/agents/${agentId}`;
  if (pathname === base || pathname === `${base}/`) return true;
  // 匹配 `/chat`（不论是否有尾随段）但不匹配 `/chats`——
  // chats 列表是同级路由，必须单独渲染，
  // 不能在其下方放置 ChatScreen。
  const tail = pathname.slice(base.length);
  return (
    tail === "/chat" ||
    tail.startsWith("/chat/") ||
    tail === "/project" ||
    tail.startsWith("/project/")
  );
}

export default function AgentLayoutClient({
  children,
}: {
  children: React.ReactNode;
}) {
  const pathname = usePathname() || "";
  // 从 URL 中提取 agent id，方式与 AgentAccessGate 一致——
  // 服务端的 `params` 在构建时始终为 "default"。
  const m = pathname.match(/^\/agents\/([^/]+)/);
  const agentId = m ? m[1] : "";
  const onChat = isChatRoute(pathname, agentId);
  return (
    <AgentAccessGate>
      {onChat ? (
        <>
          <ChatScreen />
          {/* 页面插槽仍然渲染（返回 null），以便 Next 的路由器
              在导航时挂载/卸载它——这就是触发 ChatScreen 响应的
              pathname 更新的原因。 */}
          {children}
        </>
      ) : (
        children
      )}
    </AgentAccessGate>
  );
}
