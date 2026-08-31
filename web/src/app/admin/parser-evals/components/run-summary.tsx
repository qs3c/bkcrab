import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { ParserEvalParserAggregate, ParserEvalQualityScore, ParserEvalSummary } from "@/lib/api";
import { formatParserEvalDuration, formatParserEvalScore } from "../parser-eval-state";

export function RunSummary({ summary }: { summary: ParserEvalSummary }) {
  const markitdown = summary.markitdown;
  const anydoc = summary.anydoc;
  const wins = summary.wins;
  const quality = summary.macroQuality;
  return <div className="space-y-4">
    <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
      <ParserSummaryCard title="MarkItDown" aggregate={markitdown} />
      <ParserSummaryCard title="AnyDoc" aggregate={anydoc} />
      <Card><CardHeader><CardDescription>宏平均质量总分</CardDescription><CardTitle>{formatParserEvalScore(quality?.markitdown.total)} / {formatParserEvalScore(quality?.anydoc.total)}</CardTitle></CardHeader><CardContent className="text-xs text-muted-foreground">MarkItDown / AnyDoc · 每份文档双位置盲评</CardContent></Card>
      <Card><CardHeader><CardDescription>胜负</CardDescription><CardTitle>{wins ? `${wins.markitdown} / ${wins.anydoc} / ${wins.ties}` : "—"}</CardTitle></CardHeader><CardContent className="text-xs text-muted-foreground">MarkItDown / AnyDoc / 平局{wins?.failures ? ` · ${wins.failures} 个评分失败` : ""}</CardContent></Card>
    </div>
    {quality && <Card><CardHeader><CardTitle>质量维度</CardTitle><CardDescription>0–100 分；仅汇总双位置裁判均成功的文档。</CardDescription></CardHeader><CardContent><QualityRows markitdown={quality.markitdown} anydoc={quality.anydoc} /></CardContent></Card>}
    {(summary.formats?.length ?? 0) > 0 && <Card><CardHeader><CardTitle>按格式汇总</CardTitle><CardDescription>避免文档格式占比掩盖解析器在 DOCX、PPTX 或 XLSX 上的差异。</CardDescription></CardHeader><CardContent><Table><TableHeader><TableRow><TableHead>格式</TableHead><TableHead>文档</TableHead><TableHead>MarkItDown 成功率</TableHead><TableHead>AnyDoc 成功率</TableHead><TableHead>MarkItDown 总分</TableHead><TableHead>AnyDoc 总分</TableHead></TableRow></TableHeader><TableBody>{summary.formats?.map((row) => <TableRow key={row.format}><TableCell><Badge variant="outline">{row.format.toUpperCase()}</Badge></TableCell><TableCell>{row.documents}</TableCell><TableCell>{percent(row.markitdown.successRate)}</TableCell><TableCell>{percent(row.anydoc.successRate)}</TableCell><TableCell>{formatParserEvalScore(row.quality?.markitdown.total)}</TableCell><TableCell>{formatParserEvalScore(row.quality?.anydoc.total)}</TableCell></TableRow>)}</TableBody></Table></CardContent></Card>}
    {summary.usage && <p className="text-right text-xs text-muted-foreground">裁判用量：{(summary.usage.inputTokens + summary.usage.outputTokens).toLocaleString()} tokens{typeof summary.estimatedCostUsd === "number" ? ` · 估算 $${summary.estimatedCostUsd.toFixed(4)}` : " · 模型价格未知"}</p>}
  </div>;
}

function ParserSummaryCard({ title, aggregate }: { title: string; aggregate?: ParserEvalParserAggregate }) {
  return <Card><CardHeader><CardDescription>{title}</CardDescription><CardTitle>{formatParserEvalDuration(aggregate?.parse.medianMs)}</CardTitle></CardHeader><CardContent className="space-y-1 text-xs text-muted-foreground"><p>纯解析 median · P95 {formatParserEvalDuration(aggregate?.parse.p95Ms)}</p><p>端到端 median {formatParserEvalDuration(aggregate?.endToEnd.medianMs)} · P95 {formatParserEvalDuration(aggregate?.endToEnd.p95Ms)}</p><p>成功率 {percent(aggregate?.successRate)}</p></CardContent></Card>;
}

function QualityRows({ markitdown, anydoc }: { markitdown: ParserEvalQualityScore; anydoc: ParserEvalQualityScore }) {
  const rows: Array<[string, keyof ParserEvalQualityScore]> = [["完整性", "completeness"], ["结构", "structure"], ["格式", "formatting"], ["洁净度", "cleanliness"], ["总分", "total"]];
  return <Table><TableHeader><TableRow><TableHead>维度</TableHead><TableHead>MarkItDown</TableHead><TableHead>AnyDoc</TableHead><TableHead>差值（AnyDoc - MarkItDown）</TableHead></TableRow></TableHeader><TableBody>{rows.map(([label, key]) => <TableRow key={key}><TableCell>{label}</TableCell><TableCell>{formatParserEvalScore(markitdown[key])}</TableCell><TableCell>{formatParserEvalScore(anydoc[key])}</TableCell><TableCell>{(anydoc[key] - markitdown[key]).toFixed(1)}</TableCell></TableRow>)}</TableBody></Table>;
}

function percent(value?: number): string {
  return typeof value === "number" && Number.isFinite(value) ? `${(value * 100).toFixed(1)}%` : "—";
}
