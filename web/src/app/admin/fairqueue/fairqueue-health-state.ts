import type { FairQueueHealth } from "@/lib/api";

export interface FairQueueTrendSample {
  at: number;
  activeTenants: number;
  ringMembers: number;
  globalInflight: number;
  stable: number;
  rabbitReady: number;
  rabbitDLQ: number;
}

export function trendSample(health: FairQueueHealth, at = Date.now()): FairQueueTrendSample {
  return {
    at,
    activeTenants: health.redis.activeCount,
    ringMembers: health.redis.ringMemberCount,
    globalInflight: health.redis.globalInflight,
    stable: health.redis.stableCount,
    rabbitReady: health.rabbit.readyDepthSample,
    rabbitDLQ: health.rabbit.dlqDepthSample,
  };
}

export function appendTrendSample(
  samples: FairQueueTrendSample[],
  sample: FairQueueTrendSample,
  maximum = 60,
): FairQueueTrendSample[] {
  if (maximum <= 0) return [];
  return [...samples, sample].slice(-maximum);
}

export function trendMaximum(samples: FairQueueTrendSample[]): number {
  return Math.max(
    1,
    ...samples.flatMap((sample) => [
      sample.globalInflight,
      sample.stable,
      sample.rabbitReady,
    ]),
  );
}

export function statusLabel(status: string): string {
  switch (status) {
    case "healthy":
    case "ok":
    case "ready":
    case "complete":
    case "verified":
    case "running":
      return "正常";
    case "degraded":
    case "recovering":
    case "pending":
    case "paused":
    case "unknown":
      return "降级";
    case "failed":
    case "unavailable":
    case "mismatch":
      return "异常";
    default:
      return status || "未知";
  }
}
