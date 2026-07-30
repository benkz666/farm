import test from 'node:test'
import assert from 'node:assert/strict'

import { openTasksPanel } from './taskPanel.js'

function deferred() {
  let resolve
  const promise = new Promise((res) => {
    resolve = res
  })
  return { promise, resolve }
}

async function settle(promise) {
  await promise
  await new Promise((resolve) => setImmediate(resolve))
}

test('打开任务面板立即渲染，刷新成功后总共只重绘一次', async () => {
  const refresh = deferred()
  const renders = []

  openTasksPanel({
    render: () => renders.push('render'),
    refresh: () => refresh.promise,
    isPanelOpen: (kind) => kind === 'tasks',
  })
  assert.deepEqual(renders, ['render'])

  refresh.resolve(true)
  await settle(refresh.promise)
  assert.deepEqual(renders, ['render', 'render'])
})

test('请求期间切换到其他弹窗时，TaskList 成功不覆盖当前弹窗', async () => {
  const refresh = deferred()
  let activePanel = 'tasks'
  const renders = []

  openTasksPanel({
    render: () => renders.push('render'),
    refresh: () => refresh.promise,
    isPanelOpen: (kind) => activePanel === kind,
  })
  activePanel = 'mail'
  refresh.resolve(true)
  await settle(refresh.promise)

  assert.deepEqual(renders, ['render'])
})

test('关闭任务弹窗后，晚到成功刷新不重新打开它', async () => {
  const refresh = deferred()
  let activePanel = 'tasks'
  const renders = []

  openTasksPanel({
    render: () => renders.push('render'),
    refresh: () => refresh.promise,
    isPanelOpen: (kind) => activePanel === kind,
  })
  activePanel = null
  refresh.resolve(true)
  await settle(refresh.promise)

  assert.deepEqual(renders, ['render'])
})

test('刷新失败时保留首次渲染内容', async () => {
  const renders = []
  openTasksPanel({
    render: () => renders.push('render'),
    refresh: async () => false,
    isPanelOpen: (kind) => kind === 'tasks',
  })
  await new Promise((resolve) => setImmediate(resolve))
  assert.deepEqual(renders, ['render'])
})
