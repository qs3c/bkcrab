"use client";

import * as React from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import {
  AlertCircle,
  ArrowLeft,
  ChevronDown,
  Database,
  FileText,
  History,
  Loader2,
  MessageSquareText,
  Plus,
  Send,
} from "lucide-react";
import {
  askKnowledgeBase,
  getKnowledgeBase,
  listKnowledgeChatSessions,
  listKnowledgeChatTurns,
  type KnowledgeBase,
  type KnowledgeChatSession,
  type KnowledgeSearchHit,
} from "@/lib/api";
import { RAGResourceGallery } from "@/components/rag-resource-gallery";
import { RAGAnswerMarkdown } from "@/components/rag-safe-render";
import {
  appendActAs,
  collectRAGResources,
  safeRAGMarkdownURL,
} from "@/components/rag-resource-gallery-state";
import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

type ChatTurn = {
  id: string;
  question: string;
  answer: string;
  hits: KnowledgeSearchHit[];
  status: "pending" | "done" | "error";
  createdAt?: string;
};

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

function newTurnID(): string {
  return `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

export default function KnowledgeChatClient() {
  const searchParams = useSearchParams();
  const kbId = searchParams.get("id")?.trim() || "";
  const actAs = searchParams.get("actAs")?.trim() || "";
  const [knowledgeBase, setKnowledgeBase] = React.useState<KnowledgeBase | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [loadError, setLoadError] = React.useState("");
  const [sessions, setSessions] = React.useState<KnowledgeChatSession[]>([]);
  const [sessionId, setSessionId] = React.useState("");
  const [historyLoading, setHistoryLoading] = React.useState(false);
  const [turns, setTurns] = React.useState<ChatTurn[]>([]);
  const [input, setInput] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const requestRef = React.useRef<AbortController | null>(null);
  const bottomRef = React.useRef<HTMLDivElement>(null);

  React.useEffect(() => {
    if (!kbId) return;
    let cancelled = false;
    setLoading(true);
    setLoadError("");
    const controller = new AbortController();
    void Promise.all([getKnowledgeBase(kbId), listKnowledgeChatSessions(kbId)])
      .then(async ([kb, storedSessions]) => {
        if (cancelled) return;
        setKnowledgeBase(kb);
        setSessions(storedSessions);
        const latest = storedSessions[0];
        if (!latest) {
          setSessionId("");
          setTurns([]);
          return;
        }
        setSessionId(latest.id);
        setHistoryLoading(true);
        const storedTurns = await listKnowledgeChatTurns(kbId, latest.id, controller.signal);
        if (!cancelled) {
          setTurns(storedTurns.map((turn) => ({ ...turn, status: "done" as const })));
        }
      })
      .catch((error) => {
        if (!cancelled) setLoadError(errorMessage(error, "读取知识库失败"));
      })
      .finally(() => {
        if (!cancelled) {
          setLoading(false);
          setHistoryLoading(false);
        }
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [actAs, kbId]);

  React.useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [turns]);

  React.useEffect(() => () => requestRef.current?.abort(), []);

  const startNewChat = React.useCallback(() => {
    requestRef.current?.abort();
    requestRef.current = null;
    setTurns([]);
    setSessionId("");
    setInput("");
    setLoadError("");
    setHistoryLoading(false);
    setSending(false);
  }, []);

  const openSession = React.useCallback(async (nextSessionID: string) => {
    if (!nextSessionID || nextSessionID === sessionId || sending) return;
    requestRef.current?.abort();
    const controller = new AbortController();
    requestRef.current = controller;
    setSessionId(nextSessionID);
    setTurns([]);
    setHistoryLoading(true);
    setLoadError("");
    try {
      const storedTurns = await listKnowledgeChatTurns(kbId, nextSessionID, controller.signal);
      setTurns(storedTurns.map((turn) => ({ ...turn, status: "done" as const })));
    } catch (error) {
      if (error instanceof DOMException && error.name === "AbortError") return;
      setLoadError(errorMessage(error, "读取问答历史失败"));
    } finally {
      if (requestRef.current === controller) {
        requestRef.current = null;
        setHistoryLoading(false);
      }
    }
  }, [kbId, sending, sessionId]);

  const sendQuestion = React.useCallback(async (event?: React.FormEvent) => {
    event?.preventDefault();
    const question = input.trim();
    if (!question || sending || !knowledgeBase) return;

    const turnID = newTurnID();
    const controller = new AbortController();
    requestRef.current = controller;
    setTurns((current) => [
      ...current,
      { id: turnID, question, answer: "", hits: [], status: "pending" },
    ]);
    setInput("");
    setSending(true);

    try {
      const response = await askKnowledgeBase(kbId, question, sessionId || undefined, controller.signal);
      setSessionId(response.sessionId);
      setTurns((current) => current.map((turn) => (
        turn.id === turnID
          ? { ...turn, id: response.id || turn.id, answer: response.answer, hits: response.hits, createdAt: response.createdAt, status: "done" }
          : turn
      )));
      void listKnowledgeChatSessions(kbId).then(setSessions).catch(() => undefined);
    } catch (error) {
      if (error instanceof DOMException && error.name === "AbortError") return;
      setTurns((current) => current.map((turn) => (
        turn.id === turnID
          ? { ...turn, answer: errorMessage(error, "问答请求失败，请重试"), status: "error" }
          : turn
      )));
    } finally {
      if (requestRef.current === controller) {
        requestRef.current = null;
        setSending(false);
      }
    }
  }, [input, kbId, knowledgeBase, sending, sessionId]);

  const pageError = kbId ? loadError : "缺少知识库 ID，请从知识库详情页重新进入。";
  const pageLoading = !!kbId && loading;

  return (
    <div className="flex min-h-[calc(100svh-2rem)] flex-col p-4 md:p-6">
      <header className="mx-auto flex w-full max-w-5xl items-center gap-3 border-b pb-4">
        <Link
          href={appendActAs("/knowledge/", actAs)}
          aria-label="返回知识库"
          className={buttonVariants({ variant: "ghost", size: "icon" })}
        >
          <ArrowLeft className="size-4" />
        </Link>
        <span className="flex size-9 shrink-0 items-center justify-center rounded-xl bg-primary/10 text-primary">
          <Database className="size-5" />
        </span>
        <div className="min-w-0 flex-1">
          {pageLoading ? (
            <div className="space-y-1.5">
              <Skeleton className="h-5 w-40" />
              <Skeleton className="h-3.5 w-64" />
            </div>
          ) : (
            <>
              <div className="flex items-center gap-2">
                <h1 className="truncate text-lg font-semibold">{knowledgeBase?.name || "知识库问答"}</h1>
                <Badge variant="secondary" className="hidden sm:inline-flex">知识库问答</Badge>
              </div>
              <p className="truncate text-xs text-muted-foreground">
                {knowledgeBase?.description || "每次独立检索并回答，不携带历史模型回复。"}
              </p>
            </>
          )}
        </div>
        <Select
          value={sessionId || "__new__"}
          onValueChange={(value) => {
            if (!value) return;
            if (value === "__new__") startNewChat();
            else void openSession(value);
          }}
          disabled={loading || historyLoading || sending}
        >
          <SelectTrigger size="sm" className="max-w-36 sm:max-w-52">
            <History className="size-3.5" />
            <SelectValue>
              {sessionId
                ? sessions.find((session) => session.id === sessionId)?.title || "历史问答"
                : "新问答"}
            </SelectValue>
          </SelectTrigger>
          <SelectContent align="end">
            <SelectItem value="__new__">新问答</SelectItem>
            {sessions.map((session) => (
              <SelectItem key={session.id} value={session.id}>
                {session.title}（{session.turnCount}）
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button variant="outline" size="sm" onClick={startNewChat} disabled={turns.length === 0 && !sending}>
          <Plus className="size-3.5" />
          <span className="hidden md:inline">新建问答</span>
        </Button>
      </header>

      <main className="mx-auto flex w-full max-w-5xl flex-1 flex-col py-5">
        {historyLoading ? (
          <div className="m-auto flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            正在读取历史问答…
          </div>
        ) : pageError ? (
          <Card className="m-auto w-full max-w-lg border-destructive/30">
            <CardContent className="flex items-start gap-3 text-sm text-destructive">
              <AlertCircle className="mt-0.5 size-4 shrink-0" />
              <span>{pageError}</span>
            </CardContent>
          </Card>
        ) : turns.length === 0 ? (
          <div className="m-auto flex max-w-lg flex-col items-center px-6 py-12 text-center">
            <span className="mb-4 flex size-14 items-center justify-center rounded-2xl bg-primary/10 text-primary">
              <MessageSquareText className="size-7" />
            </span>
            <h2 className="text-lg font-semibold">向这个知识库提问</h2>
            <p className="mt-2 text-sm leading-6 text-muted-foreground">
              回答仅依据本轮召回的知识库资料。问答记录会自动保存；连续提问时，最近 20 个用户问题会作为指代线索，模型之前的回答不会进入下一轮上下文。
            </p>
          </div>
        ) : (
          <div className="space-y-6 pb-4">
            {turns.map((turn) => (
              <div key={turn.id} className="space-y-3">
                <div className="ml-auto max-w-[85%] rounded-2xl rounded-br-md bg-primary px-4 py-2.5 text-sm leading-6 text-primary-foreground">
                  <p className="whitespace-pre-wrap">{turn.question}</p>
                </div>

                <div className="flex max-w-[92%] items-start gap-3">
                  <span className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground">
                    <Database className="size-4" />
                  </span>
                  <div className={cn(
                    "min-w-0 flex-1 rounded-2xl rounded-tl-md border bg-card px-4 py-3",
                    turn.status === "error" && "border-destructive/30 bg-destructive/5 text-destructive",
                  )}>
                    {turn.status === "pending" ? (
                      <div className="flex items-center gap-2 py-1 text-sm text-muted-foreground">
                        <Loader2 className="size-4 animate-spin" />
                        正在检索并生成回答…
                      </div>
                    ) : turn.status === "error" ? (
                      <div className="flex items-start gap-2 text-sm">
                        <AlertCircle className="mt-0.5 size-4 shrink-0" />
                        <span>{turn.answer}</span>
                      </div>
                    ) : (
                      <>
                        <div className="prose prose-sm max-w-none text-[15px] leading-relaxed dark:prose-invert prose-p:my-1 prose-pre:my-2 prose-ul:my-1 prose-ol:my-1">
                          <RAGAnswerMarkdown urlTransform={safeRAGMarkdownURL}>
                            {turn.answer}
                          </RAGAnswerMarkdown>
                        </div>
                        <RAGResourceGallery
                          resources={collectRAGResources(turn.hits)}
                          actAs={actAs}
                          showDisclosure
                          className="mt-4"
                        />
                        {turn.hits.length > 0 && <KnowledgeSources hits={turn.hits} actAs={actAs} />}
                      </>
                    )}
                  </div>
                </div>
              </div>
            ))}
            <div ref={bottomRef} />
          </div>
        )}
      </main>

      <div className="sticky bottom-0 mx-auto w-full max-w-5xl border-t bg-background/95 pt-3 backdrop-blur supports-[backdrop-filter]:bg-background/80">
        <form onSubmit={sendQuestion} className="flex items-end gap-2">
          <Textarea
            value={input}
            onChange={(event) => setInput(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter" && !event.shiftKey) {
                event.preventDefault();
                void sendQuestion();
              }
            }}
            placeholder={pageLoading ? "正在读取知识库…" : "输入问题，Enter 发送，Shift + Enter 换行"}
            rows={2}
            maxLength={8000}
            disabled={pageLoading || historyLoading || !!pageError || sending}
            className="min-h-12 max-h-40 resize-none"
          />
          <Button type="submit" size="icon-lg" disabled={!input.trim() || pageLoading || historyLoading || !!pageError || sending} aria-label="发送">
            {sending ? <Loader2 className="size-4 animate-spin" /> : <Send className="size-4" />}
          </Button>
        </form>
        <p className="py-2 text-center text-[11px] text-muted-foreground">
          问答记录与引用会自动保存；每轮仅生成一次回答。
        </p>
      </div>
    </div>
  );
}

function KnowledgeSources({ hits, actAs }: { hits: KnowledgeSearchHit[]; actAs?: string }) {
  return (
    <details className="group mt-3 border-t pt-3">
      <summary className="flex cursor-pointer list-none items-center gap-1.5 text-xs font-medium text-muted-foreground hover:text-foreground">
        <ChevronDown className="size-3.5 transition-transform group-open:rotate-180" />
        查看本轮引用（{hits.length}）
      </summary>
      <div className="mt-3 space-y-2">
        {hits.map((hit, index) => (
          <div key={`${hit.docId}-${hit.chunkIndex}-${index}`} className="rounded-lg bg-muted/50 p-3 text-xs">
            <div className="mb-1.5 flex flex-wrap items-center gap-1.5 text-muted-foreground">
              <Badge variant="outline" className="h-5 px-1.5">[{index + 1}]</Badge>
              <FileText className="size-3.5" />
              <span className="font-medium text-foreground">{hit.docName}</span>
              {hit.sectionTitle && <span>· {hit.sectionTitle}</span>}
              {hit.pageNum ? <span>· 第 {hit.pageNum} 页</span> : null}
              <span>· 分片 {hit.chunkIndex + 1}</span>
            </div>
            <p className="whitespace-pre-wrap leading-5 text-muted-foreground">{hit.content}</p>
            <RAGResourceGallery
              resources={collectRAGResources([hit])}
              actAs={actAs}
              title="引用图片"
              compact
              className="mt-2"
            />
          </div>
        ))}
      </div>
    </details>
  );
}
