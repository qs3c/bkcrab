// /agents/<aid>/chat — 新的临时聊天。可见 UI 由父级布局的
// <ChatScreen/> 渲染；此页面仅存在以便 Next 有路由可匹配。
// ChatScreen 读取 `usePathname()`，在没有 `chat/<sid>` 段时
// 切换到"新聊天"模式。
export default function ChatPage() {
  return null;
}
