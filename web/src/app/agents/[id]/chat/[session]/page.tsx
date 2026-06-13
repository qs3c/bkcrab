// /agents/<aid>/chat/<session> — 按 ID 打开已有聊天。UI 由
// 父级布局的 <ChatScreen/> 渲染；此页面仅存在以便 Next 有
// 动态路由可匹配。ChatScreen 从 `usePathname()` 读取会话 ID。
//
// generateStaticParams：在 output:'export' 下，Next 为每个
// 参数元组生成一个 .html。我们发布一个占位符（"_"）并依赖
// Go SPA 回退在运行时将其提供给任何具体的会话 ID。
export async function generateStaticParams() {
  return [{ session: "_" }];
}

export default function ChatSessionPage() {
  return null;
}
