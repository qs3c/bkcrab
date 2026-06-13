"use client";

import type { AnchorHTMLAttributes } from "react";

/**
 * ExternalAnchor 为 ReactMarkdown 聊天内容渲染 <a>，有一个调整：
 * 指向不同源的链接在新标签页中打开。同源 / 相对 / mailto / # 链接
 * 保持默认行为。
 *
 * 原因：聊天回复中经常包含外站 URL（Namecheap、GitHub、文档）。
 * 在当前标签页打开这些链接会导航离开聊天会话，丢失智能体上下文。
 * 仅对跨源 URL 强制 target="_blank" 可避免破坏应用内导航（应用内
 * /agents/<id>/... 链接仍依赖 Next.js 路由）。
 *
 * 搭配 rel="noopener noreferrer" 使用，使弹窗无法访问
 * window.opener —— 这是对任何在新标签页打开的锚点的标准加固。
 */
export function ExternalAnchor(props: AnchorHTMLAttributes<HTMLAnchorElement>) {
  const { href, children, ...rest } = props;
  const external = isExternalHref(href);
  if (external) {
    return (
      <a href={href} target="_blank" rel="noopener noreferrer" {...rest}>
        {children}
      </a>
    );
  }
  return (
    <a href={href} {...rest}>
      {children}
    </a>
  );
}

function isExternalHref(href: string | undefined): boolean {
  if (!href) return false;
  // 跳过 mailto:、tel:、# 锚点和无协议 / 相对路径 ——
  // 它们应保持默认的原地行为。
  if (!/^https?:\/\//i.test(href)) return false;
  if (typeof window === "undefined") {
    // 服务端渲染：假设任何绝对 http(s) 链接都可能是外部的。
    // 客户端水合时会根据真实源重新评估。
    return true;
  }
  try {
    const u = new URL(href);
    return u.host !== window.location.host;
  } catch {
    return false;
  }
}
