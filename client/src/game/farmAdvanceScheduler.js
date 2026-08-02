import { RISK_WINDOW } from './config.js';
import { PLOT } from './state.js';

const MAX_TIMER_DELAY = 2_147_000_000;
const MIN_BOUNDARY_DELAY = 40;
const RETRY_DELAY = 2_000;
const RISK_WINDOWS = Math.round(1 / RISK_WINDOW);

/**
 * 返回当前农场下一次需要服务端裁决的时刻。
 * 普通作物只需要在生长风险窗口或成熟点同步；
 * 成熟后永久保持可收获状态，不再安排时间边界。
 */
export function nextFarmSyncAt(plots, now) {
  let next = Number.POSITIVE_INFINITY;
  for (const plot of Array.isArray(plots) ? plots : []) {
    if (!plot) continue;
    if (plot.state === PLOT.GROWING) {
      const matureAt = Number(plot.matureTime) || 0;
      const seasonStart = Number(plot.plantTime) || 0;
      const duration = Number(plot.seasonMs) || 0;
      if (matureAt <= 0 || duration <= 0) continue;
      if (matureAt <= now) return now;
      next = Math.min(next, matureAt);

      // 在每个 10% 风险窗口结束时让服务端扫描刚结束的窗口。
      if (seasonStart > 0) {
        for (let k = 1; k <= RISK_WINDOWS; k++) {
          const boundary = seasonStart + Math.floor(duration * k / RISK_WINDOWS);
          if (boundary > now && boundary < matureAt) {
            next = Math.min(next, boundary);
            break;
          }
        }
      }
    }
  }
  return Number.isFinite(next) ? next : 0;
}

/**
 * 单 timer 调度器。到边界只请求一次权威 SyncFarm，不在客户端自行改状态。
 */
export function createFarmAdvanceScheduler({
  setTimeout,
  clearTimeout,
  now = () => Date.now(),
  getPlots,
  isActive,
  sync,
  getConsistencyIntervalMs = () => 0,
  onError = () => {},
}) {
  let timerId = null;
  let running = false;
  let disposed = false;

  function clear() {
    if (timerId == null) return;
    clearTimeout(timerId);
    timerId = null;
  }

  function schedule(delayOverride = null) {
    clear();
    if (disposed || !isActive()) return;
    const current = now();
    const boundaryTarget = nextFarmSyncAt(getPlots(), current);
    const consistencyInterval = Number(getConsistencyIntervalMs()) || 0;
    const consistencyTarget = consistencyInterval > 0
      ? current + consistencyInterval
      : 0;
    const target = boundaryTarget && consistencyTarget
      ? Math.min(boundaryTarget, consistencyTarget)
      : (boundaryTarget || consistencyTarget);
    if (!target) return;
    let delay = delayOverride == null ? target - current : delayOverride;
    if (delay <= 0) delay = MIN_BOUNDARY_DELAY;
    delay = Math.min(MAX_TIMER_DELAY, delay);
    timerId = setTimeout(run, delay);
  }

  async function run() {
    timerId = null;
    if (disposed || running || !isActive()) return;
    running = true;
    let failed = false;
    try {
      await sync();
    } catch (error) {
      failed = true;
      onError(error);
    } finally {
      running = false;
      if (!disposed) schedule(failed ? RETRY_DELAY : null);
    }
  }

  return {
    schedule,
    reconcileNow() {
      clear();
      return run();
    },
    dispose() {
      disposed = true;
      clear();
    },
    get disposed() {
      return disposed;
    },
  };
}
