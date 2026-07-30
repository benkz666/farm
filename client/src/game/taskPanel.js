/**
 * 先展示现有任务，再刷新并仅在面板仍打开时重渲染权威状态。
 */
export function openTasksPanel({ render, refresh, isPanelOpen }) {
  render()
  void refresh().then((ok) => {
    if (ok && isPanelOpen('tasks')) render()
  })
}
