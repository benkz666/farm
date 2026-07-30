// ============================================================
// 作物 SVG 图标：商店、背包、仓库、播种栏和图鉴共用。
// 每一种作物都有独立的轮廓，避免以首字代替物品图。
// ============================================================

const leaf = 'var(--crop-leaf)';
const main = 'var(--crop-main)';
const deep = 'var(--crop-deep)';
const light = 'var(--crop-light)';

const ART = Object.freeze({
  bailuobo: `
    <path d="M30 23c-7 7-10 17-5 25 3 5 10 5 14 0 6-8 1-19-5-25Z" fill="${main}"/>
    <path d="M31 25c-2 7-2 15 1 21" fill="none" stroke="${deep}" stroke-width="2" stroke-linecap="round" opacity=".45"/>
    <path d="M31 25c-8-3-12-9-10-14 6 1 10 5 11 11M33 24c2-8 7-11 13-10-1 7-5 11-11 12" fill="${leaf}"/>
  `,
  huluobo: `
    <path d="M31 24c-7 7-11 20-5 27 4 4 9 1 11-5 3-8-1-16-6-22Z" fill="${main}"/>
    <path d="M27 32l7-2M25 38l9-3" stroke="${deep}" stroke-width="2" stroke-linecap="round" opacity=".55"/>
    <path d="M30 25c-9-2-12-8-11-13 6 1 10 4 12 10M33 24c0-8 5-12 11-12 0 7-3 11-9 13M31 24c-4-8-1-13 3-16 4 5 4 11 0 17" fill="${leaf}"/>
  `,
  dabaicai: `
    <path d="M18 41c-5-12 3-25 14-26 4 10 2 21-5 29-4 3-7 2-9-3Z" fill="#7caf6e"/>
    <path d="M46 41c5-12-3-25-14-26-4 10-2 21 5 29 4 3 7 2 9-3Z" fill="${leaf}"/>
    <path d="M23 42c-2-13 3-23 9-28 7 5 11 15 9 28-4 6-14 6-18 0Z" fill="${main}"/>
    <path d="M32 19v25M25 27c4 3 7 4 11 0M24 35c4 3 9 3 16-1" stroke="#edf7dd" stroke-width="1.8" fill="none" opacity=".7"/>
  `,
  xiaomai: `
    <path d="M32 53V17M32 30 22 22M32 37l11-10" stroke="#8b7540" stroke-width="2.5" stroke-linecap="round"/>
    <path d="M30 17c-9 2-10 8-3 10M34 18c9 2 9 8 2 10M30 25c-10 2-10 8-3 10M34 26c9 2 9 8 2 10M30 33c-8 3-8 8-2 10M34 34c8 3 8 8 2 10" fill="${main}"/>
  `,
  shuidao: `
    <path d="M25 52c5-12 6-23 5-34M24 36c-7-5-10-9-10-15M28 42c8-5 11-10 12-16" fill="none" stroke="${leaf}" stroke-width="2.5" stroke-linecap="round"/>
    <path d="M35 18c8 4 9 11 4 20" fill="none" stroke="#8c7e3d" stroke-width="2" stroke-linecap="round"/>
    <g fill="${main}"><ellipse cx="39" cy="20" rx="3" ry="2" transform="rotate(28 39 20)"/><ellipse cx="42" cy="25" rx="3" ry="2" transform="rotate(28 42 25)"/><ellipse cx="43" cy="30" rx="3" ry="2" transform="rotate(28 43 30)"/><ellipse cx="41" cy="35" rx="3" ry="2" transform="rotate(28 41 35)"/></g>
  `,
  yumi: `
    <path d="M26 18c10-5 17 2 15 16-1 12-7 18-15 18-8-3-10-16-6-26 1-4 3-7 6-8Z" fill="${leaf}"/>
    <path d="M30 18c8 1 10 8 8 18-1 9-5 14-10 15-5-6-6-20-2-28Z" fill="${main}"/>
    <path d="M31 25h7M30 31h8M30 37h7M29 43h6" stroke="#f8eaa1" stroke-width="1.8" stroke-linecap="round" opacity=".8"/>
    <path d="M27 26c-7 4-10 12-9 18 7-2 10-8 11-15M37 28c7 3 9 9 8 15-6-2-9-7-9-13" fill="${leaf}"/>
  `,
  tudou: `
    <path d="M17 41c0-9 8-16 17-13 8 3 9 15 2 20-8 5-19 1-19-7Z" fill="${main}"/>
    <path d="M36 33c7-4 14 1 13 9-1 8-10 12-16 7-5-4-3-12 3-16Z" fill="${deep}"/>
    <g fill="#8d6d45" opacity=".7"><circle cx="25" cy="37" r="1.5"/><circle cx="31" cy="45" r="1.4"/><circle cx="42" cy="40" r="1.4"/><circle cx="38" cy="47" r="1.2"/></g>
    <path d="M29 28c-5-7-2-13 4-16 3 6 1 11-3 16" fill="${leaf}"/>
  `,
  hongzao: `
    <path d="M19 49c6-8 9-17 11-29M27 31l13-8M25 37 14 31" fill="none" stroke="#76503b" stroke-width="2.8" stroke-linecap="round"/>
    <path d="M30 25c2-6 7-8 12-7-2 6-6 8-11 9M23 36c-5-4-9-4-12-1 4 4 8 5 12 3" fill="${leaf}"/>
    <g fill="${main}"><ellipse cx="43" cy="21" rx="4" ry="5" transform="rotate(25 43 21)"/><ellipse cx="37" cy="28" rx="4" ry="5" transform="rotate(25 37 28)"/><ellipse cx="14" cy="30" rx="4" ry="5" transform="rotate(-30 14 30)"/></g>
  `,
  qiezi: `
    <path d="M26 22c10-2 19 5 16 16-2 9-10 16-18 14-8-2-9-12-5-19 2-5 4-9 7-11Z" fill="${main}"/>
    <path d="M27 23c2-7 8-10 14-7-2 6-6 9-12 10" fill="${leaf}"/>
    <path d="M29 26c3-4 7-6 12-5" fill="none" stroke="#d5a7e8" stroke-width="2" stroke-linecap="round" opacity=".65"/>
  `,
  fanqie: `
    <circle cx="32" cy="36" r="15" fill="${main}"/>
    <path d="M32 22l4 7 8-1-5 6 3 7-10-4-10 4 3-7-5-6 8 1 4-7Z" fill="${leaf}"/>
    <path d="M26 34c2-5 6-7 10-7" fill="none" stroke="#ffd1c7" stroke-width="2" stroke-linecap="round" opacity=".7"/>
  `,
  wandou: `
    <path d="M20 35c7-15 19-17 26-8 4 5 2 11-4 14-10 5-19 3-22-6Z" fill="${main}"/>
    <path d="M20 35c8 5 17 5 25 1" fill="none" stroke="#e7f5cf" stroke-width="2" stroke-linecap="round" opacity=".8"/>
    <g fill="#d8f0b5"><circle cx="28" cy="35" r="2.2"/><circle cx="34" cy="36" r="2.2"/><circle cx="40" cy="34" r="2.2"/></g>
    <path d="M22 32c-4-8-2-13 4-16 3 6 2 11-2 16" fill="${leaf}"/>
  `,
  hongmeigui: `
    <path d="M32 53V35M32 43l-8-6M32 47l8-5" fill="none" stroke="#5a8040" stroke-width="2.6" stroke-linecap="round"/>
    <path d="M24 38c-4 0-6 3-7 6 4 1 7-1 9-4M40 42c4-1 7 1 8 4-4 2-7 0-9-2" fill="${leaf}"/>
    <path d="M32 17c10 0 15 10 8 18-5 6-13 5-17 0-6-8-1-18 9-18Z" fill="${main}"/>
    <path d="M27 26c3-5 9-5 11 0 2 5-3 10-6 10-4 0-8-5-5-10ZM24 29c-5-2-6-7-2-10 5 0 7 4 5 8M40 29c5-2 6-7 2-10-5 0-7 4-5 8" fill="none" stroke="#ffd2df" stroke-width="2" stroke-linecap="round"/>
  `,
  lajiao: `
    <path d="M33 23c11 3 13 13 7 22-5 8-14 11-20 6-4-4 0-10 5-13 5-3 4-9 8-15Z" fill="${main}"/>
    <path d="M32 24c-2-6 1-10 7-11 1 6-2 10-6 13" fill="${leaf}"/>
    <path d="M27 42c4 0 8-3 10-7" fill="none" stroke="#ffb1ad" stroke-width="2" stroke-linecap="round" opacity=".7"/>
  `,
  nangua: `
    <path d="M32 22c12 0 19 8 17 18-2 10-11 15-17 14-9 1-17-6-17-15 0-10 7-17 17-17Z" fill="${main}"/>
    <path d="M32 23v30M23 27c4 7 4 16 0 24M41 27c-4 7-4 16 0 24" fill="none" stroke="${deep}" stroke-width="2.2" opacity=".65"/>
    <path d="M32 23c-2-6 1-10 7-11 1 6-2 10-6 12" fill="${leaf}"/>
  `,
  pingguo: `
    <path d="M32 23c9-7 18 0 18 13 0 11-8 18-18 18S14 47 14 36c0-13 9-20 18-13Z" fill="${main}"/>
    <path d="M32 23c-1-6 2-10 8-11" fill="none" stroke="#5a4536" stroke-width="2.5" stroke-linecap="round"/>
    <path d="M34 17c5-5 10-4 13 0-5 4-10 5-14 2" fill="${leaf}"/>
    <path d="M22 34c3-6 7-8 11-8" fill="none" stroke="#ffd5d8" stroke-width="2.4" stroke-linecap="round" opacity=".7"/>
  `,
  caomei: `
    <path d="M32 25c10 0 14 8 10 16-3 7-7 13-10 16-4-3-9-9-12-16-3-8 2-16 12-16Z" fill="${main}"/>
    <path d="M32 25l4 5 7-1-4 6-7-2-7 2-4-6 7 1 4-5Z" fill="${leaf}"/>
    <g fill="#ffeab2"><circle cx="27" cy="39" r="1"/><circle cx="33" cy="38" r="1"/><circle cx="38" cy="41" r="1"/><circle cx="30" cy="46" r="1"/><circle cx="35" cy="48" r="1"/></g>
  `,
  xigua: `
    <ellipse cx="32" cy="37" rx="18" ry="14" fill="${main}"/>
    <path d="M20 28c4 8 4 17 0 22M29 24c4 9 4 20 0 27M39 24c-4 9-4 20 0 27M46 28c-4 8-4 17 0 22" fill="none" stroke="#b6e27c" stroke-width="2.2" opacity=".85"/>
    <path d="M25 25c-3-7 0-11 6-13 2 6 0 10-4 14" fill="${leaf}"/>
  `,
  xiangjiao: `
    <path d="M20 23c7 11 16 15 26 11-3 12-16 19-26 9-5-5-5-13 0-20Z" fill="${main}"/>
    <path d="M20 23c9 10 18 13 26 11" fill="none" stroke="#c89f2d" stroke-width="2.2" stroke-linecap="round"/>
    <path d="M20 23c-1-5 2-8 7-9" fill="none" stroke="${deep}" stroke-width="3" stroke-linecap="round"/>
    <path d="M35 24c6 5 11 5 15 2" fill="none" stroke="#f9cc45" stroke-width="5" stroke-linecap="round"/>
  `,
  taozi: `
    <path d="M32 24c9-8 18 1 16 13-1 10-8 17-16 17s-15-7-16-17c-2-12 7-21 16-13Z" fill="${main}"/>
    <path d="M32 25c-2 8-2 17 0 27" fill="none" stroke="#e78095" stroke-width="1.8" opacity=".7"/>
    <path d="M32 24c1-7 5-10 11-9-2 6-6 9-10 11" fill="${leaf}"/>
  `,
  chengzi: `
    <circle cx="32" cy="37" r="16" fill="${main}"/>
    <path d="M32 21c0-6 3-10 9-11-1 6-4 10-8 12" fill="${leaf}"/>
    <path d="M22 35c4-7 9-9 14-9" fill="none" stroke="#ffd58c" stroke-width="2.2" stroke-linecap="round" opacity=".75"/>
    <g fill="#d87812" opacity=".6"><circle cx="27" cy="44" r="1"/><circle cx="37" cy="46" r="1"/><circle cx="42" cy="37" r="1"/></g>
  `,
  putao: `
    <path d="M31 18c5-5 10-4 14 0-5 4-10 5-14 2" fill="${leaf}"/>
    <path d="M31 20c0 6 1 9 4 12" fill="none" stroke="#6d4c41" stroke-width="2.2" stroke-linecap="round"/>
    <g fill="${main}"><circle cx="29" cy="31" r="6"/><circle cx="38" cy="32" r="6"/><circle cx="24" cy="40" r="6"/><circle cx="33" cy="41" r="6"/><circle cx="42" cy="41" r="6"/><circle cx="29" cy="49" r="5.5"/><circle cx="38" cy="49" r="5.5"/></g>
    <path d="M26 29c2-2 4-3 6-3" fill="none" stroke="#ddc4ff" stroke-width="1.8" stroke-linecap="round" opacity=".65"/>
  `,
  shiliu: `
    <path d="M22 27l4-7 3 4 3-7 3 7 3-4 4 7c5 6 2 22-10 25-12-3-15-19-10-25Z" fill="${main}"/>
    <path d="M24 34c6-4 12-4 18 0" fill="none" stroke="#ff9a9a" stroke-width="2" stroke-linecap="round" opacity=".75"/>
    <g fill="#ffd1b9"><circle cx="27" cy="42" r="1.4"/><circle cx="33" cy="45" r="1.4"/><circle cx="39" cy="41" r="1.4"/></g>
  `,
  youzi: `
    <circle cx="32" cy="37" r="17" fill="${main}"/>
    <path d="M32 20c0-7 4-10 10-10-1 6-4 10-9 12" fill="${leaf}"/>
    <path d="M21 34c4-7 9-9 15-9" fill="none" stroke="#fff2a4" stroke-width="2.4" stroke-linecap="round" opacity=".8"/>
    <circle cx="40" cy="44" r="1.3" fill="#c8a238" opacity=".7"/>
  `,
  boluo: `
    <path d="M31 22c9 0 14 9 11 19-3 10-7 16-11 16s-9-6-12-16c-3-10 3-19 12-19Z" fill="${main}"/>
    <path d="M31 22c-4-9-10-11-14-9 3 6 7 9 13 12M32 22c0-9 4-13 9-14 1 7-2 11-7 15M34 23c5-7 10-7 14-4-5 5-9 6-14 7" fill="${leaf}"/>
    <path d="M23 35l16 14M40 35 24 49M24 29l15 13M39 29 25 42" stroke="#c58c34" stroke-width="1.8" opacity=".8"/>
  `,
  yezi: `
    <path d="M32 53c1-16 1-26 0-36" fill="none" stroke="#805a3f" stroke-width="4" stroke-linecap="round"/>
    <path d="M32 20c-7-7-14-7-20-3 7 4 13 5 20 4M32 20c6-8 13-9 19-5-6 6-12 7-19 7M32 20c-1-9 2-14 8-17 1 8-2 13-6 18" fill="${leaf}"/>
    <g fill="${main}"><circle cx="25" cy="30" r="6"/><circle cx="36" cy="30" r="6"/><circle cx="31" cy="39" r="6"/></g>
  `,
  hulu: `
    <path d="M31 20c7 0 11 5 9 12-1 4-4 6-7 7 8 2 11 8 8 14-3 7-16 7-20 0-3-6 1-12 8-14-4-2-7-5-7-10 0-5 4-9 8-9Z" fill="${main}"/>
    <path d="M31 20c0-6 3-9 8-10" fill="none" stroke="${leaf}" stroke-width="2.8" stroke-linecap="round"/>
    <path d="M25 48c5 2 10 2 15 0" fill="none" stroke="#eaf5b9" stroke-width="2" opacity=".8"/>
  `,
  renshen: `
    <path d="M32 28c-2 7-1 14 0 23M32 39l-7 10M32 41l7 10M32 34l-5-6M33 35l6-6" fill="none" stroke="${main}" stroke-width="3" stroke-linecap="round"/>
    <path d="M31 27c-9-1-12-6-11-12 6 1 10 4 12 10M32 27c0-8 4-12 10-13 1 7-2 11-8 14M33 27c7-4 12-2 15 3-6 3-11 2-15-1" fill="${leaf}"/>
  `,
  lingzhi: `
    <path d="M32 51V31" fill="none" stroke="#e8d8b9" stroke-width="6" stroke-linecap="round"/>
    <path d="M16 33c1-13 12-20 24-15 7 3 10 9 8 15-9-4-20-4-32 0Z" fill="${main}"/>
    <path d="M19 32c8-6 18-6 26 0" fill="none" stroke="#e8b899" stroke-width="2.2" stroke-linecap="round" opacity=".8"/>
    <path d="M25 24c5-4 10-4 15 0" fill="none" stroke="#7b3f2d" stroke-width="2" stroke-linecap="round" opacity=".7"/>
  `,
  yaoqianshu: `
    <path d="M32 54c0-14 0-24-2-35M30 33 19 27M31 39l12-9M31 27l8-8" fill="none" stroke="#865c32" stroke-width="3" stroke-linecap="round"/>
    <g fill="${leaf}"><circle cx="18" cy="25" r="7"/><circle cx="40" cy="18" r="8"/><circle cx="45" cy="29" r="8"/><circle cx="24" cy="17" r="8"/></g>
    <g fill="${main}" stroke="#c58e00" stroke-width="1.5"><circle cx="20" cy="25" r="4"/><circle cx="40" cy="18" r="4.5"/><circle cx="45" cy="29" r="4.5"/><circle cx="24" cy="17" r="4.5"/></g>
    <path d="M18 25h4M22 23v4M38 18h4M40 16v4M43 29h4M45 27v4" stroke="#fff5aa" stroke-width="1.2" stroke-linecap="round"/>
  `,
  fallback: `
    <path d="M32 53V29M32 31c-9-1-13-7-11-14 7 1 11 5 12 12M33 31c1-9 6-13 13-12-1 7-5 11-12 13" fill="${leaf}"/>
    <path d="M22 52h20" stroke="${deep}" stroke-width="2.5" stroke-linecap="round"/>
  `,
});

function hex(value) {
  if (typeof value === 'string') return value;
  return `#${Number(value || 0).toString(16).padStart(6, '0')}`;
}

function darker(hexColor, amount = 36) {
  const value = Number.parseInt(hexColor.slice(1), 16);
  const channel = (shift) => Math.max(0, ((value >> shift) & 255) - amount).toString(16).padStart(2, '0');
  return `#${channel(16)}${channel(8)}${channel(0)}`;
}

/** 返回可内联到 DOM 的无文字 SVG 作物图。 */
export function cropIconHTML(crop) {
  const color = crop?.color || '#8ac46c';
  const leafColor = hex(crop?.leaf || 0x5aa54a);
  const art = ART[crop?.id] || ART.fallback;
  return `<svg class="crop-art" data-crop="${crop?.id || 'unknown'}" viewBox="0 0 64 64" aria-hidden="true" focusable="false" style="--crop-main:${color};--crop-deep:${darker(color)};--crop-leaf:${leafColor};--crop-light:#fff9e8">${art}</svg>`;
}

/** 供测试确保新增作物时必须补齐插画。 */
export const CROP_ICON_IDS = Object.freeze(Object.keys(ART).filter((id) => id !== 'fallback'));
