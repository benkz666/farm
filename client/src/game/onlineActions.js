/**
 * online 工具栏 → 协议 cmd 映射（期 2b；不含 Fertilize）。
 */
import { PLOT } from './state.js'
import {
  CMD_TILL,
  CMD_CLEAR,
  CMD_PLANT,
  CMD_WATER,
  CMD_REMOVE_WEED,
  CMD_REMOVE_PEST,
  CMD_HARVEST,
} from '../net/client.js'

/**
 * @param {string|null} tool
 * @param {string} plotState PLOT.*
 * @returns {number|null} cmd，或 null（未选工具 / 本期不支持）
 */
export function plotCmdForTool(tool, plotState) {
  switch (tool) {
    case 'till':
      if (plotState === PLOT.WASTELAND) return CMD_TILL
      if (plotState === PLOT.RESIDUE || plotState === PLOT.WITHERED) return CMD_CLEAR
      return null
    case 'plant':
      return CMD_PLANT
    case 'water':
      return CMD_WATER
    case 'weed':
      return CMD_REMOVE_WEED
    case 'pest':
      return CMD_REMOVE_PEST
    case 'harvest':
      return CMD_HARVEST
    case 'fert':
      return null // Task 10
    default:
      return null
  }
}
