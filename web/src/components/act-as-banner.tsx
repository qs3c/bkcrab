"use client";

import { useEffect, useState } from "react";
import { getMe } from "@/lib/api";

// ActAsBanner 在 super_admin 通过 ?actAs= 浏览其他用户资源时，
// 显示一个粘性只读警告。普通用户和查看自身数据的 super_admin
// 不会看到此横幅。
export function ActAsBanner() {
  const [actAs, setActAs] = useState<string>("");

  useEffect(() => {
    let aborted = false;
    (async () => {
      const me = await getMe();
      if (aborted) return;
      if (me.actAsUserId) setActAs(me.actAsUserId);
    })();
    return () => { aborted = true; };
  }, []);

  if (!actAs) return null;
  return (
    <div className="sticky top-0 z-50 bg-amber-700/80 px-4 py-2 text-center text-xs text-amber-50 backdrop-blur">
      当前查看身份 <code className="font-mono">{actAs}</code> · 只读，修改操作已禁用
    </div>
  );
}
