/**
 * 误铲 Growing 地块会扣减健康度并返回 1204；该错误是唯一携带可应用 patch 的失败响应。
 * @param {number} err
 * @param {object|null|undefined} payload
 * @returns {boolean}
 */
export function shouldApplyPatchFromError(err, payload) {
  return err === 1204
    && payload !== null
    && typeof payload === 'object'
    && payload.patch !== null
    && typeof payload.patch === 'object'
}
