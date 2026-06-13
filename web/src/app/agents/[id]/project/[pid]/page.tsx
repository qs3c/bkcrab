// /agents/<aid>/project/<pid> — 项目中的新对话。UI 由父级 layout 的
// <ChatScreen/> 渲染；此页面仅用于让 Next 有可匹配的动态路由。
// ChatScreen 从 `usePathname()` 读取项目 ID，并将其视为懒创建标记——
// 会话行在用户首次发送消息时创建，URL 通过 history.replaceState
// 升级为 /chat/<sid>/（无需重新挂载）。
//
// generateStaticParams：在 output:'export' 模式下，Next 会为每个参数
// 组合生成一个 .html。我们仅发布一个占位符（"_"），并依赖 Go 的
// SPA 回退机制在运行时为任意具体的 pid 提供服务。
export async function generateStaticParams() {
  return [{ pid: "_" }];
}

export default function ProjectPage() {
  return null;
}
