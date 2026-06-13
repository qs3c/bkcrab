// /agents/<aid>/ — 裸智能体 URL。UI 由父级 layout 的 <ChatScreen/>
// 渲染（将其视为与 /agents/<aid>/chat/ 相同的新对话状态）。
// 此页面仅用于让 Next 有可匹配的路由——缺少它则裸 URL 会 404。
export default function AgentRootPage() {
  return null;
}
