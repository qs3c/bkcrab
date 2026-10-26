export function chatFailureMessage(error: unknown): string {
  const raw = error instanceof Error ? error.message : String(error || "");
  if (/invalid credential|unauthorized|api.?key|\b40[13]\b|no usable|usable LLM provider/i.test(raw)) {
    return "模型服务暂时不可用，请在设置中检查模型与 API 密钥，或联系管理员后重试。";
  }
  if (/429|rate.?limit|quota|insufficient|balance/i.test(raw)) {
    return "模型服务额度不足或请求过于频繁，请稍后重试，或联系管理员检查额度。";
  }
  if (/timeout|timed out|deadline/i.test(raw)) return "模型回复超时，请稍后重试。";
  if (/abort|cancel|stopped/i.test(raw)) return "本次回复已停止，可以继续发送消息。";
  return "本次回复未完成，请检查网络或稍后重试。你的消息已保留。";
}

// Clipboard API requires HTTPS. The existing HTTP deployment also needs a
// selection-based fallback; a success indicator must only follow a real copy.
export async function copyText(text: string): Promise<void> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return;
    }
  } catch { /* Try the HTTP/permission-denied fallback. */ }
  const focused = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const selection = document.getSelection();
  const ranges = selection ? Array.from({ length: selection.rangeCount }, (_, i) => selection.getRangeAt(i).cloneRange()) : [];
  const field = document.createElement("textarea");
  field.value = text;
  field.setAttribute("readonly", "");
  field.style.cssText = "position:fixed;left:0;top:0;opacity:0;pointer-events:none";
  // Stay inside the current focus trap when copying from a modal.
  (focused?.closest('[role="dialog"]') || document.body).appendChild(field);
  try {
    field.select();
    if (!document.execCommand("copy")) throw new Error("copy denied");
  } catch {
    throw new Error("复制失败，请手动选择内容后复制。");
  } finally {
    field.remove();
    focused?.focus({ preventScroll: true });
    if (selection) {
      selection.removeAllRanges();
      ranges.forEach((range) => selection.addRange(range));
    }
  }
}

export function requireSaved(result: unknown): void {
  if (!result || typeof result !== "object") throw new Error("服务器未确认保存，请重试。");
  const response = result as { ok?: boolean; error?: unknown };
  if (response.ok !== true || response.error) {
    throw new Error(typeof response.error === "string" ? response.error : "保存失败，请重试。");
  }
}
