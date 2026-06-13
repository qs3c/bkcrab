"use client";

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Badge } from "@/components/ui/badge";
import { Switch } from "@/components/ui/switch";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { Clock, Plus, Trash2 } from "lucide-react";
import {
  getCronJobs,
  createCronJob,
  updateCronJob,
  deleteCronJob,
  getAgents,
  type CronJobInfo,
  type AgentDetail,
} from "@/lib/api";

function cronTypeLabel(type: string) {
  return ({ cron: "Cron 表达式", interval: "固定间隔", once: "单次执行" } as Record<string, string>)[type] || type;
}

export default function CronPage() {
  const [jobs, setJobs] = useState<CronJobInfo[]>([]);
  const [agents, setAgents] = useState<AgentDetail[]>([]);
  const [loading, setLoading] = useState(true);
  const [createOpen, setCreateOpen] = useState(false);
  const [deleteId, setDeleteId] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const [newName, setNewName] = useState("");
  const [newSchedule, setNewSchedule] = useState("");
  const [newType, setNewType] = useState("cron");
  const [newAgentId, setNewAgentId] = useState("");
  const [newMessage, setNewMessage] = useState("");

  const fetchData = () => {
    setLoading(true);
    Promise.all([getCronJobs(), getAgents()])
      .then(([j, a]) => {
        setJobs(j);
        setAgents(a);
      })
      .catch(() => {
        setJobs([]);
        setAgents([]);
      })
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    fetchData();
  }, []);

  const handleCreate = async () => {
    if (!newName.trim() || !newSchedule.trim()) return;
    setSaving(true);
    await createCronJob({
      name: newName.trim(),
      type: newType,
      schedule: newSchedule.trim(),
      agentId: newAgentId,
      message: newMessage,
      enabled: true,
    });
    setCreateOpen(false);
    setNewName("");
    setNewSchedule("");
    setNewType("cron");
    setNewAgentId("");
    setNewMessage("");
    setSaving(false);
    fetchData();
  };

  const handleToggle = async (job: CronJobInfo) => {
    await updateCronJob(job.id, { enabled: !job.enabled });
    fetchData();
  };

  const handleDelete = async () => {
    if (!deleteId) return;
    await deleteCronJob(deleteId);
    setDeleteId(null);
    fetchData();
  };

  const typeColor = (type: string) => {
    const colors: Record<string, string> = {
      cron: "bg-violet-500/10 text-violet-600 dark:text-violet-400 border-violet-500/20",
      interval: "bg-blue-500/10 text-blue-600 dark:text-blue-400 border-blue-500/20",
      exact: "bg-amber-500/10 text-amber-600 dark:text-amber-400 border-amber-500/20",
    };
    return colors[type] || "bg-muted text-muted-foreground border-border";
  };

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Cron 任务</h2>
          <p className="text-sm text-muted-foreground mt-1">
            安排智能体自动任务
          </p>
        </div>
        <Button onClick={() => setCreateOpen(true)}>
          <Plus className="h-4 w-4 mr-2" />
          新建任务
        </Button>
      </div>

      <div className="rounded-lg border border-border bg-card">
        {loading ? (
          <div className="p-6 space-y-3">
            {[1, 2].map((i) => (
              <Skeleton key={i} className="h-14 w-full" />
            ))}
          </div>
        ) : jobs.length === 0 ? (
          <div className="flex flex-col items-center justify-center py-16 text-center">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-primary/10 mb-4">
              <Clock className="h-7 w-7 text-primary" />
            </div>
            <p className="text-sm text-muted-foreground">尚未配置 Cron 任务</p>
            <Button
              onClick={() => setCreateOpen(true)}
              variant="outline"
              className="mt-4"
            >
              创建你的第一个任务
            </Button>
          </div>
        ) : (
          <div className="overflow-x-auto -mx-6 px-6">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>名称</TableHead>
                <TableHead>计划</TableHead>
                <TableHead>类型</TableHead>
                <TableHead>智能体</TableHead>
                <TableHead>上次运行</TableHead>
                <TableHead>已启用</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {jobs.map((job) => (
                <TableRow key={job.id} className="hover:bg-muted/50 transition-colors">
                  <TableCell>
                    <span className="font-medium">{job.name}</span>
                  </TableCell>
                  <TableCell>
                    <code className="rounded bg-muted px-2 py-1 text-xs font-mono">
                      {job.schedule}
                    </code>
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline" className={typeColor(job.type)}>
                      {cronTypeLabel(job.type)}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <span className="text-sm text-muted-foreground">{job.agentId || "-"}</span>
                  </TableCell>
                  <TableCell>
                    <span className="text-xs text-muted-foreground">
                      {job.lastRun || "从未"}
                    </span>
                  </TableCell>
                  <TableCell>
                    <Switch
                      checked={job.enabled}
                      onCheckedChange={() => handleToggle(job)}
                    />
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-8 w-8 text-muted-foreground hover:text-destructive"
                      onClick={() => setDeleteId(job.id)}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          </div>
        )}
      </div>

      {/* 创建对话框 */}
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>创建 Cron 任务</DialogTitle>
            <DialogDescription>
              安排一个智能体自动任务
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <Label>任务名称</Label>
              <Input
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                placeholder="daily-report"
              />
            </div>
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div className="space-y-2">
                <Label>类型</Label>
                <Select value={newType} onValueChange={(v) => v && setNewType(v)}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="cron">Cron 表达式</SelectItem>
                    <SelectItem value="interval">间隔</SelectItem>
                    <SelectItem value="exact">指定时间</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label>计划</Label>
                <Input
                  value={newSchedule}
                  onChange={(e) => setNewSchedule(e.target.value)}
                  placeholder={newType === "cron" ? "*/5 * * * *" : newType === "interval" ? "5m" : "14:30"}
                  className="font-mono"
                />
              </div>
            </div>
            <div className="space-y-2">
              <Label>智能体</Label>
              <Select value={newAgentId} onValueChange={(v) => v && setNewAgentId(v)}>
                <SelectTrigger>
                  <SelectValue placeholder="选择智能体" />
                </SelectTrigger>
                <SelectContent>
                  {agents.map((a) => (
                    <SelectItem key={a.id} value={a.id}>
                      {a.id}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label>消息</Label>
              <Textarea
                value={newMessage}
                onChange={(e) => setNewMessage(e.target.value)}
                placeholder="生成每日状态报告..."
                rows={3}
                className="resize-none"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              取消
            </Button>
            <Button
              onClick={handleCreate}
              disabled={!newName.trim() || !newSchedule.trim() || saving}
            >
              {saving ? "正在创建..." : "创建任务"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 删除确认 */}
      <AlertDialog open={!!deleteId} onOpenChange={() => setDeleteId(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除 Cron 任务</AlertDialogTitle>
            <AlertDialogDescription>
              确定要删除此任务吗？此操作无法撤销。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
