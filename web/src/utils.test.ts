import { describe, expect, it } from "vitest";

import {
  chartPolyline,
  classifyPerformanceSample,
  formatBytes,
  formatDuration,
  percentile,
  recordPhaseObservation
} from "./utils";

describe("dashboard utilities", () => {
  it("formats infrastructure values without overstating precision", () => {
    expect(formatBytes(5_872_025_600)).toBe("5.5 GiB");
    expect(formatDuration(850)).toBe("850 ms");
    expect(formatDuration(12_400)).toBe("12 s");
  });

  it("computes nearest-rank percentiles", () => {
    expect(percentile([10, 40, 20, 30], 50)).toBe(20);
    expect(percentile([10, 40, 20, 30], 95)).toBe(40);
  });

  it("deduplicates identical phase observations", () => {
    const first = recordPhaseObservation([], "Progressing", "WarmingEngine", "2026-01-01T00:00:00Z");
    const duplicate = recordPhaseObservation(first, "Progressing", "WarmingEngine", "2026-01-01T00:00:05Z");
    const ready = recordPhaseObservation(duplicate, "Ready", "EngineServing", "2026-01-01T00:00:10Z");
    expect(duplicate).toHaveLength(1);
    expect(ready).toHaveLength(2);
  });

  it("builds bounded chart coordinates", () => {
    const points = chartPolyline(
      [
        { timestamp: "2026-01-01T00:00:00Z", queueDepth: 0, runningRequests: 0, replicas: 1 },
        { timestamp: "2026-01-01T00:00:05Z", queueDepth: 4, runningRequests: 1, replicas: 2 }
      ],
      "queueDepth",
      100,
      40
    );
    expect(points).toBe("0.0,40.0 100.0,0.0");
    expect(classifyPerformanceSample(true)).toBe("activation");
  });
});
