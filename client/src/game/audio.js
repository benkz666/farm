// ============================================================
// WebAudio 合成音效，无需外部音频文件
// ============================================================
export class SFX {
  constructor() { this.ctx = null; this.enabled = true; }

  ensure() {
    if (!this.ctx) {
      try { this.ctx = new (window.AudioContext || window.webkitAudioContext)(); } catch (e) { this.enabled = false; }
    }
    if (this.ctx && this.ctx.state === 'suspended') this.ctx.resume();
    return this.ctx;
  }

  tone(freq, dur = 0.12, type = 'sine', vol = 0.15, delay = 0, slideTo = null) {
    if (!this.enabled || !this.ensure()) return;
    const t0 = this.ctx.currentTime + delay;
    const osc = this.ctx.createOscillator();
    const gain = this.ctx.createGain();
    osc.type = type; osc.frequency.setValueAtTime(freq, t0);
    if (slideTo) osc.frequency.exponentialRampToValueAtTime(slideTo, t0 + dur);
    gain.gain.setValueAtTime(0, t0);
    gain.gain.linearRampToValueAtTime(vol, t0 + 0.012);
    gain.gain.exponentialRampToValueAtTime(0.001, t0 + dur);
    osc.connect(gain); gain.connect(this.ctx.destination);
    osc.start(t0); osc.stop(t0 + dur + 0.05);
  }

  noise(dur = 0.2, vol = 0.12, freq = 800, delay = 0) {
    if (!this.enabled || !this.ensure()) return;
    const t0 = this.ctx.currentTime + delay;
    const len = Math.max(1, Math.floor(this.ctx.sampleRate * dur));
    const buf = this.ctx.createBuffer(1, len, this.ctx.sampleRate);
    const data = buf.getChannelData(0);
    for (let i = 0; i < len; i++) data[i] = (Math.random() * 2 - 1) * (1 - i / len);
    const src = this.ctx.createBufferSource(); src.buffer = buf;
    const filter = this.ctx.createBiquadFilter(); filter.type = 'bandpass'; filter.frequency.value = freq; filter.Q.value = 0.8;
    const gain = this.ctx.createGain(); gain.gain.value = vol;
    src.connect(filter); filter.connect(gain); gain.connect(this.ctx.destination);
    src.start(t0);
  }

  click()    { this.tone(880, 0.06, 'triangle', 0.08); }
  till()     { this.noise(0.18, 0.16, 300); this.tone(120, 0.15, 'sine', 0.12); }
  plant()    { this.tone(440, 0.08, 'triangle', 0.1); this.tone(660, 0.1, 'triangle', 0.1, 0.07); }
  water()    { this.noise(0.35, 0.12, 1400); this.tone(500, 0.3, 'sine', 0.05, 0, 220); }
  weed()     { this.noise(0.22, 0.14, 600); }
  pest()     { this.noise(0.15, 0.12, 900); this.tone(300, 0.1, 'square', 0.05, 0.05); }
  remove()   { this.noise(0.2, 0.14, 260); this.tone(150, 0.12, 'sine', 0.08); }
  fertilize(){ this.tone(520, 0.1, 'sine', 0.1); this.tone(780, 0.14, 'sine', 0.1, 0.08); }
  mature()   { [523, 659, 784, 1047].forEach((f, i) => this.tone(f, 0.16, 'triangle', 0.1, i * 0.06)); }
  harvest()  { [523, 659, 784].forEach((f, i) => this.tone(f, 0.12, 'triangle', 0.12, i * 0.07)); }
  steal()    { this.tone(600, 0.09, 'sawtooth', 0.06); this.tone(900, 0.12, 'sawtooth', 0.06, 0.08); }
  gold()     { this.tone(988, 0.09, 'triangle', 0.12); this.tone(1319, 0.14, 'triangle', 0.12, 0.08); }
  error()    { this.tone(220, 0.12, 'square', 0.07); this.tone(180, 0.16, 'square', 0.07, 0.1); }
  levelup()  { [523, 659, 784, 1047].forEach((f, i) => this.tone(f, 0.16, 'triangle', 0.12, i * 0.09)); }
  dog()      { this.tone(350, 0.08, 'sawtooth', 0.14); this.tone(280, 0.1, 'sawtooth', 0.14, 0.1); }
  mail()     { this.tone(1047, 0.1, 'sine', 0.1); this.tone(1319, 0.16, 'sine', 0.1, 0.1); }
  task()     { [784, 988, 1175].forEach((f, i) => this.tone(f, 0.12, 'sine', 0.1, i * 0.08)); }
}
