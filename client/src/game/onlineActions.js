/**
 * online 工具栏 → 协议 cmd 映射。
 */
import { PLOT } from './state.js'
import {
  CMD_TILL,
  CMD_CLEAR,
  CMD_PLANT,
  CMD_WATER,
  CMD_REMOVE_WEED,
  CMD_REMOVE_PEST,
  CMD_FERTILIZE,
  CMD_HARVEST,
  CMD_STEAL,
} from '../net/client.js'

/** 拜访好友农场时开放的互助 / 偷菜工具。 */
export const VISIT_TOOL_IDS = Object.freeze(['water', 'weed', 'pest', 'steal'])

/**
 * @param {string|null} tool
 * @returns {boolean}
 */
export function isVisitTool(tool) {
  return VISIT_TOOL_IDS.includes(tool)
}

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
      return CMD_FERTILIZE
    case 'steal':
      return plotState === PLOT.MATURE ? CMD_STEAL : null
    default:
      return null
  }
}
