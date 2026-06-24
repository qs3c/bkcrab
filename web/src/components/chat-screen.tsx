"use client";

import { useEffect, useState, useRef, useCallback, useMemo } from "react";
import { useRouter, usePathname, useSearchParams } from "next/navigation";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { getAgent, getChatHistoryWithCursor, getChatSessions, getChatTodo, getMe, listAgentFiles, listProjects, renameChatSession, revealAgentWorkspace, sendChatStream, steerChat, uploadAgentFiles, getSkills, type ChatHistoryMessage, type ChatStreamEvent, type ContextUsage, type SkillInfo, type TodoItem, type ToolResultMetadata, type WorkspaceFile } from "@/lib/api";
import { Bot, Send, Copy, Check, Pencil, Wrench, ChevronDown, ChevronRight, Download, X, File, FileText, FolderSearch, Image as ImageIcon, FileCode, Film, Music, Puzzle, SlidersHorizontal, ShieldCheck, Paperclip, Square, FolderOpen, RefreshCw, Eye, Code2, RotateCcw, ListChecks, Terminal, History } from "lucide-react";
import Link from "next/link";
import ReactMarkdown, { defaultUrlTransform } from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import { ExternalAnchor } from "@/components/markdown-link";

// react-markdown 的默认 urlTransform 会过滤不在安全列表（http、https、
// mailto、ircs、xmpp）中的协议——包括 `data:`。我们希望内联 base64 图片
// 能正常渲染，因此对其他情况回退到默认行为。
function urlTransform(url: string, key: string): string {
  if (key === "src" && url.startsWith("data:image/")) return url;
  return defaultUrlTransform(url);
}

// makeUrlTransform 构建一个 urlTransform，同时将沙箱路径
// `/workspace/<name>` 重映射到活跃智能体的认证文件 API URL。
// 产出文件的技能返回沙箱路径如 /workspace/img_xxx.png，LLM 将其写入
// `![](/workspace/...)`。docker bind-mount 是会话级的（host:
// ~/.bkclaw/workspaces/<agent>/sessions/<sid>/ ↔ container:/workspace），
// 因此 workspace.Store 在 sessions/<sid>/<name> 处找到文件。我们必须
// 预置该前缀，否则文件 API 会从智能体根目录解析并返回 404。
function makeUrlTransform(agentId: string, sessionId: string) {
  return (url: string, key: string): string => {
    if (key === "src" && url.startsWith("data:image/")) return url;
    // 将沙箱 `/workspace/<name>` 重映射，同时适用于图片嵌入（`src`）和
    // 超链接（`href`）。如果不处理 href，模型的"点击预览"链接会指向
    // 应用源而非文件 API 并返回 404。
    if ((key === "src" || key === "href") && url.startsWith("/workspace/")) {
      const rel = url.slice("/workspace/".length);
      const scoped = sessionId ? `sessions/${sessionId}/${rel}` : rel;
      return fileUrl(agentId, scoped, false);
    }
    return defaultUrlTransform(url);
  };
}

function zeroContextUsage(usage: ContextUsage | null): ContextUsage | null {
  return usage ? { ...usage, usedTokens: 0 } : null;
}

// 在 `![alt](data:image/...;base64,...)` 标记上分割字符串。
//
// 模型输出的实际内容比语法规则更凌乱：base64 载荷可能被换行/空格
// 包裹，闭合括号 `)` 有时被截断，`]` 和 `(` 可能分处不同行。因此我们
// 用正则匹配头部，但用手工扫描来消费 URL 主体——它容忍 base64 中的
// 空白和缺失的尾部 `)`。返回 [...{type:"text"|"image", ...}]。
function splitDataImages(s: string): Array<{ type: "text"; text: string } | { type: "image"; alt: string; src: string }> {
  const out: Array<{ type: "text"; text: string } | { type: "image"; alt: string; src: string }> = [];
  const headerRe = /!\[([^\]]*?)\]\s*\(\s*<?\s*(data:image\/[a-z0-9.+-]+;base64,)/gi;
  let lastIdx = 0;
  let m: RegExpExecArray | null;
  while ((m = headerRe.exec(s)) !== null) {
    const alt = m[1];
    const dataPrefix = m[2];
    // 消费 base64 主体：A–Z a–z 0–9 + / = 及任意空白。
    let cursor = m.index + m[0].length;
    while (cursor < s.length && /[A-Za-z0-9+/=\s]/.test(s[cursor])) cursor++;
    const rawBody = s.slice(m.index + m[0].length, cursor);
    const body = rawBody.replace(/\s+/g, "");
    if (!body) continue;
    const src = dataPrefix + body;
    // 跳过可选的 `>` 和 `)` 闭合符（或接受截断）。
    let after = cursor;
    while (after < s.length && /[\s>]/.test(s[after])) after++;
    if (s[after] === ")") after++;

    if (m.index > lastIdx) out.push({ type: "text", text: s.slice(lastIdx, m.index) });
    out.push({ type: "image", alt, src });
    lastIdx = after;
    headerRe.lastIndex = after;
  }
  if (lastIdx < s.length) out.push({ type: "text", text: s.slice(lastIdx) });
  return out;
}

// 模型有时会输出 `]` 和 `(` 之间带换行的 markdown 图片，或输出很长的
// base64 目标导致某些 commonmark 解析器放弃处理。与其与 markdown 解析器
// 较劲，不如提取 `![alt](data:image/...)`（容忍 `]` 和 `(` 之间的空白/
// 换行）并将其渲染为原生 <img>，其余内容交给 ReactMarkdown 处理。当
// 内容中没有 data-URL 图片时返回 null，这样调用方可以直接回退到普通的
// ReactMarkdown 渲染。当 `suppressAllInlineImages` 为 true 时，内容中
// 所有 data-URL 图片都会被丢弃（用于气泡顶部已附加工具输出图片的情况——
// 模型经常在回复中重新嵌入同一图片，而 LLM 无法逐字复现 base64，因此
// 重新嵌入的字节要么是已显示的重复，要么是渲染为不同图片的幻觉）。
// `surfacedSrcs` 为没有附件但之前的气泡已显示相同字节的情况提供更精确
// 的精确匹配。
function renderContentWithDataImages(
  content: string,
  surfacedSrcs?: ReadonlySet<string>,
  suppressAllInlineImages?: boolean,
  urlTransformFn?: (url: string, key: string) => string,
): React.ReactNode | null {
  const parts = splitDataImages(content);
  if (!parts.some((p) => p.type === "image")) return null;
  return (
    <>
      {parts.map((p, i) => {
        if (p.type === "image") {
          if (suppressAllInlineImages || surfacedSrcs?.has(p.src)) return null;
          return (
            // eslint-disable-next-line @next/next/no-img-element
            <img key={i} src={p.src} alt={p.alt} className="rounded-lg max-w-full h-auto my-2" />
          );
        }
        return (
          <ReactMarkdown key={i} remarkPlugins={[remarkGfm, remarkBreaks]} urlTransform={urlTransformFn} components={{ a: ExternalAnchor }}>
            {p.text}
          </ReactMarkdown>
        );
      })}
    </>
  );
}

import { usePageHeader } from "@/components/sidebar";
import { channelLabel } from "@/components/channel-icon";

interface ProducedFile {
  path: string; // 相对于工作区的路径
  size?: number;
}

interface UserAttachment {
  name: string;
  isImage: boolean;
  // 用于即时气泡内预览的本地 blob URL，无需服务器往返。
  // 仅在实时发送时设置；重新加载的历史记录不会携带它。
  previewUrl?: string;
}

// 内置斜杠命令，在输入框的 `/` 菜单中与技能一起展示。
// 与 internal/agent/slash.go 中的调度表镜像——在该文件中添加/
// 删除/重命名命令时需保持同步。
type SlashCommand = { name: string; description: string };
const BUILTIN_COMMANDS: SlashCommand[] = [
  { name: "new", description: "清空会话历史" },
  { name: "reset", description: "清空会话历史" },
  { name: "retry", description: "重新运行上一条消息" },
  { name: "undo", description: "撤销上一轮" },
  { name: "compact", description: "压缩上下文窗口" },
  { name: "status", description: "智能体状态和记忆信息" },
  { name: "usage", description: "会话令牌和轮次统计" },
  { name: "insights", description: "活动洞察（最近 N 天）" },
  { name: "personality", description: "查看或切换人格" },
  { name: "model", description: "查看或切换大模型" },
  { name: "goal", description: "持续的多轮目标" },
  { name: "plan", description: "仅拟定计划，不运行工具" },
  { name: "help", description: "显示命令帮助" },
  { name: "version", description: "显示版本" },
];
type SlashItem =
  | ({ kind: "command" } & SlashCommand)
  | ({ kind: "skill" } & SkillInfo);

interface ChatMessage {
  id: string;
  role: "user" | "agent" | "tool-group";
  content: string;
  timestamp: number;
  toolCalls?: { id: string; name: string; arguments: string; result?: string; metadata?: ToolResultMetadata }[];
  files?: ProducedFile[];
  attachments?: UserAttachment[];
  // 乐观渲染的转向气泡，等待服务器持久化的"steer"回显。仅用于与
  // 该回显去重（匹配后清除）——不做差异化渲染。
  pendingSteer?: boolean;
  // 助手侧元数据（例如迭代上限徽章）。从实时内容事件的 `metadata`
  // 载荷或刷新时的 ChatHistoryMessage.metadata 中写入。
  metadata?: ToolResultMetadata;
  // IM 桥接发送者身份，从 session_messages 元数据中提取（由智能体循环
  // 为 Discord/Telegram/... 路由的轮次设置）。存在此字段表示：渲染
  // 头像 + 昵称标题，而非匿名"你"气泡——消息来自与机器人对话的第三方，
  // 而非智能体拥有者本人。
  sender?: {
    name: string;
    avatarUrl?: string;
    id?: string;
    channel?: string;
  };
}

// 应用于每个聊天气泡 markdown 包装器（助手 + 用户）的 Tailwind 类字符串。
// 原始 `prose prose-sm` 默认样式将 markdown 渲染为长文风格——H1/H2 巨大、
// 标题有大的上下边距、表格/代码块引入额外的纵向内边距。在对话气泡中这些
// 默认值会让带 `## 小节` 的回复与周围纯文本严重不成比例。此覆盖将标题
// 限制为略大于正文字号、收紧表格单元格内边距、缩小各块元素上方间距，
// 使气泡读起来像一个连贯消息。
const CHAT_PROSE_CLASS =
  "text-[15px] leading-relaxed prose prose-sm max-w-none dark:prose-invert " +
  "prose-p:my-1 prose-pre:my-2 prose-ul:my-1 prose-ol:my-1 " +
  "prose-headings:font-semibold prose-headings:mt-3 prose-headings:mb-1 " +
  "prose-h1:text-[16px] prose-h2:text-[15.5px] prose-h3:text-[15px] " +
  "prose-h4:text-[15px] prose-h5:text-[15px] prose-h6:text-[15px] " +
  "prose-table:my-2 prose-table:text-[14px] " +
  "prose-th:py-1 prose-th:px-2 prose-td:py-1 prose-td:px-2 " +
  "prose-hr:my-3";

// 智能体发出的分词标记，用于请求多气泡回复——必须与
// internal/channels/base.go 中的 channels.SplitMessageMarker 匹配。
// 在 IM 通道上，分发器（manager.dispatchOutbound）以此标记将出站
// 文本拆分为多条平台消息；Web UI 将每个分块渲染为单独气泡以匹配体验。
const SPLIT_MARKER = "<|split|>";

// splitOnMarker 在 SPLIT_MARKER 处分割 `s`，修剪每个分块并丢弃空块。
// 在渲染时使用，使包含该标记的流式助手回复变为多个气泡，无需上游
// 内容事件重写。
function splitOnMarker(s: string): string[] {
  if (!s.includes(SPLIT_MARKER)) return [s];
  const parts = s.split(SPLIT_MARKER).map((p) => p.trim()).filter((p) => p.length > 0);
  return parts.length > 0 ? parts : [s];
}

// 路由到智能体主目录（而非工作区）的单段身份文件名——从
// "你的文件"面板中排除。
const SYSTEM_FILES = new Set([
  "SOUL.md", "IDENTITY.md", "USER.md", "BOOTSTRAP.md",
  "MEMORY.md", "HEARTBEAT.md", "AGENTS.md", "TOOLS.md", "agent.json",
]);

function isSystemFile(path: string): boolean {
  return !path.includes("/") && SYSTEM_FILES.has(path);
}

function parseWrittenSize(result: string): number | undefined {
  const m = result.match(/^Written (\d+) bytes/);
  return m ? parseInt(m[1], 10) : undefined;
}

interface ChatSession {
  id: string;
  title?: string;
  preview: string;
  // channel/accountId/chatId 随列表一起传递，以便聊天页面判断
  // 是否允许在此会话中撰写（仅 `web` 允许——IM 通道没有反向发送路径）。
  channel?: string;
  accountId?: string;
  chatId?: string;
}

function generateSessionId() {
  return `s-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

/** 将原始历史消息转换为 UI ChatMessages，将工具调用与结果分组。 */
function buildChatMessages(history: ChatHistoryMessage[]): ChatMessage[] {
  const msgs: ChatMessage[] = [];
  let i = 0;
  while (i < history.length) {
    const h = history[i];
    if (h.role === "user") {
      // 在历史加载的用户气泡上展示图片附件。服务器在 ContentParts 包含
      // image_url 块的用户轮次上发出 `imageUrls`；将它们映射为
      // UserAttachment 条目，使现有气泡渲染器（实时发送路径）无需
      // 额外分支即可处理。
      const attachments: UserAttachment[] | undefined =
        h.imageUrls && h.imageUrls.length > 0
          ? h.imageUrls.map((url, idx) => ({
              name: `image-${idx + 1}`,
              isImage: true,
              previewUrl: url,
            }))
          : undefined;
      const sender = h.senderName
        ? {
            name: h.senderName,
            avatarUrl: h.senderAvatarUrl,
            id: h.senderId,
            channel: h.senderChannel,
          }
        : undefined;
      msgs.push({ id: `h-${i}`, role: "user", content: h.content || "", timestamp: 0, attachments, sender });
      i++;
    } else if (h.role === "assistant" && h.toolCalls && h.toolCalls.length > 0) {
      // 分组：assistant tool_calls + 后续 tool results + 最终 assistant content
      const calls = h.toolCalls.map((tc) => ({
        ...tc,
        result: undefined as string | undefined,
        metadata: undefined as ToolResultMetadata | undefined,
      }));
      i++;
      // 收集工具结果
      while (i < history.length && history[i].role === "tool") {
        const toolMsg = history[i];
        const call = calls.find((c) => c.id === toolMsg.toolCallId);
        if (call) {
          call.result = toolMsg.content;
          if (toolMsg.metadata) call.metadata = toolMsg.metadata;
        }
        i++;
      }
      // 防御性处理：在离开工具结果序列时，如果某个 tool_use 仍没有结果，
      // 说明它被孤立了（客户端中止、服务端中途崩溃、持久化路径竞争）。
      // 将它们标记为已停止，使 UI 显示终止状态而非永远旋转。
      // 较新的轮次已在服务端通过 padOrphanToolResults 填充；此处
      // 捕获早于该修复的会话。
      for (const c of calls) {
        if (c.result === undefined) {
          c.result = "(stopped)";
        }
      }
      // 如果此助手轮次在工具调用的同时产出了文本（常见于"最终答案 + 收尾
      // 工具"模式如 text + update_goal），将该文本作为独立的助手气泡渲染
      // 在工具组之前，而非折叠进工具组的 content。折叠后正文会变成折叠
      // 工具块的前言；拆分后模型的实际答案则成为一等公民回复。
      if (h.content) {
        msgs.push({ id: `h-pre-${i}`, role: "agent", content: h.content, timestamp: 0, metadata: h.metadata });
      }
      msgs.push({
        id: `h-tool-${i}`,
        role: "tool-group",
        content: "",
        timestamp: 0,
        toolCalls: calls,
      });
      // 如果下一条是仅含 content 且无工具调用的助手消息（最终答案），则添加。
      // 必须跳过下一条助手消息也含工具调用的情况——在多轮对话中那是下一个
      // 工具组的*开始*，而非最终答案，在此消费会丢弃其工具调用并使后续
      // 工具结果消息成为孤立。
      if (
        i < history.length &&
        history[i].role === "assistant" &&
        history[i].content &&
        !(history[i].toolCalls && history[i].toolCalls!.length > 0)
      ) {
        msgs.push({ id: `h-${i}`, role: "agent", content: history[i].content || "", timestamp: 0, metadata: history[i].metadata });
        i++;
      }
    } else if (h.role === "assistant") {
      msgs.push({ id: `h-${i}`, role: "agent", content: h.content || "", timestamp: 0, metadata: h.metadata });
      i++;
    } else {
      i++; // 跳过意外的消息
    }
  }
  return msgs;
}

// isPendingPlanContent 识别模型在计划优先轮次中发出的收尾行（"回复 `go` 执行"
// 或其中文形式）。用于决定是否在助手气泡下方显示内联的确认/取消按钮。
// 松散匹配——模型措辞会漂移，但信号总是在最后几行中出现 "go" 附近的
// "execute / 执行"。
function isPendingPlanContent(content: string): boolean {
  if (!content) return false;
  // 仅检查尾部，以免第 3 步中出现 "go" 的冗长计划误报——模型只在
  // 收尾时发出提示行，从不在开头。
  const lines = content.split("\n");
  const tail = lines.slice(Math.max(0, lines.length - 4)).join(" ");
  // 英语："Reply `go` to execute" / "Reply 'go' to run"
  if (/reply[^.]*?\bgo\b[^.]*?(execute|run)/i.test(tail)) return true;
  // 中文："请回复 go 开始执行" / "回复 go 执行"
  if (/回复[^。]*?go[^。]*?(执行|开始)/.test(tail)) return true;
  // 通用安全网：收尾祈使句中 `go` + execute 关键词非常接近。
  // 比单独出现 "go" 更严格。
  if (/[`'"]go[`'"][^\n]{0,40}(execute|执行)/i.test(tail)) return true;
  return false;
}

// 从路径名解析路由 ID。ChatScreen 在智能体布局层级挂载一次，
// 并在这些路由间保持活跃：
//
//   /agents/<aid>/                         — 新的松散聊天
//   /agents/<aid>/chat/                    — 新的松散聊天
//   /agents/<aid>/chat/<session>           — 按 ID 打开现有聊天
//   /agents/<aid>/project/<pid>            — 项目内的新聊天
//
// 从 `usePathname()` 读取（而非从页面树接受 props）
// TodoPanel 将智能体维护的每会话 todo.md 渲染为对话上方的实时
// 进度清单。"当前步骤"是第一个未勾选项——我们将其显示为带
// "<n>/<total>" 计数器的单行，完整列表点击展开。无条目时完全
// 隐藏（由调用方负责——保持此纯组件的简洁）。
function TodoPanel({ items, active }: { items: TodoItem[]; active: boolean }) {
  const [open, setOpen] = useState(true);
  const total = items.length;
  const doneCount = items.filter((i) => i.done).length;
  const allDone = doneCount === total;
// 第一个未勾选项是实时"进行中"步骤。清单全部勾选后，回退到
    // 最后一项，使折叠标题仍显示具体内容。
  const currentIdx = allDone ? total - 1 : items.findIndex((i) => !i.done);
  const current = currentIdx >= 0 ? items[currentIdx] : null;
  return (
// 包装器将面板与输入框的 max-w-2xl 列对齐。仅内部 div 携带
        // 边框/背景，使周围聊天区域保持简洁——没有横跨页面的全宽条带。
    <div className="shrink-0 px-4 pt-2">
      <div className="mx-auto max-w-2xl">
        <div className="rounded-lg border border-border bg-muted/40 px-3 py-2 shadow-sm">
          <button
            type="button"
            onClick={() => setOpen((v) => !v)}
            className="flex w-full items-center gap-2 text-left text-sm"
            aria-expanded={open}
          >
            {allDone ? (
              <Check className="size-4 shrink-0 text-emerald-600" />
            ) : active ? (
              <div className="size-4 shrink-0 rounded-full border-2 border-amber-500 border-t-transparent animate-spin" />
            ) : (
              // 暂停：智能体未在流式输出。显示静态琥珀色圆环，使"当前位置"
                // 提示可见，但不会暗示正在进行中。
              <div className="size-4 shrink-0 rounded-full border-2 border-amber-500/70" />
            )}
            <span className="font-medium tabular-nums text-muted-foreground">
              {doneCount}/{total}
            </span>
            <span className="truncate flex-1">
              {current ? current.text : "计划清单"}
            </span>
            {open ? (
              <ChevronDown className="size-4 shrink-0 text-muted-foreground" />
            ) : (
              <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
            )}
          </button>
          {open && (
            <ol className="mt-2 space-y-1 border-t border-border/60 pt-2 text-sm">
              {items.map((it, i) => {
                const isCurrent = i === currentIdx && !it.done;
                return (
                  <li
                    key={i}
                    className={
                      "flex items-start gap-2 rounded px-1.5 py-0.5 " +
                      (isCurrent ? "bg-amber-500/10" : "")
                    }
                  >
                    {it.done ? (
                      <Check className="mt-0.5 size-3.5 shrink-0 text-emerald-600" />
                    ) : isCurrent ? (
                      active ? (
                        <div className="mt-0.5 size-3.5 shrink-0 rounded-full border-2 border-amber-500 border-t-transparent animate-spin" />
                      ) : (
                        <div className="mt-0.5 size-3.5 shrink-0 rounded-full border-2 border-amber-500/70" />
                      )
                    ) : (
                      <div className="mt-1 size-2.5 shrink-0 rounded-full border border-muted-foreground/40" />
                    )}
                    <span
                      className={
                        (it.done ? "line-through text-muted-foreground/70 " : "") +
                        (isCurrent ? "font-medium" : "")
                      }
                    >
                      {it.text}
                    </span>
                  </li>
                );
              })}
            </ol>
          )}
        </div>
      </div>
    </div>
  );
}

// 页面树）使组件实例在侧边栏导航中保持存活——sessionId /
// projectId 变为响应式值，原地更新而非阻止重新挂载。
function parseAgentRoute(pathname: string): {
  sessionId: string;
  projectId: string;
} {
  const sessMatch = pathname.match(/^\/agents\/[^/]+\/chat\/([^/]+)/);
  if (sessMatch) {
    const sid = sessMatch[1];
    // "_" 是 Next 在 output:'export' 下为动态 [session] 段发出的
    // 构建时占位符。将其视为"无会话"。
    return { sessionId: sid === "_" ? "" : sid, projectId: "" };
  }
  const projMatch = pathname.match(/^\/agents\/[^/]+\/project\/([^/]+)/);
  if (projMatch) {
    const pid = projMatch[1];
    return { sessionId: "", projectId: pid === "_" ? "" : pid };
  }
  return { sessionId: "", projectId: "" };
}

export function ChatScreen() {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  // 当 URL 中包含 `?actAs=<uid>` 时，表示超级管理员正在查看另一个
  // 用户的会话（中间件设为只读）。强制输入框进入禁用状态并显示横幅，
  // 以防管理员尝试输入后静默收到 403。
  const actAsUserId = searchParams?.get("actAs") || "";
  const isActAsView = !!actAsUserId;
  const { sessionId: urlSessionId, projectId: urlProjectId } = useMemo(
    () => parseAgentRoute(pathname || ""),
    [pathname],
  );
  // 响应式：从路径名重新派生，使切换智能体（侧边栏下拉、浏览器
  // 前进/后退）立即更新下游请求。之前 useState(() => ...) 的写法在
  // 挂载时冻结了 id，导致后台加载继续请求旧智能体而面板在新 URL 下
  // 显示过期历史。
  const selectedAgent = useAgentIdFromURL();
  const [agentName, setAgentName] = useState<string>("");
  // urlProjectId 的已解析元数据，在 /agents/<aid>/project/<pid> 的
  // 空状态信息卡中展示。获取前为 null；卡片在加载期间隐藏而非闪烁
  // 占位符。
  const [projectInfo, setProjectInfo] = useState<{
    id: string;
    name: string;
    description?: string;
    updatedAt?: string;
    createdAt?: string;
  } | null>(null);
  const [sessionId, setSessionId] = useState<string>(
    () => urlSessionId || generateSessionId(),
  );
  const [sessions, setSessions] = useState<ChatSession[]>([]);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState("");
  // 活跃会话集合：每个有进行中轮次的 sessionId 一个条目。此前是单个
  // `sending` 布尔，但整个 agent 只有一个 ChatScreen 实例（见
  // layout-client），单标志会跨会话误锁——A 流式进行时切到/新建 B 也无法
  // 发送。改为按会话隔离后，`sending`（下方派生）只反映当前显示的会话。
  // activeSessionsRef 是供异步流回调读取最新值的镜像，避免闭包陈旧。
  const [activeSessions, setActiveSessions] = useState<Set<string>>(() => new Set());
  const activeSessionsRef = useRef<Set<string>>(activeSessions);
  const markActive = useCallback((sid: string, on: boolean) => {
    setActiveSessions((prev) => {
      const next = new Set(prev);
      if (on) next.add(sid);
      else next.delete(sid);
      activeSessionsRef.current = next;
      return next;
    });
  }, []);
  // 当前显示会话是否有进行中轮次。所有现有 UI（发送/停止按钮、输入框禁用
  // 态、TodoPanel active、isLiveTurn、停止/转向逻辑等）继续读它，因此自动
  // 随显示会话切换，无需逐处改动。
  const sending = activeSessions.has(sessionId);
  // 单次组合器模式：下一次常规发送将生成工具禁用的计划，然后重置，
  // 以便后续轮次可以执行它。
  const [planMode, setPlanMode] = useState(false);
  // 当前会话的 todo.md 状态——智能体维护此文件，我们在每次
  // 涉及 todo.md 的 write_file/edit_file 事件及挂载时重新获取。
  // 空的 `items` 隐藏面板。
  const [todoItems, setTodoItems] = useState<TodoItem[]>([]);
  // 当前活跃 delegate_task 运行的最近 subagent_progress 事件。
  // 当子智能体报告 phase="done" 或发送轮次关闭时清除，因此不会跨轮次
  // 残留。一次只运行一个子智能体（delegate_task 以串行方式注册），
  // 无需按 tool_call_id 分键。
  const [subagentProgress, setSubagentProgress] = useState<null | {
    iteration?: number;
    max?: number;
    phase?: "thinking" | "running" | "final-delivery" | "done";
    tools?: string[];
  }>(null);
  const [copiedId, setCopiedId] = useState<string | null>(null);
  // 后端历史响应与 usage/done 事件汇报的上下文占用。用于在输入框页脚渲染
  // 「已用上下文百分比」指示器。
  const [contextUsage, setContextUsage] = useState<ContextUsage | null>(null);
  // 后端正在同步压缩上下文时为 true（compaction active 事件）。驱动消息流
  // 末尾的「正在压缩上下文…」横杠。压缩在轮次开始、产出任何回复前进行，
  // 是个阻塞等待，因此需要单独提示而非复用打字指示器。
  const [compacting, setCompacting] = useState(false);
  const [filesSheetOpen, setFilesSheetOpen] = useState(false);
  const [sessionTitle, setSessionTitle] = useState<string>("");
  const [attachments, setAttachments] = useState<File[]>([]);
  // 灯箱：用于点击附件缩略图（输入框）或已发送消息气泡中的内联图片。
  // `null` = 关闭。
  const [lightboxSrc, setLightboxSrc] = useState<string | null>(null);
  // 输入框中图片附件的 Object URL。以文件索引为键，以便在移除时
  // 撤销而无需在每次按键时为所有预览重新计算。当 `attachments` 变化时
  // 重新派生。
  const attachmentPreviews = useMemo(
    () =>
      attachments.map((f) =>
        f.type.startsWith("image/") ? URL.createObjectURL(f) : null,
      ),
    [attachments],
  );
  useEffect(() => {
    return () => {
      for (const url of attachmentPreviews) if (url) URL.revokeObjectURL(url);
    };
  }, [attachmentPreviews]);
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const messagesScrollRef = useRef<HTMLDivElement>(null);
  // 仅当用户已固定在底部时自动滚动。用户上滚后立即切换为 false，
  // 使流式助手输出不会在阅读中途将其拉回底部；用户回到底部
  //（或点击"滚动到最新"按钮）后翻回 true。
  const stickToBottomRef = useRef(true);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // 去重：同时在活跃 POST 流和并行 /api/chat/subscribe SSE 上到达
  // 的事件——两者在服务端订阅同一个 chat-events hub。跟踪当前会话中
  // 已渲染的最高 seq。
  const maxSeqRef = useRef<number>(-1);
  // 传递给 /api/chat/subscribe 的恢复游标，使刚重新加载的页面仅
  // 重放其未见的增量。从聊天历史端点捕获，在 sessionId 变化时刷新。
  const subscribeSinceRef = useRef<number>(-1);
  // 从订阅重放内容事件创建的临时助手气泡（即用户在轮次中途重新加载
  // 并正在追赶时）。在下一次历史重新加载时清除，届时会用从
  // session_messages 拉取的规范消息替换占位符。
  const transientBubbleIdRef = useRef<string | null>(null);
// streamingMsgIdRef 保存当前通过活跃 POST sendChatStream 累积
  // content_delta 块的助手气泡 id。与并行 /api/chat/subscribe SSE
  // 处理器共享，以便该处理器能检测到"POST 流拥有此轮次"并跳过
  // 尾随 `content` 事件的自身气泡创建路径——两个处理器从同一 hub
  // 看到相同的 content 事件，如果 subscribe 先到达，去重-by-seq
  // 守卫不会在 POST 端触发，导致产生重复气泡。此 ref 在
  // startNewGroup 和 tool_call 将气泡卷入工具组时重置为 null。
  const streamingMsgIdRef = useRef<string | null>(null);
  // 每个进行中轮次的 AbortController，按 sessionId 存储。使停止按钮只取消
  // *当前显示*会话的轮次，且多个会话可各自拥有独立的可中止流。
  const abortsRef = useRef<Map<string, AbortController>>(new Map());
  // currentSessionIdRef 始终持有当前显示的 sessionId。后台轮次（用户中途
  // 切到别的会话）的 POST 流回调据此判断自己是否仍是前台会话——不是则停止
  // 写入共享视图状态，避免污染用户正在看的另一个会话。
  const currentSessionIdRef = useRef(sessionId);
  useEffect(() => {
    currentSessionIdRef.current = sessionId;
  }, [sessionId]);

  // 控制 EventSource 效果的门：保存其历史已被获取且 subscribeSinceRef
  // 现在准确的 sessionId。没有这个门，SSE 效果（声明更早因此先运行）
  // 会在历史获取解析之前打开 `/api/chat/subscribe?since=-1`——服务端
  // 这意味着"重放每一个 session_event"，对于深度研究的聊天，重放会
  // 泛洪客户端并占据 EventSource 的 HTTP 连接槽。快速项目↔聊天导航
  // 堆叠几个这样的连接，浏览器的 6 连接/源限制会将所有请求钉在
  // `pending`，页面变白，点击不再响应。存储已加载的 sessionId
  //（而不仅是布尔值）能在 sessionId 于历史追上之前再次变化时保持门控准确。
  const [loadedSessionId, setLoadedSessionId] = useState<string | null>(null);

// 斜杠命令菜单状态。当输入框中光标位置存在以 `/` 开头的标记时打开
  // 菜单；选择技能会将该标记替换为 `/<skill-name> `，光标置于空格后
  // 以便用户继续输入提示词。
  const [skills, setSkills] = useState<SkillInfo[]>([]);
  const [slashOpen, setSlashOpen] = useState(false);
  const [slashQuery, setSlashQuery] = useState("");
  const [slashIndex, setSlashIndex] = useState(0);

  useEffect(() => {
    getSkills().then(setSkills).catch(() => setSkills([]));
  }, []);

// 活跃智能体变化时重置聊天面板状态。否则从侧边栏切换智能体会短暂
  // 在新 URL 下渲染旧智能体的历史/会话/标题，直到下方加载数果替换它们。
  // 提早清除比在每个渲染中传递"加载中"标志更经济。我们刻意保留 `input`，
  // 使半输入的消息在意外切换智能体后不会丢失。
  const lastAgentRef = useRef(selectedAgent);
  useEffect(() => {
    if (lastAgentRef.current === selectedAgent) return;
    lastAgentRef.current = selectedAgent;
    setMessages([]);
    setSessions([]);
    setSessionTitle("");
    setAgentName("");
    setAttachments([]);
  }, [selectedAgent]);

// 一次性解析智能体显示名称。聊天标题和未来可能的头部信息应显示
  // "Chat with My Helper" 而非不透明的 agt_xxx id。使用 /api/agents/{id}
  // （管理员或超级管理员），使管理员查看另一个用户的智能体仍能获得
  // 真实名称——/api/agents（列表）仅限拥有者，会遗漏。
  useEffect(() => {
    if (!selectedAgent) return;
    let aborted = false;
    getAgent(selectedAgent)
      .then((a) => {
        if (aborted) return;
        setAgentName(a?.name || a?.id || selectedAgent);
      })
      .catch(() => {
        if (!aborted) setAgentName(selectedAgent);
      });
    return () => {
      aborted = true;
    };
  }, [selectedAgent]);

// 当 URL 指向项目的空新聊天状态时，解析项目名称作为主标题。
  // 通过 listProjects 获取足够便宜——每个（用户、智能体）对的项目
  // 数量少，且侧边栏已预热了网络缓存。
  useEffect(() => {
    if (!selectedAgent || !urlProjectId) {
      setProjectInfo(null);
      return;
    }
    let aborted = false;
    listProjects(selectedAgent)
      .then((list) => {
        if (aborted) return;
        const p = list.find((x) => x.id === urlProjectId);
        setProjectInfo(p ?? null);
      })
      .catch(() => {
        if (!aborted) setProjectInfo(null);
      });
    return () => {
      aborted = true;
    };
  }, [selectedAgent, urlProjectId]);

// 检测光标是否在 /token 内部，如果是，则检测斜杠后输入了什么。
  // 运行成本足够低，可以在每次按键时执行。
  const slashContext = (value: string, caret: number): { start: number; query: string } | null => {
    const before = value.slice(0, caret);
    // 要求斜杠位于消息开头或前面有空白，以便包含斜杠的路径/URL
    // 不会触发菜单。
    const match = /(^|\s)\/([\w-]*)$/.exec(before);
    if (!match) return null;
    return { start: caret - match[2].length - 1, query: match[2] };
  };

  // 合并的命令 + 技能列表用于斜杠菜单。命令在前以便快速找到内置项；
  // 查询同时匹配名称和描述。上限 8 以保持弹出菜单不超出输入框。
  const filteredItems: SlashItem[] = slashOpen
    ? (() => {
        const q = slashQuery.toLowerCase();
        const match = (name: string, desc: string) =>
          !q || name.toLowerCase().includes(q) || desc.toLowerCase().includes(q);
        const cmds: SlashItem[] = BUILTIN_COMMANDS
          .filter((c) => match(c.name, c.description))
          .map((c) => ({ kind: "command", ...c }));
        const sks: SlashItem[] = skills
          .filter((s) => match(s.name, s.description || ""))
          .map((s) => ({ kind: "skill", ...s }));
        return [...cmds, ...sks].slice(0, 8);
      })()
    : [];

  const selectItem = useCallback(
    (item: SlashItem) => {
      const el = textareaRef.current;
      if (!el) return;
      const caret = el.selectionStart ?? input.length;
      const ctx = slashContext(input, caret);
      if (!ctx) return;
      const before = input.slice(0, ctx.start);
      const after = input.slice(caret);
      const insert = `/${item.name} `;
      const next = before + insert + after;
      setInput(next);
      setSlashOpen(false);
      setSlashQuery("");
      setSlashIndex(0);
      requestAnimationFrame(() => {
        const pos = before.length + insert.length;
        el.focus();
        el.setSelectionRange(pos, pos);
      });
    },
    [input],
  );

  // 智能体变化时加载会话
  const loadSessions = useCallback((agentId: string) => {
    getChatSessions(agentId)
      .then((list) => setSessions(list || []))
      .catch(() => setSessions([]));
  }, []);

  useEffect(() => {
    if (!selectedAgent) return;
    loadSessions(selectedAgent);
  }, [selectedAgent, loadSessions]);

// 实时 + 重放订阅。两个任务：
    //
    //   1. 由定时任务（及其他异步）触发的纯文本消息，通过服务器的
    //      WebChannel 路由——以 { text } 形式到达，追加为新的助手气泡。
    //
    //   2. 用户刷新时对正在进行的轮次进行恢复重放。服务器重放
    //      seq > since 的 chat_events，然后保持连接以接收新事件。
    //      以 ChatStreamEvent 形状（{ seq, type, data }）到达。
    //
    // 通过跳过 seq <= maxSeqRef 的事件，在此连接和并行 POST
    // sendChatStream（两者服务端订阅同一 hub）之间去重。
  useEffect(() => {
    if (!selectedAgent || !sessionId) return;
// 等待此 sessionId 的历史获取落地后再打开 SSE——参见
      // loadedSessionId 注释中此措施防止的浏览器连接池故障模式。
    if (loadedSessionId !== sessionId) return;
    const since = subscribeSinceRef.current;
    const url = `/api/chat/subscribe?agentId=${encodeURIComponent(selectedAgent)}&sessionId=${encodeURIComponent(sessionId)}&since=${since}`;
    const es = new EventSource(url, { withCredentials: true });
    es.onmessage = (ev) => {
      let data: {
        seq?: number;
        type?: string;
        text?: string;
        data?: {
          content?: string;
          message?: string;
          metadata?: ToolResultMetadata;
          // 子智能体进度字段
          iteration?: number;
          max?: number;
          phase?: "thinking" | "running" | "final-delivery" | "done";
          tools?: string[];
          // usage/done 事件携带的上下文占用
          usage?: ContextUsage;
          // compaction 事件：true=开始压缩，false=结束
          active?: boolean;
        };
      };
      try {
        data = JSON.parse(ev.data);
      } catch {
        return;
      }
      // 形态 A：ChatStreamEvent（进行中轮次的增量）。
      if (typeof data.type === "string") {
        const seq = typeof data.seq === "number" ? data.seq : -1;
        if (seq >= 0 && seq <= maxSeqRef.current) return; // already rendered via POST stream
// 注意：不要在 switch 之前提前更新 maxSeqRef。此处理器在追赶阶段
          // 有意丢弃 tool_call / tool_result（后续的历史重新加载会正确渲染它们）
          // ——但提前 bump 会将这些 seq 标记为"已渲染"，并行 POST sendChatStream
          // 回调在尝试渲染时会去重跳过这些事件。仅在真正接管此 seq 的 case
          // 内更新。
        const claim = () => {
          if (seq >= 0) maxSeqRef.current = seq;
        };
        switch (data.type) {
          case "content": {
            const content = data.data?.content || "";
            const meta = data.data?.metadata;
            if (!content && !meta) break;
// 活跃的 POST sendChatStream 正通过 content_delta 渲染此轮次到
              // streamingMsgIdRef。两个订阅监听同一 hub，因此 `content`
              // 事件会同时到达两个处理器；如果 subscribe 先到达，
              // 会创建重复的临时气泡。POST 处理器正在流式传输时退出，
              // 但不要 claim 这个 seq：真正渲染它的是 POST 回调，提前
              // claim 会让 POST 因去重跳过最终 content，从而丢掉 metadata。
              //
              // 如果 metadata 已经到达，先贴到当前流式气泡上；POST 稍后
              // 再处理一次也是幂等的。仅含元数据的事件（无内容）仍需
              // 通过，以便强制最终交付的追溯标记生效。
            if (streamingMsgIdRef.current && content) {
              if (meta) {
                const id = streamingMsgIdRef.current;
                setMessages((prev) => {
                  const idx = prev.findIndex((m) => m.id === id);
                  if (idx < 0) return prev;
                  const updated = [...prev];
                  updated[idx] = { ...updated[idx], metadata: { ...updated[idx].metadata, ...meta } };
                  return updated;
                });
              }
              break;
            }
            claim();
            setMessages((prev) => {
              if (transientBubbleIdRef.current) {
                const idx = prev.findIndex((m) => m.id === transientBubbleIdRef.current);
                if (idx >= 0) {
                  const updated = [...prev];
                  updated[idx] = {
                    ...updated[idx],
                    content: (updated[idx].content || "") + content,
                    metadata: meta ? { ...updated[idx].metadata, ...meta } : updated[idx].metadata,
                  };
                  return updated;
                }
              }
              // 仅含元数据的追溯标记事件且无临时气泡：附加到最近的助手气泡，
              // 使徽章在活跃的订阅会话中持续保留。
              if (!content && meta) {
                for (let i = prev.length - 1; i >= 0; i--) {
                  if (prev[i].role === "agent") {
                    const updated = [...prev];
                    updated[i] = { ...updated[i], metadata: { ...updated[i].metadata, ...meta } };
                    return updated;
                  }
                }
                return prev;
              }
              const id = `resume-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;
              transientBubbleIdRef.current = id;
              return [...prev, { id, role: "agent", content, timestamp: Date.now(), metadata: meta }];
            });
            break;
          }
          case "error": {
            claim();
            const msg = data.data?.message || "未知错误";
            setMessages((prev) => [
              ...prev,
              { id: `e-${Date.now()}`, role: "agent", content: `Error: ${msg}`, timestamp: Date.now() },
            ]);
            break;
          }
          case "subagent_progress": {
            claim();
            if (data.data?.phase === "done") {
              setSubagentProgress(null);
            } else {
              setSubagentProgress({
                iteration: data.data?.iteration,
                max: data.data?.max,
                phase: data.data?.phase,
                tools: data.data?.tools,
              });
            }
            break;
          }
          case "steer": {
            claim();
            applySteerEvent(data.data?.content || "");
            break;
          }
          case "compaction": {
            claim();
            setCompacting(!!data.data?.active);
            break;
          }
          case "usage": {
            claim();
            if (data.data?.usage) setContextUsage(data.data.usage);
            break;
          }
          case "done": {
            claim();
            if (data.data?.usage) setContextUsage(data.data.usage);
            setCompacting(false);
            // 防御性清理——content 事件应已封口流式气泡，但在 content 事件
            // 到达之前出错的轮次会使 ref 悬空，导致下一轮的首个
            // content_delta 写入过期的 id。
            streamingMsgIdRef.current = null;
            // 仅当通过订阅重放内容事件实际构建了临时气泡时才重新加载历史
            //（即用户在轮次中途重新加载，需要将占位符替换为 session_messages
            // 中保存的规范消息）。当活跃 POST 流直接渲染了该轮次时，
            // 临时气泡为 null——此时重新加载会覆盖任何已渲染的错误气泡，
            // 因为 LLM 错误轮次从不向 session_messages 写入助手消息。
            if (transientBubbleIdRef.current) {
              transientBubbleIdRef.current = null;
              getChatHistoryWithCursor(selectedAgent, sessionId)
                .then(({ history, latestEventSeq, contextUsage }) => {
                  if (latestEventSeq > maxSeqRef.current) maxSeqRef.current = latestEventSeq;
                  subscribeSinceRef.current = latestEventSeq;
                  setContextUsage(contextUsage);
                  setMessages(buildChatMessages(history));
                })
                .catch(() => {});
            }
            // 通知侧边栏刷新——新轮次可能产生了更新后的会话标题。
            if (typeof window !== "undefined") {
              window.dispatchEvent(
                new CustomEvent("bkclaw:sessions-changed", {
                  detail: { agentId: selectedAgent },
                }),
              );
            }
            break;
          }
          // 追赶期间的 tool_call / tool_result 在此跳过——
          // 下一次历史重新加载（在 `done` 时）会通过 buildChatMessages
          // 正确渲染它们。
        }
        return;
      }
      // 形态 B：传统 WebChannel { text } ——定时任务触发的异步消息。
      const text = data.text || "";
      if (!text) return;
      setMessages((prev) => [
        ...prev,
        {
          id: `async-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`,
          role: "agent",
          content: text,
          timestamp: Date.now(),
        },
      ]);
    };
    es.onerror = () => {
      // EventSource 在瞬态错误时自动重连；仅在卸载时关闭。
      // 持续的 404（会话已删除、智能体不存在）会不断重试但无害。
    };
    return () => {
      es.close();
    };
  }, [selectedAgent, sessionId, loadedSessionId]);

  // 当 URL 在底层变化时响应式切换 sessionId。
  // 三个 URL 转换涉及，均由同一分支逻辑处理：
  //   - /chat/A → /chat/B               ：采用新会话 id
  //   - /chat/A → /chat/ 或 /project/P  ：生成新 id，清空消息
  //   - /chat/  → /chat/A               ：采用该会话 id
  // `prevHadSessionRef` 防止初始挂载在 useState() 已选取的 id 之上
  // 重新生成，并防止连续两个无会话 URL（如 /chat/ → /project/P）
  // 反复生成 id。消息在任意 id 变化时清除，使原地切换不会在新历史
  // 获取期间短暂显示前一个聊天的内容。
  const prevHadSessionRef = useRef(false);
  useEffect(() => {
    if (urlSessionId) {
      prevHadSessionRef.current = true;
      if (urlSessionId !== sessionId) {
        setSessionId(urlSessionId);
        setMessages([]);
      }
      return;
    }
    if (prevHadSessionRef.current) {
      prevHadSessionRef.current = false;
      setSessionId(generateSessionId());
      setMessages([]);
    }
  }, [urlSessionId, sessionId]);

  // 保持本地 sessionTitle 与会话列表同步。未知会话
  // （全新、尚未保存）回退为空，使标题可渲染"新聊天"。
  useEffect(() => {
    const s = sessions.find((x) => x.id === sessionId);
    setSessionTitle(s?.title || s?.preview || "");
  }, [sessionId, sessions]);

  // 当前会话的通道，从会话列表派生。全新的 web 聊天还没有行——回退到
  // "web" 保持输入启用。IM 会话获得横幅 + 禁用输入，因为在此处写入会
  // 发送到智能体会话但永远不会到达上游信使。
  const currentChannel = useMemo<string>(() => {
    const s = sessions.find((x) => x.id === sessionId);
    return s?.channel || "web";
  }, [sessions, sessionId]);
// isReadOnlyChannel 为 IM 绑定的会话锁定输入框（回复必须来自上游通道）。
  // isActAsView 在超级管理员通过此 URL 查看另一用户的聊天时锁定。
  // 两者归约为相同的禁用状态应用于输入框/发送按钮；横幅不同，以便
  // 用户知道*原因*。
  const isReadOnlyChannel = currentChannel !== "web";
  const isReadOnlyView = isReadOnlyChannel || isActAsView;

  const handleRenameTitle = useCallback(
    async (next: string) => {
      const trimmed = next.trim();
      if (!trimmed || !selectedAgent || trimmed === sessionTitle) return;
      setSessionTitle(trimmed);
      try {
        await renameChatSession(selectedAgent, sessionId, trimmed);
      } finally {
        loadSessions(selectedAgent);
        // 通知全局侧边栏重新获取其聊天列表，使新标题无需整页刷新即可显示。
        // AppSidebar 自身的获取仅在 activeAgentId 变化时重新运行，
        // 重命名不会触发。
        if (typeof window !== "undefined") {
          window.dispatchEvent(
            new CustomEvent("bkclaw:sessions-changed", {
              detail: { agentId: selectedAgent },
            }),
          );
        }
      }
    },
    [selectedAgent, sessionId, sessionTitle, loadSessions],
  );

  // 将可编辑标题 + 工作区面板切换按钮渲染到全局固定头部（聊天容器注入
  // 任意 JSX 到此处，位于侧边栏切换按钮旁）。切换按钮在面板开启时
  // 仍保持连接，以便用户从展开时使用的同一控件折叠它。
  const headerSlot = useMemo(
    () => (
      <div className="flex flex-1 items-center justify-between gap-2 min-w-0">
        <ChatHeaderTitle
          title={sessionTitle}
          fallback={`Chat with ${agentName || selectedAgent}`}
          onSave={handleRenameTitle}
        />
        <button
          type="button"
          onClick={() => setFilesSheetOpen((v) => !v)}
          className={`shrink-0 inline-flex h-8 w-8 items-center justify-center rounded-md transition-colors ${
            filesSheetOpen
              ? "bg-muted text-foreground"
              : "text-muted-foreground hover:bg-muted/50 hover:text-foreground"
          }`}
          title={filesSheetOpen ? "隐藏工作区" : "显示工作区"}
          aria-pressed={filesSheetOpen}
        >
          <FolderOpen className="h-4 w-4" />
          <span className="sr-only">切换工作区</span>
        </button>
      </div>
    ),
    [sessionTitle, agentName, selectedAgent, handleRenameTitle, filesSheetOpen],
  );
  usePageHeader(headerSlot, [headerSlot]);

  // 会话变化时加载历史。从服务器历史重建消息后，我们还将此会话的工作区
  // 文件重新附加到最后的助手气泡，使"你的文件"面板在刷新后依然显示——
  // 服务器历史不携带每轮文件差异，因此我们通过一次列出 sessions/<sid>/
  // 下的所有内容来近似，并挂到最后一个助手消息上。
  useEffect(() => {
    if (!selectedAgent || !sessionId) return;
// 会话变化时重置去重状态——前一会话的事件不得偏置新会话的
      // seq 过滤器，临时占位符也不再相关。
    maxSeqRef.current = -1;
    subscribeSinceRef.current = -1;
    transientBubbleIdRef.current = null;
// 关闭此 sessionId 的 SSE 门；历史获取落地且 subscribeSinceRef
      // 设置为真实游标后重新开启。
    setLoadedSessionId(null);
// 会话变化时重置待办面板，使前一个聊天的清单不会在新数据获取到达之前
      // 短暂闪烁。
    setTodoItems([]);
// 子智能体进度指示器同理——从不跨会话携带；全新加载意味着没有
      // 正在进行的 delegate_task 需要跟踪。
    setSubagentProgress(null);
    setContextUsage((usage) => zeroContextUsage(usage));
// 与历史获取一同刷新 todo.md。我们不让其余加载等待它——
      // 404（尚无 todo.md）是空白会话的正常情况。
    getChatTodo(selectedAgent, sessionId)
      .then((todo) => setTodoItems(todo.items))
      .catch(() => setTodoItems([]));
    let aborted = false;
    getChatHistoryWithCursor(selectedAgent, sessionId)
      .then(async ({ history, latestEventSeq, contextUsage }) => {
        if (aborted) return;
        if (latestEventSeq > maxSeqRef.current) maxSeqRef.current = latestEventSeq;
        subscribeSinceRef.current = latestEventSeq;
        setContextUsage(contextUsage);
        if (!history || history.length === 0) {
          setMessages([]);
          setLoadedSessionId(sessionId);
          return;
        }
        const built = buildChatMessages(history);
        try {
          // listAgentFiles(agentId, sessionId) 让后端选择正确的前缀——项目聊天
          // 用 projects/<pid>/，松散聊天用 sessions/<chat>/——因此我们无需
          // 在此处硬编码 `sessions/<sid>/`。硬编码的前缀会遗漏项目聊天中
          // 的每个文件。
          const sessionFiles: ProducedFile[] = (
            await listAgentFiles(selectedAgent, sessionId)
          )
            .filter((f) => !isSystemFile(f.path))
            .map((f) => ({ path: f.path, size: f.size }));
          if (sessionFiles.length > 0) {
            for (let i = built.length - 1; i >= 0; i--) {
              if (built[i].role === "agent" || built[i].role === "tool-group") {
                built[i] = { ...built[i], files: sessionFiles };
                break;
              }
            }
          }
        } catch { /* 列出失败 — 回退为无面板 */ }
        if (aborted) return;
        setMessages(built);
        setLoadedSessionId(sessionId);
      })
      .catch(() => {
        if (aborted) return;
        setMessages([]);
        // 历史获取失败——仍然打开 SSE 以接收实时事件，但使用 seq=0
        // 而非 -1，以便不会触发全量服务端重放作为副作用。
        subscribeSinceRef.current = 0;
        setLoadedSessionId(sessionId);
      });
    return () => {
      aborted = true;
    };
  }, [selectedAgent, sessionId]);

  useEffect(() => {
    if (!stickToBottomRef.current) return;
    messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages]);

// 监听滚动容器以确定新内容到达时是否保持自动滚动。
// 64px 宽容度吸收流式抖动——没有它，单个 token 追加可能将底部
// 推出视口并在下一次自动滚动触发前暂时关闭"粘性"。
  useEffect(() => {
    const el = messagesScrollRef.current;
    if (!el) return;
    const onScroll = () => {
      const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
      stickToBottomRef.current = distance <= 64;
    };
    el.addEventListener("scroll", onScroll, { passive: true });
    return () => el.removeEventListener("scroll", onScroll);
  }, []);

  useEffect(() => {
    const el = textareaRef.current;
    if (el) {
      el.style.height = "auto";
      el.style.height = Math.min(el.scrollHeight, 200) + "px";
    }
  }, [input]);

// applySteerEvent 将服务端"steer"回显渲染为用户气泡，
// 与乐观的 pendingSteer 气泡（如果有）进行对账，避免消息重复。
// seq-去重已在上游由两个事件消费者应用。
  const applySteerEvent = useCallback((content: string) => {
    if (!content) return;
    setMessages((prev) => {
      const idx = prev.findIndex(
        (m) => m.role === "user" && m.pendingSteer && m.content === content,
      );
      if (idx >= 0) {
        const updated = [...prev];
        updated[idx] = { ...updated[idx], pendingSteer: undefined };
        return updated;
      }
      return [
        ...prev,
        { id: `s-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`, role: "user", content, timestamp: Date.now() },
      ];
    });
  }, []);

  const handleSend = useCallback(async (overrideText?: string, force?: boolean) => {
    // overrideText 允许调用方发送非来自输入框的消息（如计划确认按钮
    // 点击"go"）。存在时完全绕过输入框——输入框状态保持原样。
    // force 绕过进行中保护；由 steer 409 回退使用（服务端确认无活跃轮次）。
    const composerText = (overrideText ?? input).trim();
    const text = composerText;
    // sid 固定为本次发送的会话。用于按会话标记活跃/中止，并让后台轮次
    // （用户中途切走）的流回调据此停止写入前台视图。
    const sid = sessionId;
    // 允许仅发送附件（无文本），但至少需要一个。
    if ((!text && attachments.length === 0) || !selectedAgent || (activeSessionsRef.current.has(sid) && !force)) return;

    // `/project/<pid>` 是侧边栏放置我们的懒创建标记。在此捕获以便
    // 搭载到首次聊天请求体；会话行存在后，project_id 在行上，
    // URL 回退到裸聊天形式。
    const projectIdHint = urlProjectId;

    // 在首次发送时将 sessionId 固定到 URL 中，使刷新保持在同一会话。
    // 使用原生 History API 而非 `router.replace`，因为 output:'export'
    // 仅为 /chat/[session] 预渲染 `_` 占位符；router.replace 到真实 sid
    // 会触发 RSC 请求，SPA 后备无法满足，Next 会回退到硬
    // window.location 导航——这会杀死即将开始的流。Next 16 的 app-router
    // 修补了 history.replaceState 以分发 ACTION_RESTORE，因此
    // usePathname / useSearchParams（及从中派生的侧边栏 navigateOnce
    // 去重）仍能看到新 URL。
    const target = `/agents/${selectedAgent}/chat/${sessionId}/`;
    if (pathname !== target) {
      window.history.replaceState(null, "", target);
    }

    // 先上传附件，使智能体在首个轮次即能按名称读取。文件落至工作区存储中
    // 的 sessions/<sid>/<basename>，与 docker 沙箱绑定挂载的 /workspace
    // 是同一目录。我们还(a)构建用户气泡预览元数据，使图片无需等待服务器
    // 往返即可在气泡中渲染，(b)将图片读取为 data URL，使视觉能力模型
    // 接收它们为 image_url 内容部分。
    const filesToUpload = attachments;
    setAttachments([]);

    let userBubbleAttachments: UserAttachment[] = [];
    let imageDataUrls: string[] = [];

    if (filesToUpload.length > 0) {
      userBubbleAttachments = filesToUpload.map((f) => ({
        name: f.name,
        isImage: f.type.startsWith("image/"),
        previewUrl: f.type.startsWith("image/") ? URL.createObjectURL(f) : undefined,
      }));

      try {
        await uploadAgentFiles(selectedAgent, sessionId, filesToUpload);
      } catch (err) {
        setMessages((prev) => [
          ...prev,
          { id: `e-${Date.now()}`, role: "agent", content: `文件上传失败：${err instanceof Error ? err.message : "未知错误"}`, timestamp: Date.now() },
        ]);
        return;
      }

      // 将每张图片读取为 base64 data URL。我们在上传之后执行此操作——
      // 上传只需要 File 对象；data URL 转换是为了提供者调用。并行执行以保证
      // 多附件时的快速体验。
      imageDataUrls = (
        await Promise.all(
          filesToUpload.map(async (f) => {
            if (!f.type.startsWith("image/")) return null;
            return await new Promise<string | null>((resolve) => {
              const reader = new FileReader();
              reader.onload = () => resolve(typeof reader.result === "string" ? reader.result : null);
              reader.onerror = () => resolve(null);
              reader.readAsDataURL(f);
            });
          }),
        )
      ).filter((s): s is string => !!s);
    }
    // 构建实际发送给模型的提示词。图片以 `imageUrls` 形式发送用于视觉，
    // 但模型还需要磁盘路径用于 image-tool 等技能（`input: "/workspace/<file>"`）。
    // 我们在前面加上 `[Attached: /workspace/<name>]` 行——服务端的
    // StripAttachedPrefix 在历史读取时移除它们，使用户气泡、页面标题
    // 和侧边栏预览保持干净。
    const attachedPaths = filesToUpload.map((f) => `/workspace/${f.name}`);
    const breadcrumb = attachedPaths
      .map((p) => `[Attached: ${p}]`)
      .join("\n");
    const fullText = breadcrumb
      ? (text ? `${breadcrumb}\n${text}` : breadcrumb)
      : text;

    // 仅当发送来自输入框时清空输入框。覆盖发送（计划确认按钮等）
    // 保持用户正在输入的内容不变。
    if (overrideText === undefined) {
      setInput("");
    }
    // 发送总意味着"我想看看接下来发生什么"——即使用户上滚阅读
    // 早期对话内容也重新固定到底部。
    stickToBottomRef.current = true;
    setMessages((prev) => [
      ...prev,
      {
        id: `u-${Date.now()}`,
        role: "user",
        content: text, // 气泡仅显示文本；附件在上方单独渲染
        timestamp: Date.now(),
        attachments: userBubbleAttachments.length > 0 ? userBubbleAttachments : undefined,
      },
    ]);
    markActive(sid, true);
    const abortController = new AbortController();
    abortsRef.current.set(sid, abortController);
    // wentBackground：一旦用户在本轮流式期间切到别的会话，本次 POST 回调即
    // 停止写入共享视图状态（轮次仍在服务端继续并持久化；用户切回时靠历史
    // 重载 + SSE 重放恢复，与现有"刷新页面恢复轮次"的路径一致）。一旦置位
    // 即保持，不在切回时让 POST 回调重新接管渲染。
    let wentBackground = false;
    const isForeground = () => {
      if (currentSessionIdRef.current !== sid) wentBackground = true;
      return !wentBackground;
    };

    // 在轮次前快照工作区，以便在 `done` 时对比并附加新创建/修改的文件
    // （PDF、图片等）到最终回复。即发即弃；如果快照失败，我们只是不在此
    // 轮次中显示文件。以 `路径 → 大小|修改时间` 为键。
    const preTurnFilesPromise = listAgentFiles(selectedAgent)
      .then((items) => {
        const m = new Map<string, string>();
        for (const f of items) m.set(f.path, `${f.size}|${f.modTime}`);
        return m;
      })
      .catch(() => new Map<string, string>());

    let curGroupId = "";
    let curCalls: { id: string; name: string; arguments: string; result?: string; metadata?: ToolResultMetadata }[] = [];
    let curContent = "";
    // streamingMsgIdRef 追踪正在进行的助手气泡以累积 content_delta。
    // 存储在 ref（上面声明）以便并行的 /api/chat/subscribe SSE 处理器
    // 可以观察它，并在尾随 `content` 事件先于 POST 回调到达时跳过重复
    // 气泡创建。在 startNewGroup、`content` 封口和 tool_call/done 时
    // 重置为 null。
    const turnFiles: ProducedFile[] = [];
    const seenPaths = new Set<string>();

    const startNewGroup = () => {
      curGroupId = `tg-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
      curCalls = [];
      curContent = "";
      streamingMsgIdRef.current = null;
    };
    startNewGroup();

    try {
      const requestParams =
        overrideText === undefined && planMode ? { planMode: true } : undefined;
      if (requestParams) setPlanMode(false);
      await sendChatStream(selectedAgent, sessionId, fullText, (evt: ChatStreamEvent) => {
        // 后台轮次（用户已切到别的会话）不再写入前台视图，也不触碰本会话的
        // 去重游标 maxSeqRef / 流式气泡 ref——这些都属于当前显示的会话。
        // 服务端继续持久化事件；用户切回时由历史重载 + SSE 重放渲染。
        if (!isForeground()) return;
        // 去重 /api/chat/subscribe SSE，两者服务端订阅同一 chat-events hub。
        // 先到达的路径渲染；另一方跳过。seq < 0 表示此事件持久化失败——
        // 在此回退并接受可能的双重渲染，而非完全丢弃事件。
        if (typeof evt.seq === "number" && evt.seq >= 0) {
          if (evt.seq <= maxSeqRef.current) return;
          maxSeqRef.current = evt.seq;
        }
        switch (evt.type) {
          case "content_delta": {
            // 来自提供者的增量 token 块。追加到进行中的助手气泡——在
            // 一轮（及工具组拆分后）的首个增量时创建。最终的 `content`
            // 事件在轮次完成时仍会携带完整文本，因此刷新/重放路径保持
            // 完整，即使增量不被持久化。
            const delta = evt.data?.delta || "";
            if (!delta) break;
            if (curCalls.length > 0 && !streamingMsgIdRef.current) {
              // 工具调用后的内容 = 新一轮；重置状态，使新气泡成为独立消息，
              // 而非追加到前一个工具组的思考文本上。
              startNewGroup();
            }
            curContent += delta;
            if (!streamingMsgIdRef.current) {
              const id = `a-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
              streamingMsgIdRef.current = id;
              setMessages((prev) => [
                ...prev,
                { id, role: "agent", content: delta, timestamp: Date.now() },
              ]);
            } else {
              const id = streamingMsgIdRef.current;
              setMessages((prev) => {
                const idx = prev.findIndex((m) => m.id === id);
                if (idx < 0) return prev;
                const updated = [...prev];
                updated[idx] = { ...updated[idx], content: (updated[idx].content || "") + delta };
                return updated;
              });
            }
            break;
          }
          case "content": {
            const content = evt.data?.content || "";
            const meta = evt.data?.metadata;
            if (content === "__NEW_SESSION__") {
              handleNewChat();
              loadSessions(selectedAgent);
              return;
            }
            // 如果气泡已通过 content_delta 流式传输，最终的 `content`
            // 携带相同文本——只需封口进行中的 ID，可选附加元数据，
            // 并跳过创建重复气泡。
            if (streamingMsgIdRef.current) {
              const id = streamingMsgIdRef.current;
              streamingMsgIdRef.current = null;
              if (meta) {
                setMessages((prev) => {
                  const idx = prev.findIndex((m) => m.id === id);
                  if (idx < 0) return prev;
                  const updated = [...prev];
                  updated[idx] = { ...updated[idx], metadata: { ...updated[idx].metadata, ...meta } };
                  return updated;
                });
              }
              curContent = content;
              break;
            }
            // 仅含元数据且内容为空的事件：后端使用此来追溯标记前一个气泡
            // （例如流式强制最终交付路径中，块通过单独通道流动后元数据
            // 跟随而来）。应用到最近的助手消息而非创建空的新气泡。
            if (!content && meta) {
              setMessages((prev) => {
                for (let i = prev.length - 1; i >= 0; i--) {
                  if (prev[i].role === "agent") {
                    const updated = [...prev];
                    updated[i] = { ...updated[i], metadata: { ...updated[i].metadata, ...meta } };
                    return updated;
                  }
                }
                return prev;
              });
              break;
            }
            if (curCalls.length > 0) {
              // 工具调用后的内容 = 新一轮。完成当前组，重新开始。
              startNewGroup();
            }
            // 存储为思考内容（可能成为下一个工具组的一部分，或保留为最终答案）
            curContent = content;
            setMessages((prev) => [
              ...prev,
              { id: `a-${Date.now()}`, role: "agent", content, timestamp: Date.now(), metadata: meta },
            ]);
            break;
          }
          case "tool_call": {
            // 正在流式传输的气泡（如果有）即将被下面的"替换最后一条助手消息"
            // 逻辑转换为工具组。清除流式传输 ID，使下一轮的后续
            // content_delta 生成新气泡，而非写入已失效的 ID。
            streamingMsgIdRef.current = null;
            // 如果当前组中的每个工具都已返回结果，则开始新一轮。没有此判断，
            // 两个连续且无中间内容事件的助手轮次会在实时视图中合并为一个
            // 视觉组——与刷新路径（buildChatMessages）按助手消息正确拆分
            // 不一致。
            if (curCalls.length > 0 && curCalls.every((c) => c.result !== undefined)) {
              startNewGroup();
            }
            curCalls.push({
              id: evt.data?.id || "",
              name: evt.data?.name || "",
              arguments: evt.data?.arguments || "{}",
            });
            const groupId = curGroupId;
            const calls = [...curCalls];
            setMessages((prev) => {
              // 更新本轮的现有工具组（同一助手轮次内的额外 tool_call）。
              const idx = prev.findIndex((m) => m.id === groupId);
              if (idx >= 0) {
                const updated = [...prev];
                updated[idx] = { ...updated[idx], toolCalls: calls };
                return updated;
              }
              // 保留已流式传输的助手气泡——不将其文本折叠进工具组。
              // 与历史重载时 buildChatMessages 应用的拆分逻辑一致，
              // 使实时和重载视图保持一致。
              return [
                ...prev,
                { id: groupId, role: "tool-group" as const, content: "", timestamp: Date.now(), toolCalls: calls },
              ];
            });
            break;
          }
          case "tool_result": {
            const tc = curCalls.find((c) => c.id === (evt.data?.id || ""));
            const resultText = evt.data?.result || "";
            if (tc) {
              tc.result = resultText;
              if (evt.data?.metadata) tc.metadata = evt.data.metadata;
            }
            // 跟踪成功写入工作区的 write_file 调用（即相对路径且非系统身份文件）。
            if (tc && tc.name === "write_file" && /^Written \d+ bytes/.test(resultText)) {
              try {
                const args = JSON.parse(tc.arguments);
                const p: string = typeof args?.path === "string" ? args.path : "";
                if (p && !p.startsWith("/") && !isSystemFile(p) && !seenPaths.has(p)) {
                  seenPaths.add(p);
                  turnFiles.push({ path: p, size: parseWrittenSize(resultText) });
                }
              } catch { /* 忽略错误参数 */ }
            }
            // 当文件变更工具刚刚操作了 todo.md 时刷新待办面板。
            // 我们检查参数而非在每个 tool_result 上轮询，
            // 使网络开销与实际更新成正比（50 次 web_search 的长任务
            // 不会触发 50 次重新获取）。
            if (tc && (tc.name === "write_file" || tc.name === "edit_file" || tc.name === "apply_patch")) {
              try {
                const args = JSON.parse(tc.arguments);
                const path: string =
                  (typeof args?.path === "string" ? args.path : "") ||
                  (typeof args?.file_path === "string" ? args.file_path : "");
                if (path && /(^|\/)todo\.md$/i.test(path)) {
                  getChatTodo(selectedAgent, sessionId)
                    .then((todo) => setTodoItems(todo.items))
                    .catch(() => {});
                }
              } catch { /* 忽略错误参数 */ }
            }
            const groupId = curGroupId;
            const calls = [...curCalls];
            setMessages((prev) => {
              const idx = prev.findIndex((m) => m.id === groupId);
              if (idx < 0) return prev;
              const updated = [...prev];
              updated[idx] = { ...updated[idx], toolCalls: calls };
              return updated;
            });
            break;
          }
          case "subagent_progress": {
            // 子智能体发出心跳。存储为单个"当前运行状态"，因为
            // delegate_task 以串行注册——任何时刻只有一个子智能体在运行。
            // phase="done" 清除指示器。
            if (evt.data?.phase === "done") {
              setSubagentProgress(null);
            } else {
              setSubagentProgress({
                iteration: evt.data?.iteration,
                max: evt.data?.max,
                phase: evt.data?.phase,
                tools: evt.data?.tools,
              });
            }
            break;
          }
          case "steer": {
            // 用户在轮次中途注入的消息被服务端折叠到正在进行的轮次中。
            // 将其渲染为用户气泡（与乐观的 pendingSteer 气泡对账）。
            applySteerEvent(evt.data?.content || "");
            break;
          }
          case "error": {
            // 将后端错误显示为聊天气泡。没有这个，轮次只会挂起——模型失败
            // （提供者 4xx/5xx、序列化不匹配等）且唯一信号是用户看不到的
            // 网关日志行。
            const msg = evt.data?.message || "未知错误";
            setMessages((prev) => [
              ...prev,
              { id: `e-${Date.now()}`, role: "agent", content: `Error: ${msg}`, timestamp: Date.now() },
            ]);
            setCompacting(false);
            break;
          }
          case "compaction": {
            // 后端在轮次开始、产出回复前同步压缩上下文。active=true 时显示横杠，
            // false 时收起。这是个阻塞等待，单独提示让用户知道延迟来自压缩而非模型。
            setCompacting(!!evt.data?.active);
            break;
          }
          case "usage": {
            if (evt.data?.usage) setContextUsage(evt.data.usage);
            break;
          }
          case "done": {
            // 轮次结束。后端仍附带本轮的上下文占用（usage）作为兜底更新。
            // 并行的 /api/chat/subscribe 路径也会
            // 收到同一个带 seq 的 done 事件，但上方的 seq 去重确保只处理一次。
            if (evt.data?.usage) setContextUsage(evt.data.usage);
            setCompacting(false);
            break;
          }
        }
      }, abortController.signal, imageDataUrls, projectIdHint, requestParams);
      // 将工作区与轮次前快照对比，以便 *exec* 产出的文件（如保存 PDF 的
      // Python 脚本）也能显示——`turnFiles` 仅捕获具有相对、非身份路径的
      // write_file 工具调用，大多数真实流程都被遗漏。按路径合并两个来源。
      const postTurnFiles = await listAgentFiles(selectedAgent).catch(() => []);
      const preSnap = await preTurnFilesPromise;
      const diffFiles: ProducedFile[] = [];
      for (const f of postTurnFiles) {
        if (isSystemFile(f.path)) continue;
        const key = `${f.size}|${f.modTime}`;
        if (preSnap.get(f.path) === key) continue; // unchanged
        if (seenPaths.has(f.path)) continue;
        diffFiles.push({ path: f.path, size: f.size });
      }
      const allFiles = [...turnFiles, ...diffFiles];
      // 诊断：当沙箱执行产出文件但文件面板不显示时，我们需要知道 API
      // 是否返回了该文件以及差异在何处丢弃。保留成本低。
      if (typeof console !== "undefined") {
        console.log("[chat] post-turn files diff", {
          agent: selectedAgent,
          sessionId,
          preSnapSize: preSnap.size,
          postTurnCount: postTurnFiles.length,
          postTurnPaths: postTurnFiles.map((f) => f.path),
          turnFiles,
          diffFiles,
          attached: allFiles.length,
        });
      }
      if (allFiles.length > 0 && isForeground()) {
        setMessages((prev) => {
          if (prev.length === 0) return prev;
          const updated = [...prev];
          const last = updated[updated.length - 1];
          updated[updated.length - 1] = { ...last, files: allFiles };
          if (typeof console !== "undefined") {
            console.log("[chat] attached files to last message", {
              lastId: last.id,
              lastRole: last.role,
              files: allFiles,
            });
          }
          return updated;
        });
      }
      loadSessions(selectedAgent);
      // 新会话的首轮刚刚被持久化——通知全局侧边栏重新获取聊天列表，
      // 使新标题无需整页刷新即可出现。
      if (typeof window !== "undefined") {
        window.dispatchEvent(
          new CustomEvent("bkclaw:sessions-changed", {
            detail: { agentId: selectedAgent },
          }),
        );
      }
    } catch (err) {
      // 用户点击停止按钮产生的 AbortError 是预期行为——显示简要的"已停止"
      // 行，使用户看到取消已生效，而非通用失败消息。
      const isAbort = err instanceof DOMException && err.name === "AbortError";
      // 在 DevTools 中输出底层错误，以便未来的"获取响应失败"报告附带
      // 具体原因（网络、解析、轮次后获取等），而非通用消息。
      if (typeof console !== "undefined") {
        console.error("[chat] handleSend error", err);
      }
      // 键盘中止栈 + 流后拆卸都可能在成功轮次后抛出 AbortError（SSE 读取器
      // 在 `done` 后释放，然后滞留的 reader.cancel() 与延迟的服务端 EOF
      // 竞争并浮出水面）。两者在此看起来与用户按下停止相同，因此我们
      // 在当前轮次已有至少一条助手回复时额外抑制提示——用户已获得答案，
      // 不应再附加令人困惑的失败气泡。
      if (!isForeground()) {
        // 后台轮次出错/中止：不向用户当前正在看的另一个会话注入停止/错误气泡。
      } else if (isAbort) {
// 解决当前工具组中所有进行中的工具，使它们停止旋转。
          // 服务端的 padOrphanToolResults 会写入匹配记录；此处只是保持
          // UI 一致直到下一次历史获取覆盖它。
        setMessages((prev) =>
          prev.map((m) =>
            m.role === "tool-group" && m.toolCalls
              ? {
                  ...m,
                  toolCalls: m.toolCalls.map((tc) =>
                    tc.result === undefined ? { ...tc, result: "(stopped)" } : tc,
                  ),
                }
              : m,
          ),
        );
        setMessages((prev) => [
          ...prev,
          { id: `e-${Date.now()}`, role: "agent", content: "(Stopped)", timestamp: Date.now() },
        ]);
      } else {
        setMessages((prev) => {
          const lastUser = [...prev].reverse().findIndex((m) => m.role === "user");
          if (lastUser >= 0) {
            const replyAfter = prev
              .slice(prev.length - lastUser)
              .some((m) => m.role === "agent" || m.role === "tool-group");
            if (replyAfter) return prev; // 轮次已产出输出
          }
          const errMsg = err instanceof Error && err.message
            ? err.message
            : "获取响应失败，请确认网关是否正在运行。";
          return [
            ...prev,
            {
              id: `e-${Date.now()}`,
              role: "agent",
              content: errMsg,
              timestamp: Date.now(),
            },
          ];
        });
      }
    } finally {
      abortsRef.current.delete(sid);
      markActive(sid, false);
      // 前台 UI 清理只在本轮属于当前显示会话时执行；后台轮次结束不应抹掉
      // 用户正在看的另一个会话的子智能体进度/压缩横杠/输入焦点。
      // 双重保险：子智能体的 done 事件在正常路径上清除这些状态，但若网络
      // 抖动丢失该事件，不希望过时的"第 5/20 次迭代"或压缩横杠卡住。
      if (isForeground()) {
        setSubagentProgress(null);
        setCompacting(false);
        textareaRef.current?.focus();
      }
    }
  }, [input, attachments, selectedAgent, sessionId, loadSessions, pathname, router, urlProjectId, planMode, markActive]);

  const handleStop = useCallback(() => {
    // 停止当前显示会话的进行中轮次（后台会话的轮次保持运行）。
    abortsRef.current.get(currentSessionIdRef.current)?.abort();
  }, []);

  // handleSteer 在轮次流式传输期间触发：将消息缓冲到正在进行的轮次中
  // （智能体在工具轮次之间折叠它并在现有 SSE 上流出"steer"回显）。在 409
  // （无活跃轮次——轮次刚结束）时回退到普通发送，不会丢失任何内容。
  const handleSteer = useCallback(async () => {
    const text = input.trim();
    // 仅在 handleKeyDown 的 `if (sending)` 分支中调用；在一次渲染中
    // React 状态是快照一致的，因此 `sending` 在此处必然为 true。
    if (!text || !selectedAgent || !sending) return;
    setInput("");
    const optimisticId = `s-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
    setMessages((prev) => [
      ...prev,
      { id: optimisticId, role: "user", content: text, timestamp: Date.now(), pendingSteer: true },
    ]);
    let ok = false;
    try {
      ok = await steerChat(selectedAgent, sessionId, text, urlProjectId);
    } catch (err) {
      setMessages((prev) => [
        ...prev.filter((m) => m.id !== optimisticId),
        { id: `e-${Date.now()}`, role: "agent", content: `调整指令失败：${err instanceof Error ? err.message : "未知错误"}`, timestamp: Date.now() },
      ]);
      return;
    }
    if (!ok) {
      setMessages((prev) => prev.filter((m) => m.id !== optimisticId));
      await handleSend(text, true);
    }
  }, [input, selectedAgent, sending, sessionId, urlProjectId, handleSend]);

  const handleFilePick = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const picked = e.target.files;
    if (!picked || picked.length === 0) return;
    // 在重置输入框之前将 FileList 快照为稳定的 File[]。FileList
    // 与输入元素绑定——设置 value="" 会清空它。在 StrictMode 下 React
    // 为纯度检查会调用两次 setState 更新器；如果闭包引用了活
    // FileList，第二次调用看到空列表，状态最终为空，即使用户选择
    // 了文件。
    const newFiles = Array.from(picked);
    e.target.value = "";
    setAttachments((prev) => [...prev, ...newFiles]);
  }, []);

  const removeAttachment = useCallback((idx: number) => {
    setAttachments((prev) => prev.filter((_, i) => i !== idx));
  }, []);

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    // IME 组合激活时不要提交——此时 Enter 是确认 IME 候选（如拼音 → 好），
    // 而非发送消息。keyCode 229 在某些不设置 isComposing 的浏览器中也
    // 表示"正在组合"。
    if (e.nativeEvent.isComposing || e.keyCode === 229) return;

    // 斜杠菜单打开时，键盘处理优先：方向键移动选择，Enter 确认，
    // Escape 关闭而不发送。
    if (slashOpen && filteredItems.length > 0) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setSlashIndex((i) => (i + 1) % filteredItems.length);
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setSlashIndex((i) => (i - 1 + filteredItems.length) % filteredItems.length);
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        selectItem(filteredItems[slashIndex]);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setSlashOpen(false);
        return;
      }
    }

    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      // 轮次流式传输时，Enter 转向正在进行的轮次而非被阻止；
      // 否则为正常发送。
      if (sending) {
        handleSteer();
      } else {
        handleSend();
      }
    }
  };

  // onChange 包装：一次通过中更新输入 + 斜杠菜单可见性。
  const handleInputChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const next = e.target.value;
    setInput(next);
    const caret = e.target.selectionStart ?? next.length;
    const ctx = slashContext(next, caret);
    if (ctx) {
      setSlashOpen(true);
      setSlashQuery(ctx.query);
      setSlashIndex(0);
    } else {
      setSlashOpen(false);
    }
  };

  const handleCopy = (msg: ChatMessage) => {
    navigator.clipboard.writeText(msg.content);
    setCopiedId(msg.id);
    setTimeout(() => setCopiedId(null), 1500);
  };

// handleRetry 将此消息内容填回输入框，以便用户在重新发送前确认/编辑。
// 故意不自动发送——一键重发会静默丢弃现有助手回复，太容易误触。
  const handleRetry = (msg: ChatMessage) => {
    setInput(msg.content);
    setTimeout(() => {
      const el = textareaRef.current;
      if (el) {
        el.focus();
        const end = el.value.length;
        el.setSelectionRange(end, end);
      }
    }, 0);
  };

  const handleNewChat = () => {
    const newId = generateSessionId();
    setSessionId(newId);
    setMessages([]);
    setContextUsage((usage) => zeroContextUsage(usage));
    router.replace(`/agents/${selectedAgent}/chat/`);
  };

  const handleSelectSession = (sid: string) => {
    setSessionId(sid);
    setContextUsage((usage) => zeroContextUsage(usage));
    // history.replaceState（而非 router.replace）——原因与 handleSend 相同：
    // /chat/[session] 在 output:'export' 下仅为 `_` 占位符预渲染，
    // 因此 router 驱动的导航到真实 sid 会硬重载。详见 handleSend 中的
    // 完整说明。
    window.history.replaceState(null, "", `/agents/${selectedAgent}/chat/${sid}/`);
  };

  const formatTime = (ts: number) =>
    new Date(ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

// 空白新聊天状态：将消息滚动区从 flex-1 泳道折叠出来，垂直居中标题 + 输入框，
// Manus 风格。有消息后布局切换回标准的"上方滚动，底部固定输入框"形态。
  const isEmpty = messages.length === 0;
// 计算最新助手气泡是否为待定计划（编号计划 + "回复 `go` 执行" 页脚），
// 仅当没有后续用户消息时。这是获得内联确认/取消按钮的唯一个气泡；
// 历史中更早的计划不会重新渲染按钮。
  const pendingPlanId: string | null = (() => {
    if (sending) return null;
    for (let i = messages.length - 1; i >= 0; i--) {
      const m = messages[i];
      if (m.role === "user") return null; // 用户已回复 → 计划不再待定
      if (m.role === "agent" && isPendingPlanContent(m.content)) return m.id;
    }
    return null;
  })();
// 主标题在空白智能体主页和项目落地页上相同——Manus 风格的"我能做什么"提示。
// 项目页面在主标题下方渲染小型信息卡（文件夹 + 名称 + 元数据）而非取代标题，
// 使用户始终知道他们正在与哪个智能体聊天。
  const heroTitle = "我能为你做些什么？";

  return (
    <div className="flex h-[calc(100vh-3rem)] flex-row">
      <div
        className={
          "flex flex-1 min-w-0 flex-col" +
          // pb-12 (3rem) 匹配我们已从父元素 h-[calc(100vh-3rem)] 中减去的头部高度。
          // 没有它，`justify-center` 在头部后区域内居中内容，视觉上偏下约 24px——
          // 用户会注意到主标题 + 输入框组合向下偏移。添加相等的底部内边距使
          // 居中组向上偏移半个头部高度，使视觉中心与几何中心对齐。
          (isEmpty ? " justify-center pb-12" : "")
        }
      >
      {/* 消息 */}
        <div
          ref={messagesScrollRef}
          className={
            "min-h-0 px-4 " +
            (isEmpty ? "shrink-0" : "flex-1 overflow-y-auto py-4")
          }
        >
          <div className="mx-auto max-w-2xl space-y-3">
            {isEmpty && (
              <div className="py-8 text-center">
                <h1 className="text-3xl md:text-4xl font-semibold tracking-tight">
                  {heroTitle}
                </h1>
              </div>
            )}

            {(() => {
              // 工具组产物（如沙箱内 Python 脚本渲染为 base64 的图片）附加到
              // *下一个*助手回复气泡——使它们作为助手回答的一部分显示，
              // 而非在工具面板内。如果没有后续助手回复（工具仍在运行或
              // 链未完成），我们跳过显示图片；它将在下一次回复中显示。
              // `surfacedSrcs` 追踪每个已显示的图片 src，气泡可据此抑制
              // 重复的内联副本。
              const attachedImages = new Map<string, Array<{ alt: string; src: string }>>();
              const surfacedSrcs = new Set<string>();
              let pending: Array<{ alt: string; src: string }> = [];
              for (const m of messages) {
                if (m.role === "tool-group" && m.toolCalls) {
                  for (const tc of m.toolCalls) {
                    if (!tc.result) continue;
                    for (const p of splitDataImages(tc.result)) {
                      if (p.type === "image") {
                        pending.push({ alt: p.alt, src: p.src });
                      }
                    }
                  }
                  continue;
                }
                if (m.role === "agent" && pending.length > 0) {
                  attachedImages.set(m.id, pending);
                  for (const img of pending) surfacedSrcs.add(img.src);
                  pending = [];
                }
              }
              // renderSegment 把"一段消息"渲染成元素数组：合并连续工具回合（避免
              // 长 ReAct 轮次里七个顺序回合占满整屏）、展开 <|split|> 多气泡、其余走
              // 常规气泡。抽成函数后即可对同一轮内的"中间过程段"与"最终输出段"
              // 分别调用——前者塞进可折叠的 TurnProcess。
              function renderSegment(segMsgs: ChatMessage[]): React.ReactNode[] {
              const elements: React.ReactNode[] = [];
              for (let i = 0; i < segMsgs.length; i++) {
                const msg = segMsgs[i];
                if (msg.role === "tool-group") {
                  const start = i;
                  while (
                    i + 1 < segMsgs.length &&
                    segMsgs[i + 1].role === "tool-group"
                  ) {
                    i++;
                  }
                  const rounds = segMsgs.slice(start, i + 1);
// 将每个回合产出文件的展示保留在此处，使每个回合面板仍按时间
                    // 顺序在打包下方渲染。
                  const filePanels = rounds
                    .filter((r) => r.files && r.files.length > 0)
                    .map((r) => (
                      <FilesPanel
                        key={`files-${r.id}`}
                        agentId={selectedAgent}
                        files={r.files!}
                      />
                    ));
                  if (rounds.length === 1) {
                    elements.push(
                      <div key={rounds[0].id}>
                        <ToolCallGroup
                          msg={rounds[0]}
                          surfacedSrcs={surfacedSrcs}
                          agentId={selectedAgent}
                          sessionId={sessionId}
                          subagentProgress={subagentProgress}
                        />
                        {filePanels}
                      </div>,
                    );
                  } else {
                    elements.push(
                      <div key={`bundle-${rounds[0].id}`}>
                        <ToolRoundsBundle
                          rounds={rounds}
                          surfacedSrcs={surfacedSrcs}
                          agentId={selectedAgent}
                          sessionId={sessionId}
                          subagentProgress={subagentProgress}
                        />
                        {filePanels}
                      </div>,
                    );
                  }
                  continue;
                }
                // 助手气泡可能携带 LLM 为多气泡输出发出的 `<|split|>` 标记
                //（镜像 IM 通道行为）。在每个分块处展开为单独气泡，
                // 使标记不会显示为字面文本。文件/元数据仅附加到最后一个
                // 分块，以匹配 IM 分发器的"附加到最后一个分块"规则。
                if (msg.role === "agent" && msg.content.includes(SPLIT_MARKER)) {
                  const parts = splitOnMarker(msg.content);
                  parts.forEach((part, idx) => {
                    const isLast = idx === parts.length - 1;
                    elements.push(
                      renderRegularBubble({
                        ...msg,
                        id: `${msg.id}-s${idx}`,
                        content: part,
                        files: isLast ? msg.files : undefined,
                        metadata: isLast ? msg.metadata : undefined,
                      }),
                    );
                  });
                  continue;
                }
                elements.push(renderRegularBubble(msg));
              }
              return elements;
              }

              // 按"轮次"分段：每个 user 消息开启一轮，其后连续的非 user 消息属于该轮。
              // 正在流式输出的最后一轮全程铺开（沿用现状，便于实时观察工具决策与执行）；
              // 已结束的轮次把"最后一次工具回合及其之前的全部中间过程（工具回合 +
              // 工具之间的叙述气泡）"折叠进单个 TurnProcess 容器，只把其后的最终输出
              // 留在容器外展开。不含工具的轮次（纯文字直答）不折叠。
              const out: React.ReactNode[] = [];
              for (let i = 0; i < messages.length; i++) {
                const msg = messages[i];
                if (msg.role === "user") {
                  out.push(renderRegularBubble(msg));
                  continue;
                }
                // 收集本轮：从当前非 user 消息起，吃掉后续所有非 user 消息。
                const turnStart = i;
                while (i + 1 < messages.length && messages[i + 1].role !== "user") i++;
                const turnMsgs = messages.slice(turnStart, i + 1);
                const isLiveTurn = i === messages.length - 1 && sending;
                const lastToolIdx = turnMsgs.map((m) => m.role).lastIndexOf("tool-group");
                if (isLiveTurn || lastToolIdx === -1) {
                  // 实时轮次或纯文字轮次：原样铺开。
                  out.push(...renderSegment(turnMsgs));
                  continue;
                }
                const processMsgs = turnMsgs.slice(0, lastToolIdx + 1);
                const finalMsgs = turnMsgs.slice(lastToolIdx + 1);
                const toolCount = processMsgs.reduce(
                  (n, m) => n + (m.toolCalls?.length ?? 0),
                  0,
                );
                const roundCount = processMsgs.filter(
                  (m) => m.role === "tool-group",
                ).length;
                out.push(
                  <TurnProcess
                    key={`proc-${turnMsgs[0].id}`}
                    toolCount={toolCount}
                    roundCount={roundCount}
                  >
                    {renderSegment(processMsgs)}
                  </TurnProcess>,
                );
                out.push(...renderSegment(finalMsgs));
              }
              return out;

              function renderRegularBubble(msg: ChatMessage) {
                return (
                <div
                  key={msg.id}
                  className={`flex ${msg.role === "user" ? "justify-end" : "justify-start"}`}
                >
                  <div
                    className={`group relative max-w-[80%] ${
                      msg.role === "user" ? "order-1" : ""
                    }`}
                  >
                    {msg.role === "user" && msg.sender && (
                      <div className="mb-1 flex items-center justify-end gap-2 text-xs text-muted-foreground">
                        <span className="font-medium text-foreground/80">{msg.sender.name}</span>
                        {msg.sender.avatarUrl ? (
                          // eslint-disable-next-line @next/next/no-img-element
                          <img
                            src={msg.sender.avatarUrl}
                            alt={msg.sender.name}
                            className="h-5 w-5 rounded-full object-cover ring-1 ring-border"
                          />
                        ) : (
                          <span className="flex h-5 w-5 items-center justify-center rounded-full bg-primary/20 text-[10px] font-semibold uppercase text-foreground">
                            {msg.sender.name.slice(0, 1)}
                          </span>
                        )}
                      </div>
                    )}
                    <div
                      className={`rounded-2xl px-4 py-2.5 break-words ${
                        msg.role === "user"
                          ? "bg-primary/10 dark:bg-primary/15 text-foreground rounded-br-md"
                          : "bg-muted rounded-bl-md"
                      }`}
                    >
                      {(() => {
                        const attached = attachedImages.get(msg.id);
                        return attached && attached.length > 0 ? (
                          <div className="space-y-2 mb-2">
                            {attached.map((img, i) => (
                              // eslint-disable-next-line @next/next/no-img-element
                              <img
                                key={i}
                                src={img.src}
                                alt={img.alt}
                                className="rounded-lg max-w-full h-auto"
                              />
                            ))}
                          </div>
                        ) : null;
                      })()}
                      {msg.role === "user" && msg.attachments && msg.attachments.length > 0 && (
                        <div className="flex flex-wrap gap-2 mb-2 justify-end">
                          {msg.attachments.map((att, i) =>
                            att.isImage && att.previewUrl ? (
                              <button
                                key={i}
                                type="button"
                                onClick={() => setLightboxSrc(att.previewUrl!)}
                                className="block cursor-zoom-in"
                                aria-label={`预览 ${att.name}`}
                              >
                                {/* eslint-disable-next-line @next/next/no-img-element */}
                                <img
                                  src={att.previewUrl}
                                  alt={att.name}
                                  className="rounded-lg max-h-48 max-w-[12rem] w-auto h-auto object-cover"
                                />
                              </button>
                            ) : (
                              <div
                                key={i}
                                className="flex items-center gap-2 rounded-md bg-sidebar-foreground/10 px-2 py-1.5 text-xs"
                              >
                                <Paperclip className="h-3 w-3 opacity-70" />
                                <span className="truncate">{att.name}</span>
                              </div>
                            ),
                          )}
                        </div>
                      )}
                      {msg.content && (
                        <div className={CHAT_PROSE_CLASS}>
                          {renderContentWithDataImages(
                            msg.content,
                            surfacedSrcs,
                            (attachedImages.get(msg.id)?.length ?? 0) > 0,
                            makeUrlTransform(selectedAgent, sessionId),
                          ) ?? (
                            <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]} urlTransform={makeUrlTransform(selectedAgent, sessionId)} components={{ a: ExternalAnchor }}>
                              {msg.content}
                            </ReactMarkdown>
                          )}
                        </div>
                      )}
                      {msg.role === "agent" && msg.metadata?.iterationCapReached && (
                        <div className="mt-2 flex items-start gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 px-2.5 py-1.5 text-xs text-amber-900 dark:text-amber-200">
                          <span className="font-medium">已达到迭代次数上限</span>
                          <span className="opacity-80">
                            智能体已达到 {msg.metadata.iterationCapValue ?? ""} 工具调用预算上限，尚未完成任务。 上方回答由部分结果汇总生成 — 部分字段可能标记为未知或不完整。 可继续对话以推进任务。
                          </span>
                        </div>
                      )}
                      {msg.role === "agent" && msg.metadata?.planMode && (
                        <div className="mt-2 flex items-start gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 px-2.5 py-1.5 text-xs text-amber-900 dark:text-amber-200">
                          <ListChecks className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                          <span className="font-medium">仅生成计划，请在执行前确认。</span>
                          <span className="opacity-80">
                            本轮已禁用工具。回复“执行”（或修改意见）即可运行。
                          </span>
                        </div>
                      )}
                      {msg.role === "agent" && msg.id === pendingPlanId && (
                        <div className="mt-3 flex flex-wrap items-center gap-2">
                          <Button
                            size="sm"
                            onClick={() => handleSend("go")}
                            disabled={sending}
                            className="h-8 gap-1.5"
                          >
                            <Check className="h-3.5 w-3.5" />
                            执行计划
                          </Button>
                          <Button
                            size="sm"
                            variant="outline"
                            onClick={() => {
// 不发送载荷——用户拒绝计划。聚焦输入框并提示他们输入修改内容；
                                // 按钮在下一个助手回复再次匹配待定计划信号时重新渲染。
                              setInput("");
                              textareaRef.current?.focus();
                            }}
                            disabled={sending}
                            className="h-8 gap-1.5"
                          >
                            <X className="h-3.5 w-3.5" />
                            编辑
                          </Button>
                          <span className="text-xs text-muted-foreground">
                            执行计划以授权智能体完成整个流程，或点击“编辑”修改下方内容。
                          </span>
                        </div>
                      )}
                    </div>
                    {msg.files && msg.files.length > 0 && (
                      <FilesPanel agentId={selectedAgent} files={msg.files} />
                    )}
                    <div
                      className={`flex items-center gap-1.5 mt-1 ${
                        msg.role === "user" ? "justify-end" : "justify-start"
                      }`}
                    >
                      {msg.role === "user" ? (
                        <>
                          {msg.timestamp > 0 && (
                            <span className="opacity-0 group-hover:opacity-100 text-[10px] text-muted-foreground/60 transition-all">
                              {formatTime(msg.timestamp)}
                            </span>
                          )}
                          <button
                            onClick={() => handleCopy(msg)}
                            className="opacity-0 group-hover:opacity-100 p-0.5 rounded hover:bg-muted text-muted-foreground/60 hover:text-muted-foreground transition-all"
                            title="复制"
                          >
                            {copiedId === msg.id ? (
                              <Check className="h-3 w-3 text-emerald-500" />
                            ) : (
                              <Copy className="h-3 w-3" />
                            )}
                          </button>
                          <button
                            onClick={() => handleRetry(msg)}
                            className="opacity-0 group-hover:opacity-100 p-0.5 rounded hover:bg-muted text-muted-foreground/60 hover:text-muted-foreground transition-all"
                            title="重新发送（回填到输入框）"
                          >
                            <RotateCcw className="h-3 w-3" />
                          </button>
                        </>
                      ) : (
                        <>
                          {msg.timestamp > 0 && (
                            <span className="text-[10px] text-muted-foreground/60">
                              {formatTime(msg.timestamp)}
                            </span>
                          )}
                          <button
                            onClick={() => handleCopy(msg)}
                            className="opacity-0 group-hover:opacity-100 p-0.5 rounded hover:bg-muted text-muted-foreground/60 hover:text-muted-foreground transition-all"
                            title="复制"
                          >
                            {copiedId === msg.id ? (
                              <Check className="h-3 w-3 text-emerald-500" />
                            ) : (
                              <Copy className="h-3 w-3" />
                            )}
                          </button>
                          <button
                            onClick={() => setFilesSheetOpen(true)}
                            className="opacity-0 group-hover:opacity-100 inline-flex items-center gap-1 px-1.5 py-0.5 rounded hover:bg-muted text-[10px] text-muted-foreground/60 hover:text-muted-foreground transition-all"
                            title="查看任务文件"
                          >
                            <FolderOpen className="h-3 w-3" />
                            <span>文件</span>
                          </button>
                        </>
                      )}
                    </div>
                  </div>
                </div>
              );
              }
            })()}

            {compacting && (
              // 上下文压缩横杠：压缩在轮次开始、产出任何回复前同步进行，
              // 用一道横贯的横杠 + 旋转图标提示「正在压缩上下文…」，让用户
              // 明白此处的停顿来自压缩而非模型思考。压缩结束（或轮次终止）即收起。
              <div className="my-1 flex items-center gap-3 text-xs text-amber-600 dark:text-amber-400" aria-live="polite">
                <span className="h-px flex-1 bg-gradient-to-r from-transparent to-amber-500/40" />
                <span className="flex shrink-0 items-center gap-1.5">
                  <span className="size-3 shrink-0 rounded-full border-2 border-amber-500 border-t-transparent animate-spin" />
                  正在压缩上下文…
                </span>
                <span className="h-px flex-1 bg-gradient-to-l from-transparent to-amber-500/40" />
              </div>
            )}

            {sending && !compacting && (
              <div className="flex justify-start">
                <div className="bg-muted rounded-2xl rounded-bl-md px-4 py-3">
                  <div className="flex items-center gap-1">
                    <span className="typing-dot inline-block h-2 w-2 rounded-full bg-muted-foreground/60" style={{ animationDelay: "0ms" }} />
                    <span className="typing-dot inline-block h-2 w-2 rounded-full bg-muted-foreground/60" style={{ animationDelay: "200ms" }} />
                    <span className="typing-dot inline-block h-2 w-2 rounded-full bg-muted-foreground/60" style={{ animationDelay: "400ms" }} />
                  </div>
                </div>
              </div>
            )}

            <div ref={messagesEndRef} />
          </div>
        </div>

        {/* 实时进度面板：智能体维护按会话的 `todo.md` 清单，我们在输入框上方
            渲染它，使用户视线聚焦在即将授权的下一步，而非埋在长滚动历史的顶部。
            文件不存在或无复选框项时自动隐藏。 */}
        {!isEmpty && todoItems.length > 0 && (
          <TodoPanel items={todoItems} active={sending} />
        )}

        {/* 输入 */}
        <div className="shrink-0 px-4 pb-6 pt-2">
          <div className="mx-auto max-w-2xl relative">
            {isReadOnlyChannel && (
// Web 输入路径无法将消息送达上游 IM 平台（没有反向通道适配器，
                // 没有出站路由），因此在此处写入会静默损坏会话：智能体会处理该轮次，
                // IM 用户永远看不到它，刷新后原始会话历史胜出，因为孤立写入落在
                // 不匹配的三重查找下。直接阻止输入并告诉用户在何处回复。
              <div className="mb-2 rounded-lg border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
                此对话来自{" "}
                <span className="font-medium text-foreground">
                  {channelLabel(currentChannel)}
                </span>
                。请从对应平台回复，在此输入的消息不会发送给另一端用户。
              </div>
            )}
            {isActAsView && !isReadOnlyChannel && (
// 超级管理员通过管理聊天页面（?actAs=<uid>）查看另一用户的聊天。
                // 中间件将整个请求设为只读，因此任何发送都会 403——禁用
                // 输入框并说明原因。
              <div className="mb-2 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">
                只读：你正在查看其他用户的对话，无法在此发送消息。
              </div>
            )}
            {slashOpen && filteredItems.length > 0 && (
              <SlashMenu
                items={filteredItems}
                activeIndex={slashIndex}
                onHover={setSlashIndex}
                onSelect={selectItem}
              />
            )}
            <div
              className={
                "border border-border bg-card focus-within:ring-2 focus-within:ring-ring/20 transition-shadow " +
                (isEmpty ? "rounded-2xl px-5 pt-4 pb-3" : "rounded-xl px-4 py-3")
              }
            >
              {attachments.length > 0 && (
                <div className="flex flex-wrap gap-2 mb-2 pb-2 border-b border-border/60">
                  {attachments.map((f, i) => {
                    const preview = attachmentPreviews[i];
                    if (preview) {
                      return (
                        <div
                          key={`${f.name}-${i}`}
                          className="group relative h-14 w-14 overflow-hidden rounded-md border border-border bg-muted"
                        >
                          <button
                            type="button"
                            onClick={() => setLightboxSrc(preview)}
                            className="block h-full w-full cursor-zoom-in"
                            aria-label={`预览 ${f.name}`}
                          >
                            {/* eslint-disable-next-line @next/next/no-img-element */}
                            <img
                              src={preview}
                              alt={f.name}
                              className="h-full w-full object-cover"
                            />
                          </button>
                          <button
                            type="button"
                            onClick={() => removeAttachment(i)}
                            className="absolute right-0.5 top-0.5 flex h-4 w-4 items-center justify-center rounded-full bg-background/80 text-muted-foreground opacity-0 transition group-hover:opacity-100 hover:text-foreground"
                            aria-label="移除附件"
                          >
                            <X className="h-3 w-3" />
                          </button>
                        </div>
                      );
                    }
                    return (
                      <div
                        key={`${f.name}-${i}`}
                        className="flex items-center gap-1.5 rounded-md bg-muted/60 pl-2 pr-1 py-1 text-xs"
                      >
                        <Paperclip className="h-3 w-3 text-muted-foreground" />
                        <span className="max-w-[160px] truncate">{f.name}</span>
                        <button
                          type="button"
                          onClick={() => removeAttachment(i)}
                          className="p-0.5 rounded hover:bg-muted-foreground/15 text-muted-foreground hover:text-foreground"
                          aria-label="移除附件"
                        >
                          <X className="h-3 w-3" />
                        </button>
                      </div>
                    );
                  })}
                </div>
              )}
{/* 空状态输入框：Manus 风格——输入框填充顶部，操作行在下方。有消息后
                   切回紧凑单行布局，使输入框不会占据聊天主体。 */}
              {isEmpty ? (
                <>
                  <textarea
                    ref={textareaRef}
                    value={input}
                    onChange={handleInputChange}
                    onKeyDown={handleKeyDown}
                    onBlur={() => setTimeout(() => setSlashOpen(false), 120)}
                    placeholder={
                      isActAsView
                        ? "只读：正在查看其他用户的对话"
                        : isReadOnlyChannel
                          ? `只读 — reply from ${channelLabel(currentChannel)}`
                          : selectedAgent
                            ? `消息 ${agentName || selectedAgent}... ("/" to pick a skill)`
                            : "请先选择智能体"
                    }
                    disabled={!selectedAgent || isReadOnlyView}
                    rows={3}
                    className="block w-full resize-none bg-transparent text-[15px] placeholder:text-muted-foreground/50 outline-none disabled:opacity-50"
                    style={{ maxHeight: 240, minHeight: 72 }}
                  />
                  <div className="mt-2 flex items-center justify-between">
                    <div className="flex items-center gap-2 min-w-0">
                      <label
                        className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-full border border-border text-muted-foreground transition-colors ${
                          !selectedAgent || sending || isReadOnlyView
                            ? "opacity-50 cursor-not-allowed"
                            : "hover:bg-muted hover:text-foreground cursor-pointer"
                        }`}
                        aria-label="添加附件"
                      >
                        <Paperclip className="h-4 w-4" />
                        <input
                          ref={fileInputRef}
                          type="file"
                          multiple
                          className="sr-only"
                          onChange={handleFilePick}
                          disabled={!selectedAgent || sending || isReadOnlyView}
                        />
                      </label>
                      <button
                        type="button"
                        onClick={() => setPlanMode((enabled) => !enabled)}
                        disabled={!selectedAgent || sending || isReadOnlyView}
                        title={
                          planMode
                            ? "下一条消息将使用计划模式。点击可关闭。"
                            : "先制定计划：仅拟定步骤，不运行工具。"
                        }
                        aria-pressed={planMode}
                        className={`flex h-9 shrink-0 items-center gap-1.5 rounded-full border px-3 text-xs transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${
                          planMode
                            ? "border-amber-500/50 bg-amber-500/15 text-amber-900 dark:text-amber-200"
                            : "border-border text-muted-foreground hover:bg-muted hover:text-foreground"
                        }`}
                      >
                        <ListChecks className="h-3.5 w-3.5" />
                        <span>计划</span>
                      </button>
                      {urlProjectId && projectInfo && (
                        <div
                          className="flex h-9 min-w-0 items-center gap-1.5 rounded-full border border-border px-3 text-xs text-muted-foreground"
                          title={projectInfo.name}
                        >
                          <FolderOpen className="size-3.5 shrink-0" />
                          <span className="truncate max-w-[20ch]">
                            {projectInfo.name}
                          </span>
                        </div>
                      )}
                      {contextUsage && <ContextUsageBadge usage={contextUsage} />}
                    </div>
                    {sending ? (
                      <Button
                        onClick={handleStop}
                        size="icon"
                        className="h-9 w-9 shrink-0 rounded-full"
                        aria-label="停止生成"
                      >
                        <Square className="h-3.5 w-3.5 fill-current" />
                      </Button>
                    ) : (
                      <Button
                        onClick={() => handleSend()}
                        disabled={(!input.trim() && attachments.length === 0) || !selectedAgent || isReadOnlyView}
                        size="icon"
                        className="h-9 w-9 shrink-0 rounded-full"
                        aria-label="发送消息"
                      >
                        <Send className="h-4 w-4" />
                      </Button>
                    )}
                  </div>
                </>
              ) : (
                <div className="flex items-center gap-2">
                  <label
                    className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-lg text-muted-foreground transition-colors ${
                      !selectedAgent || sending || isReadOnlyView
                        ? "opacity-50 cursor-not-allowed"
                        : "hover:bg-muted hover:text-foreground cursor-pointer"
                    }`}
                    aria-label="添加附件"
                  >
                    <Paperclip className="h-4 w-4" />
                    <input
                      ref={fileInputRef}
                      type="file"
                      multiple
                      className="sr-only"
                      onChange={handleFilePick}
                      disabled={!selectedAgent || sending || isReadOnlyView}
                    />
                  </label>
                  <button
                    type="button"
                    onClick={() => setPlanMode((enabled) => !enabled)}
                    disabled={!selectedAgent || sending || isReadOnlyView}
                    title={
                      planMode
                        ? "下一条消息将使用计划模式。点击可关闭。"
                        : "先制定计划：仅拟定步骤，不运行工具。"
                    }
                    aria-pressed={planMode}
                    className={`flex h-8 shrink-0 items-center gap-1 rounded-lg px-2 text-xs transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${
                      planMode
                        ? "bg-amber-500/15 text-amber-900 ring-1 ring-amber-500/40 dark:text-amber-200"
                        : "text-muted-foreground hover:bg-muted hover:text-foreground"
                    }`}
                  >
                    <ListChecks className="h-3.5 w-3.5" />
                    <span>计划</span>
                  </button>
                  <textarea
                    ref={textareaRef}
                    value={input}
                    onChange={handleInputChange}
                    onKeyDown={handleKeyDown}
                    onBlur={() => setTimeout(() => setSlashOpen(false), 120)}
                    placeholder={
                      isActAsView
                        ? "只读：正在查看其他用户的对话"
                        : isReadOnlyChannel
                          ? `只读 — reply from ${channelLabel(currentChannel)}`
                          : selectedAgent
                            ? `消息 ${agentName || selectedAgent}... ("/" to pick a skill)`
                            : "请先选择智能体"
                    }
                    disabled={!selectedAgent || isReadOnlyView}
                    rows={1}
                    className="flex-1 resize-none bg-transparent text-[15px] leading-8 placeholder:text-muted-foreground/50 outline-none disabled:opacity-50"
                    style={{ maxHeight: 200, minHeight: 32 }}
                  />
                  {contextUsage && <ContextUsageBadge usage={contextUsage} />}
                  {sending ? (
                    <Button
                      onClick={handleStop}
                      size="icon"
                      className="h-8 w-8 shrink-0 rounded-lg"
                      aria-label="停止生成"
                    >
                      <Square className="h-3.5 w-3.5 fill-current" />
                    </Button>
                  ) : (
                    <Button
                      onClick={() => handleSend()}
                      disabled={(!input.trim() && attachments.length === 0) || !selectedAgent || isReadOnlyView}
                      size="icon"
                      className="h-8 w-8 shrink-0 rounded-lg"
                      aria-label="发送消息"
                    >
                      <Send className="h-4 w-4" />
                    </Button>
                  )}
                </div>
              )}
            </div>
          </div>
        </div>
        {lightboxSrc && (
          <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/80 p-6 cursor-zoom-out"
            onClick={() => setLightboxSrc(null)}
            role="dialog"
            aria-modal="true"
            aria-label="图片预览"
          >
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img
              src={lightboxSrc}
              alt="预览"
              className="max-h-full max-w-full rounded-lg shadow-2xl"
              onClick={(e) => e.stopPropagation()}
            />
            <button
              type="button"
              onClick={() => setLightboxSrc(null)}
              className="absolute right-4 top-4 flex h-9 w-9 items-center justify-center rounded-full bg-background/80 text-foreground hover:bg-background"
              aria-label="关闭预览"
            >
              <X className="h-5 w-5" />
            </button>
          </div>
        )}
      </div>
      {filesSheetOpen && selectedAgent && (sessionId || urlProjectId) && (
        <WorkspacePanel
          agentId={selectedAgent}
          // 在项目落地页（无 urlSessionId）时，此处的 sessionId 是 chat-screen
          // 为即将到来的"新聊天"生成的合成 id——它不对应磁盘上的任何内容，
          // 因此我们抑制它并让 projectId 驱动范围。在实际聊天中，urlSessionId
          // 已设置，我们传递真实的 sessionId。
          sessionId={urlSessionId ? sessionId : ""}
          projectId={!urlSessionId && urlProjectId ? urlProjectId : undefined}
          onClose={() => setFilesSheetOpen(false)}
        />
      )}
    </div>
  );
}

// ContextUsageBadge 渲染输入框页脚的「已用上下文百分比」指示器。
// 一条细进度条 + 百分比文字，并在阈值处画一道竖线标出后端自动压缩的
// 触发点（triggerTokens）。占用接近/越过该阈值时变为琥珀色，提示下一轮
// 可能触发压缩。百分比以完整上下文窗口为分母，便于直观对照模型上限。
function ContextUsageBadge({ usage }: { usage: ContextUsage }) {
  const { usedTokens, contextWindow, triggerTokens } = usage;
  const budgetTokens = usage.budgetTokens ?? usedTokens;
  const source = usage.source ?? "provider";
  if (!contextWindow || contextWindow <= 0) return null;
  const pct = Math.min(100, Math.max(0, (usedTokens / contextWindow) * 100));
  const budgetPct = Math.min(100, Math.max(0, (budgetTokens / contextWindow) * 100));
  const triggerPct =
    triggerTokens > 0 ? Math.min(100, (triggerTokens / contextWindow) * 100) : 0;
  const atLimit = triggerTokens > 0 && budgetTokens >= triggerTokens;
  const fmt = (n: number) =>
    n >= 1000 ? `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k` : `${n}`;
  const isEstimated = source === "estimate";
  const warn = atLimit || pct >= 90;
  return (
    <div
      className={`flex shrink-0 items-center gap-1.5 text-xs tabular-nums ${
        warn ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground"
      }`}
      title={`上下文占用：${isEstimated ? "约 " : ""}${fmt(usedTokens)} / ${fmt(contextWindow)} tokens（${pct.toFixed(0)}%）${
        source === "provider" && budgetTokens !== usedTokens
          ? `\n压缩判断口径约 ${fmt(budgetTokens)} tokens（${budgetPct.toFixed(0)}%）`
          : ""
      }${
        triggerPct > 0
          ? `\n自动压缩阈值约 ${triggerPct.toFixed(0)}%${atLimit ? " — 已达到，下一轮将压缩较早的历史" : ""}`
          : ""
      }`}
    >
      <span className="relative h-1.5 w-16 overflow-hidden rounded-full bg-muted">
        <span
          className={`absolute inset-y-0 left-0 rounded-full ${
            warn ? "bg-amber-500" : "bg-primary/60"
          }`}
          style={{ width: `${pct}%` }}
        />
        {triggerPct > 0 && triggerPct < 100 && (
          <span
            className="absolute inset-y-0 w-px bg-foreground/40"
            style={{ left: `${triggerPct}%` }}
            aria-hidden
          />
        )}
      </span>
      <span>{isEstimated ? "~" : ""}{pct.toFixed(0)}%</span>
    </div>
  );
}

interface ChatHeaderTitleProps {
  title: string;
  fallback: string;
  onSave: (next: string) => void | Promise<void>;
}

/** 可编辑的聊天标题，通过 usePageHeader 渲染到全局固定头部。
 *  点击/聚焦编辑；Enter 或失焦提交。 */
function ChatHeaderTitle({ title, fallback, onSave }: ChatHeaderTitleProps) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(title);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!editing) setDraft(title);
  }, [title, editing]);

  useEffect(() => {
    if (editing) inputRef.current?.select();
  }, [editing]);

  const commit = () => {
    setEditing(false);
    const next = draft.trim();
    if (!next || next === title) return;
    onSave(next);
  };

  if (editing) {
// field-sizing: content 使输入框随文本增长；min-width 在进入编辑模式后
      // 即使当前标题很短也保持合理宽度。
    return (
      <input
        ref={inputRef}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
// 当用户正在组合输入时（CJK IME）忽略 Enter。两个条件都很重要：
            // isComposing 是现代信号，keyCode 229 是某些浏览器（特别是 macOS 拼音）
            // 在不设置 isComposing 时仍发出的旧标记。
          if (e.nativeEvent.isComposing || e.keyCode === 229) return;
          if (e.key === "Enter") {
            e.preventDefault();
            commit();
          } else if (e.key === "Escape") {
            setDraft(title);
            setEditing(false);
          }
        }}
        onBlur={commit}
        style={{ fieldSizing: "content" } as React.CSSProperties}
        className="h-7 min-w-[8ch] max-w-[40ch] rounded-md bg-transparent px-2 text-sm outline-none ring-1 ring-border focus:ring-primary/40"
      />
    );
  }

  return (
    <button
      onClick={() => {
        setDraft(title);
        setEditing(true);
      }}
      // 响应式限制标题宽度，使长自动摘要不会在窄视口上将整个头部推出屏幕。
      // 任意 `min(...)` 使窄宽度手机保持 60vw，桌面端上限约 32rem；
      // sm:/md: 断点为中间尺寸提供确定性宽度。
      className="group flex min-w-0 max-w-[min(60vw,18rem)] sm:max-w-[24rem] md:max-w-[28rem] lg:max-w-[32rem] items-center gap-1.5 rounded-md px-2 py-1 text-sm text-foreground hover:bg-muted/50"
      title={title || fallback}
    >
      <span className="truncate">{title || fallback}</span>
      <Pencil className="h-3 w-3 shrink-0 text-muted-foreground/50 opacity-0 transition-opacity group-hover:opacity-100" />
    </button>
  );
}

/** 将一组工具调用渲染为可折叠摘要。当 `nested` 为 true 时，
 *  去掉外层 flex/max-width 包装，使父容器（ToolRoundsBundle）
 *  可以堆叠回合而每个回合不重新施加自己的气泡对齐。 */
function ToolCallGroup({ msg, surfacedSrcs, agentId, sessionId, nested = false, roundIndex, subagentProgress }: { msg: ChatMessage; surfacedSrcs?: ReadonlySet<string>; agentId: string; sessionId: string; nested?: boolean; roundIndex?: number; subagentProgress?: { iteration?: number; max?: number; phase?: "thinking" | "running" | "final-delivery" | "done"; tools?: string[] } | null }) {
  const [groupOpen, setGroupOpen] = useState(false);
  const [expandedTool, setExpandedTool] = useState<Record<string, boolean>>({});

  const tools = msg.toolCalls || [];
  const doneCount = tools.filter((tc) => tc.result != null).length;
  const allDone = doneCount === tools.length;

  // delegate_task 以串行注册，因此此轮中第一个尚未返回的
  // delegate_task 才对应活跃的 subagentProgress 事件流。
  // 较早的已完成；较晚的在互斥锁上排队，尚无进度。
  const activeDelegateId = (() => {
    for (const tc of tools) {
      if (tc.name === "delegate_task" && tc.result == null) {
        return tc.id;
      }
    }
    return null;
  })();

  const toggleTool = (id: string) =>
    setExpandedTool((prev) => ({ ...prev, [id]: !prev[id] }));

  const inner = (
    <>
      {/* 工具之前的内容 */}
      {msg.content && (
        <div className="bg-muted rounded-2xl rounded-bl-md px-4 py-2.5">
          <div className={CHAT_PROSE_CLASS}>
            {renderContentWithDataImages(msg.content, surfacedSrcs, false, makeUrlTransform(agentId, sessionId)) ?? (
              <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]} urlTransform={makeUrlTransform(agentId, sessionId)} components={{ a: ExternalAnchor }}>
                {msg.content}
              </ReactMarkdown>
            )}
          </div>
        </div>
      )}
      {/* 折叠的工具组摘要 */}
      <div className="rounded-lg border border-border bg-card/50 overflow-hidden">
          <button
            onClick={() => setGroupOpen(!groupOpen)}
            className="flex w-full items-center gap-2 px-3 py-2 text-xs hover:bg-muted/50 transition-colors"
          >
            {!allDone ? (
              <div className="h-5 w-5 shrink-0 rounded-full border-2 border-amber-500 border-t-transparent animate-spin" />
            ) : roundIndex !== undefined ? (
// 当此组是打包中的回合时，前导字形携带回合编号——
                  // 为打包的展开视图提供内置步骤指示器，
                  // 无需额外的"ROUND N"标签行。
              <span className="h-5 w-5 shrink-0 inline-flex items-center justify-center rounded-full bg-amber-500/10 text-[11px] font-semibold text-amber-600 dark:text-amber-400">
                {roundIndex}
              </span>
            ) : (
              <Wrench className="h-3.5 w-3.5 text-amber-500 shrink-0" />
            )}
            <span className="font-medium text-foreground">
              {allDone
                ? `Executed ${tools.length} tool${tools.length > 1 ? "s" : ""}`
                : `Running 工具 (${doneCount}/${tools.length})...`}
            </span>
            <span className="text-muted-foreground/60 text-[11px] flex-1 text-left truncate">
              {tools.map((tc) => tc.name).join(", ")}
            </span>
            {groupOpen ? (
              <ChevronDown className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            ) : (
              <ChevronRight className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            )}
          </button>

          {groupOpen && (
            <div className="border-t border-border">
              {tools.map((tc) => (
                <div key={tc.id} className="border-b border-border last:border-b-0">
                  <button
                    onClick={() => toggleTool(tc.id)}
                    className="flex w-full items-center gap-2 px-3 py-1.5 text-xs hover:bg-muted/30 transition-colors"
                  >
                    {tc.result === undefined ? (
                      <div className="h-3 w-3 shrink-0 rounded-full border-2 border-amber-500/60 border-t-transparent animate-spin" />
                    ) : (
                      <Check className="h-3 w-3 text-emerald-500 shrink-0" />
                    )}
                    <span className="font-medium text-foreground">{tc.name}</span>
                    {tc.metadata?.sandbox && (
                      <span
                        className="flex items-center gap-0.5 rounded bg-emerald-500/10 px-1 py-0.5 text-[10px] font-medium text-emerald-600 dark:text-emerald-400"
                        title="在沙箱容器内执行"
                      >
                        <ShieldCheck className="h-2.5 w-2.5" />
                        沙箱
                      </span>
                    )}
                    <span className="text-muted-foreground/50 font-mono truncate flex-1 text-left text-[11px]">
                      {(() => {
                        try {
                          const args = JSON.parse(tc.arguments);
                          // delegate_task 的 `task` 参数总是以相同的样板开头（"You are a B2B lead
                          // researcher…"）；区分部分是更下方的 markdown 标题
                          //（"## Target: <industry>"）。显示该行而非开头，
                          // 使 N 个委托的扇出不会看起来像 N 个相同调用。
                          if (tc.name === "delegate_task" && typeof args.task === "string") {
                            const m = args.task.match(/^#+\s*Target:\s*(.+)$/m) ||
                                      args.task.match(/^#+\s+(.+)$/m);
                            if (m) return m[1].trim();
                            return args.task.replace(/\s+/g, " ").slice(0, 120);
                          }
                          return Object.values(args).join(", ");
                        } catch {
                          return tc.arguments;
                        }
                      })()}
                    </span>
                    {expandedTool[tc.id] ? (
                      <ChevronDown className="h-3 w-3 text-muted-foreground/50 shrink-0" />
                    ) : (
                      <ChevronRight className="h-3 w-3 text-muted-foreground/50 shrink-0" />
                    )}
                  </button>
                  {expandedTool[tc.id] && (
                    <div className="px-3 py-2 space-y-2 bg-muted/20">
                      <div>
                        <p className="text-[10px] font-medium text-muted-foreground uppercase mb-1">输入</p>
                        <pre className="text-xs font-mono bg-muted/50 rounded p-2 overflow-x-auto whitespace-pre-wrap break-all max-h-40">
                          {(() => {
                            try { return JSON.stringify(JSON.parse(tc.arguments), null, 2); }
                            catch { return tc.arguments; }
                          })()}
                        </pre>
                      </div>
                      {tc.result != null ? (
                        <div>
                          <p className="text-[10px] font-medium text-muted-foreground uppercase mb-1">输出</p>
                          <pre className="text-xs font-mono bg-muted/50 rounded p-2 overflow-x-auto whitespace-pre-wrap break-all max-h-60">
                            {tc.result.length > 2000 ? tc.result.slice(0, 2000) + "..." : tc.result}
                          </pre>
                        </div>
                      ) : tc.name === "delegate_task" && tc.id === activeDelegateId && subagentProgress ? (
                        <div className="text-xs text-muted-foreground/80 italic">
                          {(() => {
                            const it = subagentProgress.iteration;
                            const mx = subagentProgress.max;
                            const phase = subagentProgress.phase;
                            const tools = subagentProgress.tools;
                            const counter = it && mx ? `Iteration ${it}/${mx}` : "Sub-agent running";
                            let detail = "";
                            if (phase === "thinking") detail = "thinking";
                            else if (phase === "running" && tools?.length) detail = `running ${tools.join(", ")}`;
                            else if (phase === "final-delivery") detail = "synthesizing final answer";
                            return detail ? `${counter} · ${detail}` : counter;
                          })()}
                        </div>
                      ) : tc.name === "delegate_task" && tc.result == null && tc.id !== activeDelegateId ? (
                        <p className="text-xs text-muted-foreground/60 italic">已排队（等待前一个子智能体完成）…</p>
                      ) : (
                        <p className="text-xs text-muted-foreground/60 italic">正在执行...</p>
                      )}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      </>
    );
  if (nested) {
    return <div className="space-y-2">{inner}</div>;
  }
  return (
    <div className="flex justify-start">
      <div className="max-w-[85%] space-y-2">{inner}</div>
    </div>
  );
}

/** ToolRoundsBundle 将连续的工具组回合（智能体运行工具、获取结果、
 *  再运行更多工具……）包装在单个可折叠头部中，使长时间 ReAct 轮次
 *  不会用七个独立的"已执行 N 个工具"框占据整个聊天。聚合徽章显示
 *  总回合数 + 总工具数；展开显示每个回合为常规 ToolCallGroup，各自
 *  仍可折叠。单回合不打包——这些仍渲染为扁平 ToolCallGroup，使
 *  额外层仅在有价值时出现。 */
function ToolRoundsBundle({
  rounds,
  surfacedSrcs,
  agentId,
  sessionId,
  subagentProgress,
}: {
  rounds: ChatMessage[];
  surfacedSrcs?: ReadonlySet<string>;
  agentId: string;
  sessionId: string;
  subagentProgress?: { iteration?: number; max?: number; phase?: "thinking" | "running" | "final-delivery" | "done"; tools?: string[] } | null;
}) {
  const [open, setOpen] = useState(false);
  const allTools = rounds.flatMap((r) => r.toolCalls || []);
  const totalTools = allTools.length;
  const doneCount = allTools.filter((tc) => tc.result != null).length;
  const allDone = doneCount === totalTools;
  return (
    <div className="flex justify-start">
      <div className="max-w-[85%] w-full">
        <div className="rounded-lg border border-border bg-card/50 overflow-hidden">
          <button
            onClick={() => setOpen(!open)}
            className="flex w-full items-center gap-2 px-3 py-2 text-xs hover:bg-muted/50 transition-colors"
          >
            {!allDone ? (
              <div className="h-3.5 w-3.5 shrink-0 rounded-full border-2 border-amber-500 border-t-transparent animate-spin" />
            ) : (
              <Wrench className="h-3.5 w-3.5 text-amber-500 shrink-0" />
            )}
            <span className="font-medium text-foreground">
              {allDone
                ? `Used ${totalTools} tool${totalTools === 1 ? "" : "s"} across ${rounds.length} round${rounds.length === 1 ? "" : "s"}`
                : `Running 工具… (${doneCount}/${totalTools} across ${rounds.length} rounds)`}
            </span>
            <span className="ml-auto" />
            {open ? (
              <ChevronDown className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            ) : (
              <ChevronRight className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            )}
          </button>
          {open && (
            <div className="border-t border-border p-2 space-y-1.5 bg-background/30">
              {rounds.map((round, idx) => (
                <ToolCallGroup
                  key={round.id || idx}
                  msg={round}
                  surfacedSrcs={surfacedSrcs}
                  agentId={agentId}
                  sessionId={sessionId}
                  nested
                  roundIndex={idx + 1}
                  subagentProgress={subagentProgress}
                />
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

/** TurnProcess 把"一轮已完成对话的中间过程"（工具回合 + 工具之间的叙述气泡）
 *  收进一个默认折叠的容器，标题只占一行："运行过程 · N 轮 · M 个工具"。最终输出
 *  （最后一次工具回合之后的文字）由调用方在容器外单独渲染并保持展开，因此回看
 *  历史时每条用户消息下方只剩「一行折叠条 + 最终回答」。点击标题展开后，内部按
 *  原样显示完整的工具卡片与中间叙述，便于回溯执行细节。Codex 风格的运行日志。 */
function TurnProcess({
  children,
  toolCount,
  roundCount,
}: {
  children: React.ReactNode;
  toolCount: number;
  roundCount: number;
}) {
  const [open, setOpen] = useState(false);
  return (
    <div className="flex justify-start">
      <div className="w-full max-w-[85%]">
        <button
          onClick={() => setOpen(!open)}
          className="flex w-full items-center gap-2 rounded-lg px-2.5 py-1.5 text-xs text-muted-foreground transition-colors hover:bg-muted/50"
        >
          <History className="h-3.5 w-3.5 shrink-0 text-muted-foreground/70" />
          <span className="font-medium text-foreground/80">运行过程</span>
          <span className="text-muted-foreground/60">
            {roundCount} 轮 · {toolCount} 个工具
          </span>
          <span className="ml-auto" />
          {open ? (
            <ChevronDown className="h-3.5 w-3.5 shrink-0" />
          ) : (
            <ChevronRight className="h-3.5 w-3.5 shrink-0" />
          )}
        </button>
        {open && (
          <div className="mt-1.5 space-y-2 border-l-2 border-border/60 pl-3">
            {children}
          </div>
        )}
      </div>
    </div>
  );
}

/** 文件扩展名 → 图标 + 预览类型。 */
function fileKind(path: string): { icon: typeof File; preview: "image" | "pdf" | "markdown" | "html" | "text" | "none" } {
  const ext = path.toLowerCase().split(".").pop() || "";
  if (["png", "jpg", "jpeg", "gif", "svg", "webp", "bmp", "ico"].includes(ext)) return { icon: ImageIcon, preview: "image" };
  if (ext === "pdf") return { icon: FileText, preview: "pdf" };
  if (ext === "md" || ext === "markdown") return { icon: FileText, preview: "markdown" };
  if (ext === "html" || ext === "htm") return { icon: FileCode, preview: "html" };
  if (["mp4", "webm", "mov", "mkv"].includes(ext)) return { icon: Film, preview: "none" };
  if (["mp3", "wav", "ogg", "flac", "m4a"].includes(ext)) return { icon: Music, preview: "none" };
  if (["js", "ts", "tsx", "jsx", "py", "go", "rs", "c", "cpp", "h", "java", "rb", "sh", "json", "yaml", "yml", "toml", "xml", "css"].includes(ext))
    return { icon: FileCode, preview: "text" };
  if (["txt", "csv", "log"].includes(ext)) return { icon: FileText, preview: "text" };
  return { icon: File, preview: "none" };
}

function formatBytes(n?: number): string {
  if (n === undefined) return "";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

// fileUrl / zipUrl 不再在查询字符串中携带 bearer token。Web UI 运行
// 在同源环境，认证中间件读取登录时设置的会话 cookie，因此 <img src>、
// <a href> 和直接下载均通过 cookie 认证，与所有其他 API 调用相同。
// 此前我们追加 `?token=<bearer>`，使仅持 bearer 的程序化客户端能渲染
// 图片，但该 token 是完整的 API 凭证——将其放入 URL 会通过 Referer
// （当工作区 HTML 文件链接到第三方站点时）、浏览器历史和反向代理访问
// 日志泄露。服务端仍接受 `?token=` 以向后兼容自建 URL 的 CLI 脚本；
// 前端仅停止提供它。
function fileUrl(agentId: string, path: string, download: boolean): string {
  const encoded = path.split("/").map(encodeURIComponent).join("/");
  const params = new URLSearchParams();
  if (download) params.set("download", "1");
  const qs = params.toString();
  return `/api/agents/${agentId}/files/${encoded}${qs ? "?" + qs : ""}`;
}

function zipUrl(agentId: string, sessionId: string, projectId?: string): string {
  const params = new URLSearchParams();
  // projectId 优先于 sessionId——与后端的 fileScopeForRequest 优先级一致，
  // 后者将 projectId-without-session 视为"整个项目 zip"。
  if (projectId) params.set("projectId", projectId);
  else if (sessionId) params.set("sessionId", sessionId);
  const qs = params.toString();
  return `/api/agents/${agentId}/files.zip${qs ? "?" + qs : ""}`;
}

function FilesPanel({ agentId, files }: { agentId: string; files: ProducedFile[] }) {
  const [previewing, setPreviewing] = useState<ProducedFile | null>(null);
  return (
    <>
      <div className="mt-2 space-y-1.5 max-w-[85%]">
        <p className="text-[11px] font-medium uppercase tracking-wider text-muted-foreground/70">
          你的文件
        </p>
        <div className="flex flex-col gap-1.5">
          {files.map((f) => {
            const { icon: Icon } = fileKind(f.path);
            const basename = f.path.split("/").pop() || f.path;
            const downloadUrl = fileUrl(agentId, f.path, true);
            return (
              <div
                key={f.path}
                className="group flex items-center gap-2.5 rounded-lg border border-border bg-card/50 px-3 py-2 hover:bg-card/80 transition-colors"
              >
                <Icon className="h-4 w-4 text-muted-foreground shrink-0" />
                <button
                  onClick={() => setPreviewing(f)}
                  className="flex-1 min-w-0 text-left"
                  title="打开预览"
                >
                  <div className="text-sm font-medium text-foreground truncate">{basename}</div>
                  {f.size !== undefined && (
                    <div className="text-[11px] text-muted-foreground/70">{formatBytes(f.size)}</div>
                  )}
                </button>
                <a
                  href={downloadUrl}
                  className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors"
                  title="下载"
                >
                  <Download className="h-4 w-4" />
                </a>
              </div>
            );
          })}
        </div>
      </div>
      {previewing && (
        <FilePreview
          agentId={agentId}
          file={previewing}
          onClose={() => setPreviewing(null)}
        />
      )}
    </>
  );
}

const FILES_PANEL_MIN = 280;
const FILES_PANEL_MAX = 640;
const FILES_PANEL_DEFAULT = 280;
const FILES_PANEL_KEY = "chat:filesPanelWidth";

// WorkspacePanel 在活跃范围中渲染文件：
//   - 聊天范围（设置了 sessionId）：此对话中产出的文件。项目聊天也显示根级
//     项目文件，使共享笔记与聊天自身产出并列可见。
//   - 项目范围（设置了 projectId，无会话）：项目下所有文件——根级 + 每个聊天的
//     子树。用于 /agents/<aid>/project/<pid> 落地页，未选特定聊天时用户仍可查看
//     项目中积累的内容。
// 智能体的共享文件（SKILL.md / main.py / templates）由后端范围过滤器排除，
// 不会泄露到任一视图中混淆"此对话产出了什么"。
function WorkspacePanel({
  agentId,
  sessionId,
  projectId,
  onClose,
}: {
  agentId: string;
  sessionId: string;
  projectId?: string;
  onClose: () => void;
}) {
  const [files, setFiles] = useState<WorkspaceFile[]>([]);
  const [loading, setLoading] = useState(false);
  const [previewing, setPreviewing] = useState<ProducedFile | null>(null);
  // 自托管专用的"在 Finder 中打开"功能。我们在挂载时从 /api/me 获取
  // 部署模式；运行时不变，因此每个面板实例一次获取就够了。托管部署
  // 保持 null，按钮永不渲染。
  const [deployMode, setDeployMode] = useState<"self-hosted" | "hosted" | null>(null);
  const [revealing, setRevealing] = useState(false);
  useEffect(() => {
    let cancelled = false;
    getMe()
      .then((m) => {
        if (cancelled) return;
        if (m.deployMode === "self-hosted" || m.deployMode === "hosted") {
          setDeployMode(m.deployMode);
        }
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);
  const [width, setWidth] = useState<number>(() => {
    if (typeof window === "undefined") return FILES_PANEL_DEFAULT;
    const stored = Number(window.localStorage.getItem(FILES_PANEL_KEY));
    if (Number.isFinite(stored) && stored >= FILES_PANEL_MIN && stored <= FILES_PANEL_MAX) {
      return stored;
    }
    return FILES_PANEL_DEFAULT;
  });
  const [resizing, setResizing] = useState(false);

  useEffect(() => {
    if (!resizing) return;
    const handleMove = (e: MouseEvent) => {
      const next = Math.min(
        FILES_PANEL_MAX,
        Math.max(FILES_PANEL_MIN, window.innerWidth - e.clientX),
      );
      setWidth(next);
    };
    const handleUp = () => {
      setResizing(false);
      try {
        window.localStorage.setItem(FILES_PANEL_KEY, String(width));
      } catch { /* 忽略配额错误 */ }
    };
    window.addEventListener("mousemove", handleMove);
    window.addEventListener("mouseup", handleUp);
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
    return () => {
      window.removeEventListener("mousemove", handleMove);
      window.removeEventListener("mouseup", handleUp);
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
    };
  }, [resizing, width]);

  const handleReveal = useCallback(async () => {
    if (!agentId || (!sessionId && !projectId)) return;
    setRevealing(true);
    try {
      const res = await revealAgentWorkspace(agentId, sessionId || undefined, projectId);
      if (!res.ok) {
// 尽力 UX——行内显示错误而非我们没有的 toast 库。消息来自后端
      // （例如"S3-backed store, no host path"）。
        // eslint-disable-next-line no-alert
        alert(res.error || "无法打开工作区文件夹");
      }
    } finally {
      setRevealing(false);
    }
  }, [agentId, sessionId, projectId]);

  const refresh = useCallback(async () => {
    // 项目范围（无会话）通过 projectId 处理；聊天范围需要 sessionId。
    // 两者都没有时，无需获取。
    if (!agentId || (!sessionId && !projectId)) return;
    setLoading(true);
    try {
      // 设置 projectId 时跳过 sessionId——后端范围过滤器期望恰好一个来驱动
      // 前缀匹配。混合它们会进入聊天范围分支并遗漏其他聊天的文件。
      const list = projectId
        ? await listAgentFiles(agentId, undefined, projectId)
        : await listAgentFiles(agentId, sessionId);
      const cleaned = list
        .filter((f) => !isSystemFile(f.path))
        .sort((a, b) => (b.modTime || 0) - (a.modTime || 0));
      setFiles(cleaned);
    } finally {
      setLoading(false);
    }
  }, [agentId, sessionId, projectId]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  return (
    <>
      <aside
        style={{ width }}
        className="relative z-30 hidden md:flex shrink-0 flex-col border-l border-border bg-background -mt-12 h-screen"
      >
        <div
          onMouseDown={(e) => { e.preventDefault(); setResizing(true); }}
          className={`absolute -left-1 top-0 bottom-0 w-2 cursor-col-resize z-10 group ${resizing ? "" : ""}`}
          title="拖动以调整大小"
        >
          <div
            className={`absolute inset-y-0 left-1/2 w-px -translate-x-1/2 transition-colors ${
              resizing ? "bg-primary" : "bg-transparent group-hover:bg-primary/40"
            }`}
          />
        </div>
        <div className="flex items-center justify-between gap-2 px-4 h-12 border-b border-border">
          <div className="flex items-center gap-2 text-sm font-medium">
            <FolderOpen className="h-4 w-4" />
            工作区
          </div>
          <div className="flex items-center gap-1">
            <a
              href={
                files.length > 0
                  ? zipUrl(agentId, sessionId, projectId)
                  : undefined
              }
              aria-disabled={files.length === 0}
              className={`p-1.5 rounded-md transition-colors ${
                files.length === 0
                  ? "text-muted-foreground/40 pointer-events-none"
                  : "text-muted-foreground hover:text-foreground hover:bg-muted/50"
              }`}
              title="全部下载为 ZIP"
            >
              <Download className="h-4 w-4" />
            </a>
{/* 在操作系统的文件浏览器中打开工作区文件夹。仅限自托管——
                 托管部署不暴露有意义的"本地文件夹"，因此按钮完全隐藏
                 （我们在挂载时从 /api/me 获取部署模式）。 */}
            {deployMode === "self-hosted" && (
              <button
                onClick={handleReveal}
                disabled={revealing || (!sessionId && !projectId)}
                className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors disabled:opacity-50"
                title="在文件管理器中打开文件夹"
              >
                <FolderSearch className="h-4 w-4" />
              </button>
            )}
            <button
              onClick={refresh}
              disabled={loading}
              className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors disabled:opacity-50"
              title="刷新"
            >
              <RefreshCw className={`h-4 w-4 ${loading ? "animate-spin" : ""}`} />
            </button>
            <button
              onClick={onClose}
              className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors"
              title="关闭"
            >
              <X className="h-4 w-4" />
            </button>
          </div>
        </div>
        <div className="flex-1 overflow-y-auto p-2">
          {!loading && files.length === 0 ? (
            <p className="px-3 py-8 text-center text-sm text-muted-foreground">
              {projectId
                ? "此项目中暂无文件。"
                : "此会话中暂无文件。"}
            </p>
          ) : (
            <div className="flex flex-col">
              <div className="grid grid-cols-[1fr_auto_auto] gap-3 px-3 py-2 text-[11px] font-medium uppercase tracking-wider text-muted-foreground/70 border-b">
                <span>名称</span>
                <span>修改时间</span>
                <span>大小</span>
              </div>
              {files.map((f) => {
                const { icon: Icon } = fileKind(f.path);
                const basename = f.path.split("/").pop() || f.path;
                const downloadUrl = fileUrl(agentId, f.path, true);
                return (
                  <div
                    key={f.path}
                    className="group grid grid-cols-[1fr_auto_auto] items-center gap-3 px-3 py-2 hover:bg-muted/40 rounded-md transition-colors"
                  >
                    <button
                      onClick={() => setPreviewing({ path: f.path, size: f.size })}
                      className="flex items-center gap-2 min-w-0 text-left"
                      title="打开预览"
                    >
                      <Icon className="h-4 w-4 text-muted-foreground shrink-0" />
                      <span className="text-sm text-foreground truncate">{basename}</span>
                    </button>
                    <span className="text-[11px] text-muted-foreground/70 whitespace-nowrap">
                      {formatRelativeTime(f.modTime)}
                    </span>
                    <a
                      href={downloadUrl}
                      className="text-[11px] text-muted-foreground/70 whitespace-nowrap hover:text-foreground"
                      title="下载"
                    >
                      {formatBytes(f.size)}
                    </a>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </aside>
      {previewing && (
        <FilePreview
          agentId={agentId}
          file={previewing}
          onClose={() => setPreviewing(null)}
        />
      )}
    </>
  );
}

function formatRelativeTime(ts?: number): string {
  if (!ts) return "—";
  const d = new Date(ts * 1000);
  const now = Date.now();
  const diff = now - d.getTime();
  if (diff < 60_000) return "刚刚";
  if (diff < 3600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  if (diff < 86400_000) return `${Math.floor(diff / 3600_000)} 小时前`;
  if (diff < 7 * 86400_000) return `${Math.floor(diff / 86400_000)} 天前`;
  return d.toLocaleDateString("zh-CN", { month: "short", day: "numeric" });
}

function FilePreview({ agentId, file, onClose }: { agentId: string; file: ProducedFile; onClose: () => void }) {
  const { preview } = fileKind(file.path);
  const src = fileUrl(agentId, file.path, false);
  const downloadUrl = fileUrl(agentId, file.path, true);
  const basename = file.path.split("/").pop() || file.path;
  const [text, setText] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [htmlView, setHtmlView] = useState<"rendered" | "source">("rendered");

  useEffect(() => {
    // 仅当用户切换到源码视图时，HTML 懒加载其文本。
    if (preview !== "markdown" && preview !== "text") return;
    let cancelled = false;
    fetch(src)
      .then((r) => { if (!r.ok) throw new Error(`HTTP ${r.status}`); return r.text(); })
      .then((t) => { if (!cancelled) setText(t); })
      .catch((e) => { if (!cancelled) setError(String(e)); });
    return () => { cancelled = true; };
  }, [src, preview]);

  useEffect(() => {
    if (preview !== "html" || htmlView !== "source" || text !== null) return;
    let cancelled = false;
    fetch(src)
      .then((r) => { if (!r.ok) throw new Error(`HTTP ${r.status}`); return r.text(); })
      .then((t) => { if (!cancelled) setText(t); })
      .catch((e) => { if (!cancelled) setError(String(e)); });
    return () => { cancelled = true; };
  }, [src, preview, htmlView, text]);

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-background/80 backdrop-blur-sm p-4" onClick={onClose}>
      <div className="flex h-[85vh] w-full max-w-4xl flex-col rounded-xl border border-border bg-card shadow-2xl" onClick={(e) => e.stopPropagation()}>
        <div className="flex items-center justify-between border-b border-border px-4 py-3 shrink-0">
          <div className="flex items-center gap-2 min-w-0">
            <FileText className="h-4 w-4 text-muted-foreground shrink-0" />
            <span className="font-medium text-sm truncate">{basename}</span>
            {file.size !== undefined && (
              <span className="text-[11px] text-muted-foreground shrink-0">{formatBytes(file.size)}</span>
            )}
          </div>
          <div className="flex items-center gap-1 shrink-0">
            {preview === "html" && (
              <button
                onClick={() => setHtmlView(htmlView === "rendered" ? "source" : "rendered")}
                className="rounded-md p-1.5 text-muted-foreground hover:text-foreground hover:bg-muted/50"
                title={htmlView === "rendered" ? "查看源码" : "查看渲染结果"}
              >
                {htmlView === "rendered" ? <Code2 className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
              </button>
            )}
            <a
              href={downloadUrl}
              className="rounded-md p-1.5 text-muted-foreground hover:text-foreground hover:bg-muted/50"
              title="下载"
            >
              <Download className="h-4 w-4" />
            </a>
            <button
              onClick={onClose}
              className="rounded-md p-1.5 text-muted-foreground hover:text-foreground hover:bg-muted/50"
              title="关闭"
            >
              <X className="h-4 w-4" />
            </button>
          </div>
        </div>
        <div className="flex-1 overflow-auto p-4 min-h-0">
          {preview === "image" && (
            <img src={src} alt={basename} className="max-w-full max-h-full mx-auto object-contain" />
          )}
          {preview === "pdf" && (
            <iframe src={src} className="h-full w-full border-0" title={basename} />
          )}
          {preview === "markdown" && (
            error ? <p className="text-sm text-destructive">加载失败： {error}</p>
            : text === null ? <p className="text-sm text-muted-foreground">正在加载…</p>
            : (
              <div className="prose prose-sm dark:prose-invert max-w-none">
                <ReactMarkdown remarkPlugins={[remarkGfm]} components={{ a: ExternalAnchor }}>{text}</ReactMarkdown>
              </div>
            )
          )}
          {preview === "text" && (
            error ? <p className="text-sm text-destructive">加载失败： {error}</p>
            : text === null ? <p className="text-sm text-muted-foreground">正在加载…</p>
            : (
              <pre className="text-xs font-mono whitespace-pre-wrap break-all bg-muted/30 rounded p-3">{text}</pre>
            )
          )}
          {preview === "html" && (
            htmlView === "rendered" ? (
// sandbox="allow-scripts" 在空源中运行页面：CSS、动画、图表可以工作，
                    // 但脚本无法访问父级 cookie/storage/API——对不受信任的智能体输出安全。
              <iframe
                src={src}
                sandbox="allow-scripts"
                className="h-full w-full border-0 rounded bg-white"
                title={basename}
              />
            ) : error ? <p className="text-sm text-destructive">加载失败： {error}</p>
            : text === null ? <p className="text-sm text-muted-foreground">正在加载…</p>
            : (
              <pre className="text-xs font-mono whitespace-pre-wrap break-all bg-muted/30 rounded p-3">{text}</pre>
            )
          )}
          {preview === "none" && (
            <div className="flex flex-col items-center justify-center h-full gap-3 text-center">
              <File className="h-12 w-12 text-muted-foreground/50" />
              <p className="text-sm text-muted-foreground">此文件类型不支持预览。</p>
              <a href={downloadUrl} className="inline-flex items-center gap-2 rounded-md bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground hover:bg-primary/90">
                <Download className="h-3.5 w-3.5" /> 下载
              </a>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function SlashMenu({
  items,
  activeIndex,
  onHover,
  onSelect,
}: {
  items: SlashItem[];
  activeIndex: number;
  onHover: (i: number) => void;
  onSelect: (s: SlashItem) => void;
}) {
  return (
    <div className="absolute bottom-full left-0 right-0 mb-2 rounded-xl border border-border bg-popover shadow-lg overflow-hidden z-20">
      <div className="max-h-[320px] overflow-y-auto py-1">
        {items.map((it, i) => {
          const isCmd = it.kind === "command";
          const Icon = isCmd ? Terminal : Puzzle;
          const badge = isCmd ? "command" : (it.type || "skill");
          const label = isCmd ? `/${it.name}` : it.name;
          return (
            <button
              key={`${it.kind}-${it.name}`}
              // onMouseDown 在 textarea 的 onBlur 之前触发，因此点击
                // 不会被 blur 驱动的菜单关闭吞没。
              onMouseDown={(e) => {
                e.preventDefault();
                onSelect(it);
              }}
              onMouseEnter={() => onHover(i)}
              className={`w-full flex items-start gap-3 px-3 py-2 text-left transition-colors ${
                i === activeIndex ? "bg-muted/60" : "hover:bg-muted/40"
              }`}
            >
              <Icon className="h-4 w-4 mt-0.5 shrink-0 text-muted-foreground" />
              <div className="flex-1 min-w-0">
                <div className="flex items-center gap-2">
                  <p className="text-sm font-medium truncate">{label}</p>
                  <span className="text-[10px] uppercase tracking-wider text-muted-foreground/70">
                    {badge}
                  </span>
                </div>
                {it.description && (
                  <p className="text-xs text-muted-foreground line-clamp-1">
                    {it.description}
                  </p>
                )}
              </div>
            </button>
          );
        })}
      </div>
      <Link
        href="/skills/"
        className="flex items-center gap-2 border-t border-border px-3 py-2 text-xs text-muted-foreground hover:text-foreground hover:bg-muted/30 transition-colors"
      >
        <SlidersHorizontal className="h-3.5 w-3.5" />
        管理技能
      </Link>
    </div>
  );
}
