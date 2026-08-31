export function generateStaticParams() {
  // 静态导出只烘焙一个占位详情页；Go 的 SPA fallback 会在运行时为
  // 任意真实 run ID 返回该页面，客户端再从 URL 读取具体 ID。
  return [{ id: "_" }];
}

export default function ParserEvaluationRunLayout({ children }: { children: React.ReactNode }) {
  return children;
}
