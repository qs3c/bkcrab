"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";

// AuthGuard owns authentication and the shared login screen for every URL.
export default function RootPage() {
  const router = useRouter();
  useEffect(() => { router.replace("/overview/"); }, [router]);
  return <div role="status" className="p-6 text-sm text-muted-foreground">正在打开工作台…</div>;
}
