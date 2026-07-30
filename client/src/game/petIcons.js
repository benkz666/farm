// ============================================================
// 看家狗 SVG 图标：商店、宠物面板和顶部状态入口共用。
// ============================================================

const DOG_ART = Object.freeze({
  tugou: `
    <ellipse cx="49" cy="58" rx="35" ry="3" fill="#4e3327" opacity=".14"/>
    <path d="M29 35C18 37 12 30 16 23c3-6 11-7 14-2 2 4-1 8-5 7" fill="none" stroke="#4e3327" stroke-width="7" stroke-linecap="round"/>
    <path d="M29 35C18 37 12 30 16 23c3-6 11-7 14-2" fill="none" stroke="#b7793e" stroke-width="4.3" stroke-linecap="round"/>
    <path d="M31 31c9-8 27-8 39-1l8 8-4 10H37c-9 0-14-10-6-17Z" fill="#b7793e" stroke="#4e3327" stroke-width="2.4" stroke-linejoin="round"/>
    <path d="M35 45 32 57h7l4-12M65 45l1 12h7l1-14" fill="#a66435" stroke="#4e3327" stroke-width="2.4" stroke-linejoin="round"/>
    <path d="M44 46 43 57h7l2-10M71 44l6 12h7l-3-16" fill="#d69758" stroke="#4e3327" stroke-width="2.4" stroke-linejoin="round"/>
    <path d="M63 30c2-8 9-13 17-10l3 4c5 1 9 4 11 8l-5 5-13 1-7 8-7-5Z" fill="#c9894d" stroke="#4e3327" stroke-width="2.4" stroke-linejoin="round"/>
    <path d="m68 23 2-12 10 9" fill="#9a5b34" stroke="#4e3327" stroke-width="2.4" stroke-linejoin="round"/>
    <path d="M78 31c5-3 10-2 15 1l-4 5-11 1Z" fill="#f5d0a6"/>
    <path d="M66 32c5 3 10 3 15 0" fill="none" stroke="#d64a3a" stroke-width="3.4"/>
    <path d="M70 29c-4 4-4 10 0 15" fill="#f5d0a6" opacity=".72"/>
    <path d="M84 26c-2-2-5-2-7 0" fill="none" stroke="#4e3327" stroke-width="1.8" stroke-linecap="round"/>
    <circle cx="82" cy="27" r="1.8" fill="#241915"/>
    <path d="m92 31 3 2-4 3-3-2Z" fill="#241915"/>
  `,
  muyang: `
    <ellipse cx="49" cy="58" rx="36" ry="3" fill="#26313d" opacity=".14"/>
    <path d="M30 34C20 35 12 42 8 51c9-1 16-5 23-12" fill="#586575" stroke="#26313d" stroke-width="2.5" stroke-linejoin="round"/>
    <path d="M29 31c11-7 30-7 42 0l8 8-6 9H37c-10 0-16-10-8-17Z" fill="#8d99a8" stroke="#26313d" stroke-width="2.5" stroke-linejoin="round"/>
    <path d="M36 44 33 57h8l4-13M65 45l2 12h8l-1-15" fill="#687789" stroke="#26313d" stroke-width="2.5" stroke-linejoin="round"/>
    <path d="M45 46 45 57h8l1-11M73 42l6 15h8l-3-18" fill="#dce4e6" stroke="#26313d" stroke-width="2.5" stroke-linejoin="round"/>
    <path d="M36 29c8-4 21-4 31 0l-9 14H38Z" fill="#3d4a59"/>
    <path d="M65 31c1-10 8-17 17-14l3 6c5 1 9 5 11 9l-5 6-13-1-7 9-8-6Z" fill="#7f8d9b" stroke="#26313d" stroke-width="2.5" stroke-linejoin="round"/>
    <path d="m68 23 1-14 11 10M79 18l7-11 2 17" fill="#4b5868" stroke="#26313d" stroke-width="2.5" stroke-linejoin="round"/>
    <path d="M79 30c5-3 11-2 16 2l-4 6-12-1Z" fill="#edf1ef"/>
    <path d="M69 31c5 3 10 4 16 1" fill="none" stroke="#2e8ab8" stroke-width="3.5"/>
    <path d="M69 27c1 6 3 12 7 17" fill="#edf1ef" opacity=".88"/>
    <path d="M87 25c-2-2-5-2-7 0" fill="none" stroke="#26313d" stroke-width="1.8" stroke-linecap="round"/>
    <circle cx="85" cy="26.5" r="1.8" fill="#17202a"/>
    <path d="m94 31 3 2-4 4-4-3Z" fill="#17202a"/>
  `,
  zangao: `
    <ellipse cx="49" cy="58" rx="38" ry="3.2" fill="#171312" opacity=".18"/>
    <path d="M30 35C17 39 11 31 17 23c5-7 15-5 16 3-1 5-5 8-10 7" fill="none" stroke="#171312" stroke-width="10" stroke-linecap="round"/>
    <path d="M30 35C18 39 13 32 18 25c4-5 11-4 12 2" fill="none" stroke="#5a473b" stroke-width="6" stroke-linecap="round"/>
    <path d="M27 30c11-9 31-9 45-1l10 11-7 10H36c-12 0-18-11-9-20Z" fill="#4b3a32" stroke="#171312" stroke-width="2.8" stroke-linejoin="round"/>
    <path d="M31 43 28 57h9l5-14M65 45l2 12h9l1-16" fill="#352925" stroke="#171312" stroke-width="2.8" stroke-linejoin="round"/>
    <path d="M43 46 42 57h9l3-11M73 42l7 15h10l-4-19" fill="#5a473b" stroke="#171312" stroke-width="2.8" stroke-linejoin="round"/>
    <path d="M60 27c4-9 14-15 24-10l-1 5c8 1 13 6 15 12l-6 7-15-2-7 10-12-8Z" fill="#392e2a" stroke="#171312" stroke-width="2.8" stroke-linejoin="round"/>
    <path d="m66 22 2-12 11 9M80 18l8-9 1 15" fill="#261f1c" stroke="#171312" stroke-width="2.8" stroke-linejoin="round"/>
    <path d="M57 25c4-7 12-12 19-10l-2 5 7 1-4 5 6 3-6 4 4 5-8 1 1 7-7-3-5 5-3-7-7-1 5-6-5-4 7-3Z" fill="#5a473b" stroke="#171312" stroke-width="2.2" stroke-linejoin="round"/>
    <path d="M79 31c5-3 12-1 18 3l-5 7-13-2Z" fill="#816658"/>
    <path d="M66 30c5 4 11 5 17 2" fill="none" stroke="#c79235" stroke-width="4"/>
    <circle cx="70" cy="33" r="1.4" fill="#ead18b"/><circle cx="77" cy="34" r="1.4" fill="#ead18b"/>
    <path d="M89 26c-2-2-5-2-7 0" fill="none" stroke="#171312" stroke-width="2" stroke-linecap="round"/>
    <circle cx="87" cy="27.5" r="1.9" fill="#090807"/>
    <path d="m96 33 3 2-4 4-4-3Z" fill="#090807"/>
  `,
  fallback: `
    <ellipse cx="49" cy="58" rx="35" ry="3" fill="#35261f" opacity=".14"/>
    <path d="M29 32c11-8 30-8 43 0l9 9-7 8H37c-10 0-16-10-8-17Z" fill="#a87f5b" stroke="#35261f" stroke-width="2.5"/>
    <path d="M36 45 33 57h8l4-12M68 44l3 13h8l-2-15" fill="#896448" stroke="#35261f" stroke-width="2.5"/>
    <path d="M66 31c2-9 10-14 18-10l2 4c5 1 9 4 11 8l-5 6-13-1-7 9-8-7Z" fill="#a87f5b" stroke="#35261f" stroke-width="2.5"/>
    <path d="m71 23 2-12 10 10M83 30c5-2 10 0 14 3l-5 6-10-2Z" fill="#f0d1af" stroke="#35261f" stroke-width="2.2"/>
    <circle cx="87" cy="27" r="1.8" fill="#211713"/><path d="m95 32 3 2-4 4-4-3Z" fill="#211713"/>
  `,
});

function escapeAttribute(value) {
  return String(value || '看家狗').replace(/[&<>"']/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]);
}

/** 返回可内联到 DOM 的无文字宠物 SVG。 */
export function petIconHTML(dog) {
  const art = DOG_ART[dog?.id] || DOG_ART.fallback;
  return `<svg class="pet-art" data-pet="${dog?.id || 'unknown'}" data-pose="profile-full-body" viewBox="0 0 100 64" aria-hidden="true" focusable="false">${art}</svg>`;
}

export function petBadgeHTML(dog, size = 'md') {
  return `<span class="pet-badge pet-badge--${size}" role="img" aria-label="${escapeAttribute(dog?.name)}">${petIconHTML(dog)}</span>`;
}

/** 供测试确保每个可选宠物都拥有专属插画。 */
export const PET_ICON_IDS = Object.freeze(Object.keys(DOG_ART).filter((id) => id !== 'fallback'));
