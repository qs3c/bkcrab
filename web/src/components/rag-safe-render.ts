import {
  createElement,
  type AnchorHTMLAttributes,
  type ComponentProps,
  type ImgHTMLAttributes,
  type ReactNode,
} from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

type MarkdownProps = ComponentProps<typeof ReactMarkdown>;

type MarkdownAnchorProps = AnchorHTMLAttributes<HTMLAnchorElement> & {
  node?: unknown;
};

type MarkdownImageProps = ImgHTMLAttributes<HTMLImageElement> & {
  node?: unknown;
};

const SAFE_INLINE_RASTER = /^data:image\/(?:png|jpe?g|webp|gif);base64,/i;

function isSafeAgentImageSource(src: string | Blob | undefined): src is string {
  const value = typeof src === "string" ? src.trim() : "";
  if (!value || /[\u0000-\u001f\u007f]/.test(value)) return false;
  if (SAFE_INLINE_RASTER.test(value)) return true;

  // Markdown images may use same-origin relative paths (including the
  // authenticated workspace routes produced by makeUrlTransform). Absolute,
  // protocol-relative, backslash-relative, and custom-scheme URLs are blocked
  // so model output can never initiate a cross-origin image request.
  if (value.startsWith("//") || value.startsWith("\\") || value.startsWith("/\\")) {
    return false;
  }
  return !/^[a-z][a-z0-9+.-]*:/i.test(value);
}

export function AgentMarkdownImage({ src, alt, node, ...rest }: MarkdownImageProps) {
  void node;
  if (!isSafeAgentImageSource(src)) {
    return createElement(
      "span",
      { className: "text-muted-foreground" },
      `[external image omitted${alt ? `: ${alt}` : ""}]`,
    );
  }
  return createElement("img", {
    ...rest,
    src: src.trim(),
    alt: alt || "",
    loading: "lazy",
    referrerPolicy: "no-referrer",
  });
}

function isExternalHTTPURL(href: string | undefined): boolean {
  if (!href || !/^https?:\/\//i.test(href)) return false;
  if (typeof window === "undefined") return true;
  try {
    return new URL(href).host !== window.location.host;
  } catch {
    return false;
  }
}

function RAGMarkdownAnchor({ href, children, node, ...rest }: MarkdownAnchorProps) {
  void node;
  const external = isExternalHTTPURL(href);
  return createElement(
    "a",
    {
      ...rest,
      href,
      ...(external ? { target: "_blank", rel: "noopener noreferrer" } : {}),
    },
    children,
  );
}

function IgnoredRAGMarkdownImage({ alt, node }: MarkdownImageProps) {
  void node;
  return createElement(
    "span",
    { className: "text-muted-foreground" },
    `[已忽略回答中的外部图片${alt ? `：${alt}` : ""}]`,
  );
}

const RAG_MARKDOWN_COMPONENTS: NonNullable<MarkdownProps["components"]> = {
  a: RAGMarkdownAnchor,
  img: IgnoredRAGMarkdownImage,
};

export function RAGAnswerMarkdown({
  children,
  urlTransform,
}: {
  children: string;
  urlTransform: NonNullable<MarkdownProps["urlTransform"]>;
}) {
  return createElement(
    ReactMarkdown,
    {
      remarkPlugins: [remarkGfm],
      components: RAG_MARKDOWN_COMPONENTS,
      urlTransform,
      skipHtml: true,
    },
    children,
  );
}

export function RAGPlainText({
  value,
  as = "span",
  className,
  title,
}: {
  value: string;
  as?: "span" | "p";
  className?: string;
  title?: string;
}): ReactNode {
  return createElement(as, { className, title }, value);
}
