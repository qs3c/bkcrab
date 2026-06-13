import AgentLayoutClient from "./layout-client";

export function generateStaticParams() {
  return [{ id: "default" }];
}

// 此处的服务端参数在构建时解析（output: 'export' 会将
// generateStaticParams 的 "default" 烘焙到产物中），因此无法向下
// 传递 agent id——客户端包装组件通过 usePathname() 从 URL 中读取。
export default function AgentLayout({ children }: { children: React.ReactNode }) {
  return <AgentLayoutClient>{children}</AgentLayoutClient>;
}
