/* 音效：WebAudio 实时合成，无需音频文件 */
window.Farm = window.Farm || {};

(function (Farm) {
  'use strict';

  let ctx = null;
  let master = null;

  function ensure() {
    if (ctx) return ctx;
    const AC = window.AudioContext || window.webkitAudioContext;
    if (!AC) return null;
    ctx = new AC();
    master = ctx.createGain();
    master.gain.value = 0.22;
    master.connect(ctx.destination);
    return ctx;
  }

  function enabled() {
    const st = Farm.game && Farm.game.state;
    return !st || st.settings.sound !== false;
  }

  function tone(freq, dur, opts) {
    const c = ensure();
    if (!c) return;
    const o = opts || {};
    const osc = c.createOscillator();
    const gain = c.createGain();
    const t0 = c.currentTime + (o.delay || 0);
    osc.type = o.type || 'triangle';
    osc.frequency.setValueAtTime(freq, t0);
    if (o.to) osc.frequency.exponentialRampToValueAtTime(o.to, t0 + dur);
    gain.gain.setValueAtTime(0.0001, t0);
    gain.gain.exponentialRampToValueAtTime(o.gain || 0.6, t0 + 0.012);
    gain.gain.exponentialRampToValueAtTime(0.0001, t0 + dur);
    osc.connect(gain).connect(master);
    osc.start(t0);
    osc.stop(t0 + dur + 0.02);
  }

  function noise(dur, opts) {
    const c = ensure();
    if (!c) return;
    const o = opts || {};
    const len = Math.floor(c.sampleRate * dur);
    const buf = c.createBuffer(1, len, c.sampleRate);
    const data = buf.getChannelData(0);
    for (let i = 0; i < len; i++) data[i] = (Math.random() * 2 - 1) * (1 - i / len);
    const src = c.createBufferSource();
    src.buffer = buf;
    const filter = c.createBiquadFilter();
    filter.type = o.type || 'lowpass';
    const t0 = c.currentTime + (o.delay || 0);
    filter.frequency.setValueAtTime(o.freq || 1200, t0);
    if (o.to) filter.frequency.exponentialRampToValueAtTime(o.to, t0 + dur);
    const gain = c.createGain();
    gain.gain.value = o.gain || 0.5;
    src.connect(filter).connect(gain).connect(master);
    src.start(t0);
  }

  const SFX = {
    click: function () { tone(620, 0.07, { type: 'square', gain: 0.28 }); },
    till: function () { noise(0.24, { freq: 900, to: 220, gain: 0.7 }); },
    plant: function () { tone(430, 0.1, { to: 660 }); tone(660, 0.12, { delay: 0.08, gain: 0.4 }); },
    water: function () { noise(0.45, { type: 'bandpass', freq: 2600, to: 700, gain: 0.55 }); },
    weed: function () { noise(0.2, { type: 'highpass', freq: 1400, gain: 0.45 }); },
    pest: function () { noise(0.3, { type: 'bandpass', freq: 3200, to: 1400, gain: 0.4 }); tone(240, 0.14, { to: 120, type: 'sawtooth', gain: 0.2 }); },
    harvest: function () {
      [523, 659, 784].forEach(function (f, i) { tone(f, 0.14, { delay: i * 0.07, gain: 0.5 }); });
    },
    coin: function () { tone(1180, 0.07, { type: 'square', gain: 0.3 }); tone(1560, 0.1, { delay: 0.06, type: 'square', gain: 0.26 }); },
    levelup: function () {
      [523, 659, 784, 1046, 1318].forEach(function (f, i) { tone(f, 0.2, { delay: i * 0.09, gain: 0.5 }); });
    },
    steal: function () { tone(300, 0.18, { to: 900, type: 'sine', gain: 0.35 }); },
    caught: function () { tone(400, 0.3, { to: 90, type: 'sawtooth', gain: 0.4 }); },
    error: function () { tone(200, 0.16, { to: 130, type: 'square', gain: 0.24 }); }
  };

  function play(name) {
    if (!enabled()) return;
    const fn = SFX[name];
    if (!fn) return;
    try {
      if (ctx && ctx.state === 'suspended') ctx.resume();
      fn();
    } catch (e) { /* 忽略音频异常 */ }
  }

  function unlock() {
    const c = ensure();
    if (c && c.state === 'suspended') c.resume();
  }

  Farm.audio = { play: play, unlock: unlock };
})(window.Farm);
