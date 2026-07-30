// ============================================================
// 页面卸载生命周期：tick / pointermove / scene / 网络
// ============================================================

/**
 * 绑定 pagehide：幂等清理 interval、全局 pointermove、scene.dispose，
 * 并保留 reconnectBinding.dispose + netClient.close。
 *
 * @param {object} deps
 * @param {(type: string, fn: EventListener) => void} deps.addEventListener
 * @param {(type: string, fn: EventListener) => void} deps.removeEventListener
 * @param {(id: ReturnType<typeof setInterval>) => void} deps.clearInterval
 * @param {ReturnType<typeof setInterval>|null|undefined} deps.tickIntervalId
 * @param {EventListener|null|undefined} deps.onPointerMove
 * @param {() => { dispose?: () => void }|null|undefined} deps.getReconnectBinding
 * @param {(v: null) => void} deps.setReconnectBinding
 * @param {{ dispose?: () => void }|null|undefined} deps.scene
 * @param {() => { close?: () => void }|null|undefined} deps.getNetClient
 * @param {() => void} [deps.onCleanup] 额外清理（如任务跨日 timer）
 * @returns {() => void} 解除 pagehide 监听（一般无需调用）
 */
export function bindPageUnload(deps) {
  let done = false;
  const onPageHide = () => {
    if (done) return;
    done = true;
    if (deps.tickIntervalId != null) deps.clearInterval(deps.tickIntervalId);
    if (deps.onPointerMove) deps.removeEventListener('pointermove', deps.onPointerMove);
    const binding = deps.getReconnectBinding?.();
    binding?.dispose?.();
    deps.setReconnectBinding?.(null);
    deps.scene?.dispose?.();
    deps.getNetClient?.()?.close?.();
    deps.onCleanup?.();
  };
  deps.addEventListener('pagehide', onPageHide);
  return () => deps.removeEventListener('pagehide', onPageHide);
}
