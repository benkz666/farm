// ============================================================
// 看家狗 SVG 图标：商店、宠物面板和顶部状态入口共用。
// ============================================================

const DOG_ART = Object.freeze({
  tugou: `
    <path d="M18 25 13 13c-1-4 4-6 8-3l7 8M46 25l5-12c1-4-4-6-8-3l-7 8" fill="#8d674c"/>
    <path d="M18 27c0-10 6-17 14-17s14 7 14 17v13c0 10-6 16-14 16s-14-6-14-16V27Z" fill="#b08968"/>
    <path d="M22 36c4-4 16-4 20 0v8c-3 6-17 6-20 0v-8Z" fill="#f1d0ac"/>
    <circle cx="26" cy="31" r="2.5" fill="#2f241f"/><circle cx="38" cy="31" r="2.5" fill="#2f241f"/>
    <path d="M29 40h6l-3 3-3-3Z" fill="#39271f"/><path d="M32 43v4M28 47c2 2 6 2 8 0" fill="none" stroke="#39271f" stroke-width="1.8" stroke-linecap="round"/>
    <path d="M22 25c2-5 5-8 10-8" fill="none" stroke="#d8ad84" stroke-width="2" stroke-linecap="round" opacity=".75"/>
  `,
  muyang: `
    <path d="M18 26 18 9c0-4 5-5 8-1l7 10M46 26V9c0-4-5-5-8-1l-7 10" fill="#687487"/>
    <path d="M18 27c0-11 6-18 14-18s14 7 14 18v14c0 9-6 15-14 15s-14-6-14-15V27Z" fill="#8d99ae"/>
    <path d="M24 20c3 4 5 9 5 17M40 20c-3 4-5 9-5 17" fill="none" stroke="#e9edf2" stroke-width="5" stroke-linecap="round"/>
    <path d="M24 38c3-5 13-5 16 0v7c-2 5-14 5-16 0v-7Z" fill="#f4eee4"/>
    <circle cx="26" cy="31" r="2.4" fill="#26313d"/><circle cx="38" cy="31" r="2.4" fill="#26313d"/>
    <path d="M29 41h6l-3 3-3-3Z" fill="#34404e"/><path d="M32 44v4M28 48c2 2 6 2 8 0" fill="none" stroke="#34404e" stroke-width="1.8" stroke-linecap="round"/>
  `,
  zangao: `
    <path d="M16 25 13 11c-1-5 5-6 9-2l7 9M48 25l3-14c1-5-5-6-9-2l-7 9" fill="#2f241f"/>
    <path d="M16 28c0-12 7-20 16-20s16 8 16 20v14c0 9-7 15-16 15s-16-6-16-15V28Z" fill="#4a3728"/>
    <path d="M20 29c3-8 8-11 12-11s9 3 12 11" fill="none" stroke="#765b44" stroke-width="3" stroke-linecap="round" opacity=".8"/>
    <path d="M22 39c3-6 15-6 20 0v7c-4 7-16 7-20 0v-7Z" fill="#9d7658"/>
    <circle cx="26" cy="33" r="2.6" fill="#17120f"/><circle cx="38" cy="33" r="2.6" fill="#17120f"/>
    <path d="M29 42h6l-3 3-3-3Z" fill="#211713"/><path d="M32 45v4M28 49c2 2 6 2 8 0" fill="none" stroke="#211713" stroke-width="1.9" stroke-linecap="round"/>
    <path d="M19 52c8 4 18 4 26 0" fill="none" stroke="#bd9268" stroke-width="3" stroke-linecap="round"/>
  `,
  fallback: `
    <path d="M19 25 15 13c-1-4 4-6 8-3l7 8M45 25l4-12c1-4-4-6-8-3l-7 8" fill="#7b6048"/>
    <path d="M18 27c0-11 6-18 14-18s14 7 14 18v14c0 9-6 15-14 15s-14-6-14-15V27Z" fill="#a87f5b"/>
    <path d="M23 38c4-5 14-5 18 0v7c-3 5-15 5-18 0v-7Z" fill="#f0d1af"/>
    <circle cx="26" cy="31" r="2.4" fill="#29201b"/><circle cx="38" cy="31" r="2.4" fill="#29201b"/><path d="M29 41h6l-3 3-3-3Z" fill="#35261f"/>
  `,
});

function escapeAttribute(value) {
  return String(value || '看家狗').replace(/[&<>"']/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]);
}

/** 返回可内联到 DOM 的无文字宠物 SVG。 */
export function petIconHTML(dog) {
  const art = DOG_ART[dog?.id] || DOG_ART.fallback;
  return `<svg class="pet-art" data-pet="${dog?.id || 'unknown'}" viewBox="0 0 64 64" aria-hidden="true" focusable="false">${art}</svg>`;
}

export function petBadgeHTML(dog, size = 'md') {
  return `<span class="pet-badge pet-badge--${size}" role="img" aria-label="${escapeAttribute(dog?.name)}">${petIconHTML(dog)}</span>`;
}

/** 供测试确保每个可选宠物都拥有专属插画。 */
export const PET_ICON_IDS = Object.freeze(Object.keys(DOG_ART).filter((id) => id !== 'fallback'));
