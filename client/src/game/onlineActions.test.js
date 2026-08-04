import test from 'node:test'
import assert from 'node:assert/strict'

import { plotCmdForTool, isVisitTool } from './onlineActions.js'
import { PLOT } from './state.js'
import { CMD_CLEAR, CMD_WATER, CMD_REMOVE_WEED, CMD_REMOVE_PEST, CMD_STEAL, CMD_HARVEST } from '../net/client.js'

test('拜访工具仅开放浇水/除草/除虫/偷菜', () => {
  assert.equal(isVisitTool('water'), true)
  assert.equal(isVisitTool('weed'), true)
  assert.equal(isVisitTool('pest'), true)
  assert.equal(isVisitTool('steal'), true)
  assert.equal(isVisitTool('remove'), false)
  assert.equal(isVisitTool('plant'), false)
  assert.equal(isVisitTool('harvest'), false)
})

test('偷菜只在成熟地块映射 Steal 命令', () => {
  assert.equal(plotCmdForTool('steal', PLOT.MATURE), CMD_STEAL)
  assert.equal(plotCmdForTool('steal', PLOT.GROWING), null)
  assert.equal(plotCmdForTool('water', PLOT.GROWING), CMD_WATER)
  assert.equal(plotCmdForTool('weed', PLOT.GROWING), CMD_REMOVE_WEED)
  assert.equal(plotCmdForTool('pest', PLOT.GROWING), CMD_REMOVE_PEST)
  assert.equal(plotCmdForTool('remove', PLOT.GROWING), CMD_CLEAR)
  assert.equal(plotCmdForTool('harvest', PLOT.MATURE), CMD_HARVEST)
  assert.equal(plotCmdForTool('water', PLOT.MATURE), CMD_WATER)
  assert.equal(plotCmdForTool('weed', PLOT.MATURE), CMD_REMOVE_WEED)
  assert.equal(plotCmdForTool('pest', PLOT.MATURE), CMD_REMOVE_PEST)
  assert.equal(plotCmdForTool('remove', PLOT.MATURE), null)
  assert.equal(plotCmdForTool('remove', PLOT.WITHERED), CMD_CLEAR)
})

test('铲除工具可以移除生长中的植物和枯萎植物', () => {
  assert.equal(plotCmdForTool('remove', PLOT.GROWING), CMD_CLEAR)
  assert.equal(plotCmdForTool('remove', PLOT.WITHERED), CMD_CLEAR)
  assert.equal(plotCmdForTool('remove', PLOT.TILLED), null)
})
