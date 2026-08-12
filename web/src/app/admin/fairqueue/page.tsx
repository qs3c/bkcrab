"use client";

import * as React from "react";
import {
  Activity,
  AlarmClock,
  CircleDot,
  Database,
  Gauge,
  Network,
  RefreshCcw,
  RotateCcw,
  ServerCog,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Switch } from "@/components/ui/switch";
import {
  adminGetFairQueueHealth,
  type FairQueueHealth,
  type FairQueueLoopHealth,
} from "@/lib/api";
import {
  appendTrendSample,
  statusLabel,
  trendMaximum,
  trendSample,
  type FairQueueTrendSample,
} from "./fairqueue-health-state";

const POLL_INTERVAL_MS = 2_000;

function badgeVariant(status: string): "default" | "secondary" | "destructive" | "outline" {
  if (["healthy", "ok", "ready", "complete", "verified", "running"].includes(status)) {
    return "default";
  }
  if (["failed", "unavailable", "mismatch"].includes(status)) return "destructive";
  return "secondary";
}

function formatTime(value?: string): string {
  if (!value) return "尚无样本";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString("zh-CN", { hour12: false });
}

function StateBadge({ value }: { value: string }) {
  return <Badge variant={badgeVariant(value)}>{statusLabel(value)}</Badge>;
}

export default function AdminFairQueuePage() {
  const [health, setHealth] = React.useState<FairQueueHealth | null>(null);
  const [samples, setSamples] = React.useState<FairQueueTrendSample[]>([]);
  const [autoRefresh, setAutoRefresh] = React.useState(true);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");
  const [updatedAt, setUpdatedAt] = React.useState<number | null>(null);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const response = await adminGetFairQueueHealth();
      const now = Date.now();
      setHealth(response.fairQueue);
      setSamples((current) => appendTrendSample(current, trendSample(response.fairQueue, now)));
      setUpdatedAt(now);
      setError("");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "公平队列状态加载失败");
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  React.useEffect(() => {
    if (!autoRefresh) return;
    const timer = window.setInterval(() => void load(), POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [autoRefresh, load]);

  const maximum = trendMaximum(samples);

  return (
    <div className="mx-auto max-w-6xl space-y-6 p-4 md:p-6">
      <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <div className="flex items-center gap-2">
            <h1 className="text-2xl font-semibold tracking-tight">公平队列</h1>
            {health && <StateBadge value={health.status} />}
          </div>
          <p className="mt-1 text-sm text-muted-foreground">
            RAG 多租户调度、Redis 协调状态与 RabbitMQ 积压的实时快照。
          </p>
        </div>
        <div className="flex items-center gap-3">
          <label className="flex items-center gap-2 text-sm text-muted-foreground">
            <Switch checked={autoRefresh} onCheckedChange={setAutoRefresh} />
            每 2 秒刷新
          </label>
          <Button variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
            <RefreshCcw className={`mr-2 size-4 ${loading ? "animate-spin" : ""}`} />
            刷新
          </Button>
        </div>
      </div>

      {error && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="py-4 text-sm text-destructive">{error}</CardContent>
        </Card>
      )}

      {!health ? (
        <Card><CardContent className="py-12 text-center text-sm text-muted-foreground">正在读取运行状态…</CardContent></Card>
      ) : (
        <>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <MetricCard icon={Network} label="活跃租户" value={health.redis.activeCount} hint={`环成员 ${health.redis.ringMemberCount}`} />
            <MetricCard icon={Gauge} label="全局执行中" value={health.redis.globalInflight} hint={`稳定执行 ${health.redis.stableCount} · 临时预约 ${health.redis.provisionalCount}`} />
            <MetricCard icon={Activity} label="Rabbit 待处理" value={health.rabbit.readyDepthSample} hint={`DLQ ${health.rabbit.dlqDepthSample}`} danger={health.rabbit.dlqDepthSample > 0} />
            <MetricCard icon={CircleDot} label="调度门" value={health.gateOpen ? "已打开" : "已暂停"} hint={`${health.mode} 模式`} danger={!health.gateOpen && health.mode === "fair"} />
          </div>

          <Card>
            <CardHeader className="flex flex-row items-center justify-between">
              <div>
                <CardTitle className="text-base">最近两分钟负载</CardTitle>
                <p className="mt-1 text-xs text-muted-foreground">浏览器内保留最近 60 个快照，刷新页面后清空。</p>
              </div>
              <div className="flex flex-wrap gap-3 text-xs text-muted-foreground">
                <Legend color="bg-blue-500" label="全局执行中" />
                <Legend color="bg-emerald-500" label="稳定执行" />
                <Legend color="bg-amber-500" label="Rabbit ready" />
              </div>
            </CardHeader>
            <CardContent>
              <div className="flex h-40 items-end gap-1 rounded-md border bg-muted/20 p-3">
                {samples.map((sample) => (
                  <div key={sample.at} className="flex h-full min-w-0 flex-1 items-end justify-center gap-px" title={`${new Date(sample.at).toLocaleTimeString("zh-CN", { hour12: false })} · 执行中 ${sample.globalInflight} · 稳定执行 ${sample.stable} · ready ${sample.rabbitReady}`}>
                    <TrendBar value={sample.globalInflight} maximum={maximum} color="bg-blue-500" />
                    <TrendBar value={sample.stable} maximum={maximum} color="bg-emerald-500" />
                    <TrendBar value={sample.rabbitReady} maximum={maximum} color="bg-amber-500" />
                  </div>
                ))}
                {samples.length === 0 && <div className="m-auto text-sm text-muted-foreground">等待采样</div>}
              </div>
            </CardContent>
          </Card>

          <div className="grid gap-4 lg:grid-cols-3">
            <DependencyCard title="Redis" icon={Database} status={health.redis.status} rows={[
              ["资源状态", health.redis.resourceState || "未知"],
              ["模式", health.redis.mode || "未知"],
              ["公平环", `${health.redis.ringCount} turns / ${health.redis.ringMemberCount} users`],
              ["预留令牌", `${health.redis.provisionalCount} 临时 / ${health.redis.stableCount} 稳定`],
              ["瞬时调度轮次", `${health.redis.processingCount}（通常仅持续毫秒）`],
              ["需人工介入", health.redis.operatorRequired ? "是" : "否"],
            ]} />
            <DependencyCard title="RabbitMQ" icon={Network} status={health.rabbit.status} rows={[
              ["Ready", String(health.rabbit.readyDepthSample)],
              ["DLQ", String(health.rabbit.dlqDepthSample)],
              ["最近确认", formatTime(health.rabbit.lastConfirmAt)],
              ["最近退回", formatTime(health.rabbit.lastReturnAt)],
            ]} />
            <DependencyCard title="MySQL 权威状态" icon={ServerCog} status={health.mysql.status} rows={[
              ["Schema", health.mysql.schemaReady ? "已就绪" : "未就绪"],
              ["拓扑", health.mysql.writerTopology || "未知"],
              ["会话亲和性", health.mysql.sessionAffinity || "未知"],
              ["控制指纹", health.mysql.controlFingerprintMatch ? "匹配" : "不匹配"],
              ["操作日志", `${health.mysql.operationJournal.kind} / ${health.mysql.operationJournal.phase}`],
            ]} />
          </div>

          <div className="grid gap-4 lg:grid-cols-2">
            <Card>
              <CardHeader><CardTitle className="flex items-center gap-2 text-base"><AlarmClock className="size-4" />后台循环</CardTitle></CardHeader>
              <CardContent className="space-y-3">
                <LoopRow name="Scheduler" loop={health.loops.scheduler} state={health.loops.scheduler.state} />
                <LoopRow name="Dispatcher" loop={health.loops.dispatcher} />
                <LoopRow name="Sweeper" loop={health.loops.sweeper} />
                <LoopRow name="Reconciler" loop={health.loops.reconciler} />
              </CardContent>
            </Card>
            <Card>
              <CardHeader><CardTitle className="flex items-center gap-2 text-base"><RotateCcw className="size-4" />恢复与安全状态</CardTitle></CardHeader>
              <CardContent className="grid grid-cols-2 gap-4 text-sm">
                <Fact label="启动恢复" value={health.recovery.startup} />
                <Fact label="已扫描页数" value={String(health.recovery.pagesCompleted)} />
                <Fact label="已收敛" value={health.recovery.converged ? "是" : "否"} />
                <Fact label="操作遍历完成" value={health.recovery.operationPassComplete ? "是" : "否"} />
                <Fact label="致命状态" value={health.fatal ? "是" : "否"} />
                <Fact label="正在关闭" value={health.shuttingDown ? "是" : "否"} />
              </CardContent>
            </Card>
          </div>
        </>
      )}

      <p className="text-xs text-muted-foreground">
        最后更新：{updatedAt ? new Date(updatedAt).toLocaleString("zh-CN", { hour12: false }) : "尚未更新"}。该页面只显示内存中的安全聚合快照，不读取原始租户 ID、凭据或任务正文。
      </p>
    </div>
  );
}

function MetricCard({ icon: Icon, label, value, hint, danger = false }: { icon: React.ComponentType<{ className?: string }>; label: string; value: string | number; hint: string; danger?: boolean }) {
  return (
    <Card className={danger ? "border-destructive/50" : undefined}>
      <CardContent className="py-5">
        <div className="flex items-center justify-between text-sm text-muted-foreground"><span>{label}</span><Icon className="size-4" /></div>
        <p className={`mt-2 text-2xl font-semibold tabular-nums ${danger ? "text-destructive" : ""}`}>{value}</p>
        <p className="mt-1 text-xs text-muted-foreground">{hint}</p>
      </CardContent>
    </Card>
  );
}

function TrendBar({ value, maximum, color }: { value: number; maximum: number; color: string }) {
  const height = value <= 0 ? 2 : Math.max(5, (value / maximum) * 100);
  return <div className={`w-1.5 rounded-t-sm ${color} opacity-80`} style={{ height: `${height}%` }} />;
}

function Legend({ color, label }: { color: string; label: string }) {
  return <span className="flex items-center gap-1.5"><span className={`size-2 rounded-full ${color}`} />{label}</span>;
}

function DependencyCard({ title, icon: Icon, status, rows }: { title: string; icon: React.ComponentType<{ className?: string }>; status: string; rows: [string, string][] }) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between">
        <CardTitle className="flex items-center gap-2 text-base"><Icon className="size-4" />{title}</CardTitle>
        <StateBadge value={status} />
      </CardHeader>
      <CardContent className="space-y-2 text-sm">
        {rows.map(([label, value]) => <div key={label} className="flex items-start justify-between gap-4"><span className="text-muted-foreground">{label}</span><span className="break-all text-right font-medium">{value}</span></div>)}
      </CardContent>
    </Card>
  );
}

function LoopRow({ name, loop, state }: { name: string; loop: FairQueueLoopHealth; state?: string }) {
  return <div className="flex items-center justify-between gap-4 rounded-md border px-3 py-2"><div><p className="text-sm font-medium">{name}</p><p className="text-xs text-muted-foreground">{formatTime(loop.lastSuccessAt)}</p></div><div className="text-right"><p className="text-sm tabular-nums">延迟 {loop.lagSeconds}s</p>{state && <p className="text-xs text-muted-foreground">{state}</p>}</div></div>;
}

function Fact({ label, value }: { label: string; value: string }) {
  return <div className="rounded-md border p-3"><p className="text-xs text-muted-foreground">{label}</p><p className="mt-1 font-medium">{value}</p></div>;
}
