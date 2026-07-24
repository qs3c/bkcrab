"use client";

import * as React from "react";
import Image from "next/image";
import { Download, FileImage, ImageOff, ZoomIn } from "lucide-react";

import {
  buildOwnerAttachmentURL,
  buildOwnerAssetURL,
  formatSourceLocation,
  getRAGResourceKey,
  markAssetUnavailable,
  type RAGAttachmentURLBuilder,
  type RAGAssetURLBuilder,
  type RAGGalleryResource,
} from "@/components/rag-resource-gallery-state";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { buttonVariants } from "@/components/ui/button";
import { RAGPlainText } from "@/components/rag-safe-render";
import { cn } from "@/lib/utils";

export interface RAGResourceGalleryProps {
  resources: readonly RAGGalleryResource[];
  actAs?: string;
  assetURLBuilder?: RAGAssetURLBuilder;
  attachmentURLBuilder?: RAGAttachmentURLBuilder;
  title?: string;
  compact?: boolean;
  showDisclosure?: boolean;
  className?: string;
}

export function RAGResourceGallery({
  resources,
  actAs = "",
  assetURLBuilder,
  attachmentURLBuilder,
  title = "相关图片（来自检索资料）",
  compact = false,
  showDisclosure = false,
  className,
}: RAGResourceGalleryProps) {
  const [selectedResourceKey, setSelectedResourceKey] = React.useState("");
  const [unavailableAssetIDs, setUnavailableAssetIDs] = React.useState<string[]>([]);
  const selected = resources.find(
    (resource) => getRAGResourceKey(resource.asset) === selectedResourceKey,
  );
  const assetURL = assetURLBuilder
    || ((assetID: string, variant: "display" | "thumbnail") => buildOwnerAssetURL(assetID, variant, actAs));
  const attachmentURL = attachmentURLBuilder
    || ((attachmentID: string) => buildOwnerAttachmentURL(attachmentID, actAs));
  const selectedAttachment = selected?.asset.attachment?.kind === "visio_source"
    ? selected.asset.attachment
    : undefined;
  const selectedAttachmentURL = selectedAttachment
    ? attachmentURL(selectedAttachment.id)
    : "";

  if (resources.length === 0) return null;

  const markUnavailable = (assetID: string) => {
    setUnavailableAssetIDs((current) => markAssetUnavailable(current, assetID));
  };

  return (
    <div className={cn("space-y-2.5", className)}>
      {title && (
        <div className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
          <FileImage className="size-3.5" aria-hidden="true" />
          <span>{title}</span>
        </div>
      )}
      <div className={cn(
        "grid gap-2",
        compact ? "grid-cols-2 sm:grid-cols-3" : "grid-cols-2 sm:grid-cols-3 lg:grid-cols-6",
      )}>
        {resources.map((resource) => {
          const resourceKey = getRAGResourceKey(resource.asset);
          const unavailable = unavailableAssetIDs.includes(resource.asset.id);
          const location = formatSourceLocation(
            resource.sourceLocation || resource.asset.location,
            resource.asset.pageNum,
          );
          const caption = resource.asset.caption.trim();
          return (
            <button
              key={resourceKey}
              type="button"
              className={cn(
                "group overflow-hidden rounded-lg border bg-muted/30 text-left transition-colors",
                unavailable ? "cursor-default" : "hover:border-primary/40 hover:bg-muted/60",
              )}
              onClick={() => !unavailable && setSelectedResourceKey(resourceKey)}
              aria-label={unavailable
                ? `${resource.docName || "检索资料"}的图片暂不可用`
                : `预览${resource.docName || "检索资料"}中的图片`}
            >
              <span className={cn(
                "relative flex w-full items-center justify-center overflow-hidden bg-muted",
                compact ? "aspect-[4/3]" : "aspect-square",
              )}>
                {unavailable ? (
                  <span className="flex flex-col items-center gap-1 px-2 text-center text-[11px] text-muted-foreground">
                    <ImageOff className="size-5" aria-hidden="true" />
                    图片暂不可用
                  </span>
                ) : (
                  <>
                    <Image
                      src={assetURL(resource.asset.id, "thumbnail")}
                      alt={caption || `${resource.docName || "文档"}中的相关图片`}
                      fill
                      unoptimized
                      sizes={compact ? "160px" : "240px"}
                      className="object-cover"
                      onError={() => markUnavailable(resource.asset.id)}
                    />
                    <span className="absolute inset-0 flex items-center justify-center bg-black/0 text-white opacity-0 transition-all group-hover:bg-black/25 group-hover:opacity-100">
                      <ZoomIn className="size-5 drop-shadow" aria-hidden="true" />
                    </span>
                  </>
                )}
              </span>
              <span className="block space-y-0.5 px-2 py-1.5">
                <span className="block truncate text-[11px] font-medium" title={resource.docName}>
                  {resource.docName || "检索资料"}
                </span>
                {(location || caption) && (
                  <RAGPlainText
                    value={caption || location}
                    className="block truncate text-[10px] text-muted-foreground"
                    title={caption || location}
                  />
                )}
              </span>
            </button>
          );
        })}
      </div>
      {showDisclosure && (
        <p className="text-[11px] leading-5 text-muted-foreground">
          图片由解析阶段转写，回答模型依据文字资料作答。
        </p>
      )}

      <Dialog
        open={!!selected}
        onOpenChange={(open) => {
          if (!open) setSelectedResourceKey("");
        }}
      >
        {selected && (
          <DialogContent className="max-w-4xl">
            <DialogHeader>
              <DialogTitle>{selected.docName || "相关图片"}</DialogTitle>
              <DialogDescription>
                {[
                  formatSourceLocation(
                    selected.sourceLocation || selected.asset.location,
                    selected.asset.pageNum,
                  ),
                  selected.sectionTitle,
                ].filter(Boolean).join(" · ") || "来自检索资料"}
              </DialogDescription>
            </DialogHeader>
            <div className="relative min-h-64 overflow-hidden rounded-lg border bg-muted sm:min-h-[28rem]">
              {unavailableAssetIDs.includes(selected.asset.id) ? (
                <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 text-sm text-muted-foreground">
                  <ImageOff className="size-8" aria-hidden="true" />
                  图片已删除或当前无权访问
                </div>
              ) : (
                <Image
                  src={assetURL(selected.asset.id, "display")}
                  alt={selected.asset.caption || `${selected.docName || "文档"}中的相关图片`}
                  fill
                  unoptimized
                  priority
                  sizes="(max-width: 768px) 92vw, 800px"
                  className="object-contain"
                  onError={() => markUnavailable(selected.asset.id)}
                />
              )}
            </div>
            {selected.asset.caption && (
              <RAGPlainText
                as="p"
                value={selected.asset.caption}
                className="whitespace-pre-wrap text-sm leading-6 text-muted-foreground"
              />
            )}
            {selectedAttachment && selectedAttachmentURL && (
              <div className="flex flex-wrap items-center justify-between gap-2">
                <span className="truncate text-xs text-muted-foreground">
                  {selectedAttachment.fileName || "Visio 工程文件 (.vsdx)"}
                </span>
                <a
                  href={selectedAttachmentURL}
                  target="_blank"
                  rel="noopener noreferrer"
                  download
                  className={buttonVariants({ variant: "outline" })}
                >
                  <Download aria-hidden="true" />
                  下载 Visio 工程文件
                </a>
              </div>
            )}
          </DialogContent>
        )}
      </Dialog>
    </div>
  );
}
