"use client";

import { useEffect, useMemo, useState } from "react";
import { Coins, RefreshCcw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  getAgentTokenUsage,
  getChatSessions,
  type AgentTokenUsage,
  type ChatSessionEntry,
  type TokenUsageRange,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

const RANGES: { value: TokenUsageRange; label: string }[] = [
  { value: "24h", label: "24h" },
  { value: "7d", label: "7d" },
  { value: "30d", label: "30d" },
];

// fmt 将大数字折叠为 12.3K / 4.5M 格式用于表格。1000 以下保留
// 原始数值，这样快速测试会话会显示 "47" 而不是 "0.0K"。
function fmt(n: number): string {
  if (!Number.isFinite(n)) return "—";
  if (Math.abs(n) < 1000) return n.toString();
  const abs = Math.abs(n);
  if (abs < 1_000_000) return (n / 1_000).toFixed(1) + "K";
  if (abs < 1_000_000_000) return (n / 1_000_000).toFixed(2) + "M";
  return (n / 1_000_000_000).toFixed(2) + "B";
}

export default function AgentUsagePage() {
  const agentId = useAgentIdFromURL();
  const [range, setRange] = useState<TokenUsageRange>("7d");
  const [data, setData] = useState<AgentTokenUsage | null>(null);
  const [sessions, setSessions] = useState<ChatSessionEntry[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  // 一次性拉取会话元数据，以便表格能显示标题而非不透明的
  // session_key。getChatSessions 返回当前用户视角下该智能体
  // 的对话列表；密钥未出现在其中的行（例如公开智能体上由
  // 定时任务为其他用户触发的会话）会回退到截断后的密钥。
  useEffect(() => {
    if (!agentId) return;
    let aborted = false;
    (async () => {
      try {
        const list = await getChatSessions(agentId);
        if (!aborted) setSessions(list);
      } catch {
        // 非致命错误——表格仅显示原始密钥。
      }
    })();
    return () => {
      aborted = true;
    };
  }, [agentId]);

  const sessionTitles = useMemo(() => {
    const m: Record<string, string> = {};
    for (const s of sessions) {
      m[s.id] = s.title || s.preview || s.id;
    }
    return m;
  }, [sessions]);

  async function load(r: TokenUsageRange) {
    if (!agentId) return;
    setLoading(true);
    setError("");
    try {
      const d = await getAgentTokenUsage(agentId, r, 50);
      setData(d);
    } catch (e) {
      setError(e instanceof Error ? e.message : "加载用量失败");
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    load(range);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId, range]);

  function renderSessionLabel(key: string): string {
    if (!key) return "（未跟踪）";
    const t = sessionTitles[key];
    if (t) return t;
    // 密钥是不透明的哈希值——截断以保持行可读。
    return key.length > 14 ? key.slice(0, 14) + "…" : key;
  }

  const rows = data?.sessions ?? [];

  return (
    <div className="p-6 space-y-6 max-w-3xl">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-xl font-semibold tracking-tight">令牌用量</h2>
          <p className="text-sm text-muted-foreground mt-1">
            此智能体按对话会话统计的令牌消耗。
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Tabs value={range} onValueChange={(v) => setRange(v as TokenUsageRange)}>
            <TabsList>
              {RANGES.map((r) => (
                <TabsTrigger key={r.value} value={r.value}>
                  {r.label}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
          <Button variant="outline" size="sm" onClick={() => load(range)} disabled={loading}>
            <RefreshCcw className={`h-4 w-4 ${loading ? "animate-spin" : ""}`} />
          </Button>
        </div>
      </div>

      {error && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent>
            <p className="text-sm text-destructive">{error}</p>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardContent>
          {rows.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-10 text-center">
              <Coins className="h-8 w-8 text-muted-foreground mb-3" />
              <p className="text-sm text-muted-foreground">
                当前时间范围内暂无令牌用量记录。
              </p>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>会话</TableHead>
                  <TableHead className="text-right">输入</TableHead>
                  <TableHead className="text-right">输出</TableHead>
                  <TableHead className="text-right">缓存</TableHead>
                  <TableHead className="text-right">总计</TableHead>
                  <TableHead className="text-right">请求数</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((r) => {
                  // 缓存 = 总计 - 输入 - 输出；API 将 cache_read + cache_creation
                  // 合并到 `tokens` 中，但暂未在传输中分别列出。显示单个
                  // "缓存"列使行内数字可加（输入 + 输出 + 缓存 = 总计），
                  // 同时不忽略提示缓存命中的存在。
                  const cache = Math.max(0, r.tokens - r.inputTokens - r.outputTokens);
                  return (
                    <TableRow key={r.key || "untracked"}>
                      <TableCell className="font-medium max-w-[260px] truncate" title={r.key}>
                        {renderSessionLabel(r.key)}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">{fmt(r.inputTokens)}</TableCell>
                      <TableCell className="text-right tabular-nums">{fmt(r.outputTokens)}</TableCell>
                      <TableCell className="text-right tabular-nums text-muted-foreground">{fmt(cache)}</TableCell>
                      <TableCell className="text-right tabular-nums font-medium">{fmt(r.tokens)}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.requestCount}</TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
