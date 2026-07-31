import test from 'node:test'
import assert from 'node:assert/strict'

import { plotCmdForTool, isVisitTool } from './onlineActions.js'
import { PLOT } from './state.js'
import { CMD_WATER, CMD_REMOVE_WEED, CMD_REMOVE_PEST, CMD_STEAL, CMD_HARVEST } from '../net/client.js'

test('拜访工具仅开放浇水/除草/除虫/偷菜', () => {
  assert.equal(isVisitTool('water'), true)
  assert.equal(isVisitTool('weed'), true)
  assert.equal(isVisitTool('pest'), true)
  assert.equal(isVisitTool('steal'), true)
  assert.equal(isVisitTool('plant'), false)
  assert.equal(isVisitTool('harvest'), false)
})

test('偷菜只在成熟地块映射 Steal 命令', () => {
  assert.equal(plotCmdForTool('steal', PLOT.MATURE), CMD_STEAL)
  assert.equal(plotCmdForTool('steal', PLOT.GROWING), null)
  assert.equal(plotCmdForTool('water', PLOT.GROWING), CMD_WATER)
  assert.equal(plotCmdForTool('weed', PLOT.GROWING), CMD_REMOVE_WEED)
  assert.equal(plotCmdForTool('pest', PLOT.GROWING), CMD_REMOVE_PEST)
  assert.equal(plotCmdForTool('harvest', PLOT.MATURE), CMD_HARVEST)
  assert.equal(plotCmdForTool('water', PLOT.MATURE), null)
  assert.equal(plotCmdForTool('weed', PLOT.MATURE), null)
  assert.equal(plotCmdForTool('pest', PLOT.MATURE), null)
})
