// Audio plumbing for the browser walkie: mic → Opus (WebCodecs) uplink,
// Opus → speaker downlink with a small jitter buffer, tones and TTS.
'use strict';

const PTTAudio = (() => {
  const SAMPLE_RATE = 16000;
  const FRAME_US = 60000;

  const supported = typeof AudioEncoder !== 'undefined' && typeof AudioDecoder !== 'undefined';

  let playCtx = null;
  let playGain = null;

  function ensurePlayback() {
    if (!playCtx) {
      playCtx = new AudioContext();
      playGain = playCtx.createGain();
      playGain.connect(playCtx.destination);
    }
    if (playCtx.state === 'suspended') playCtx.resume();
    return playCtx;
  }

  // ---------- tones ----------
  function tone(freqs, durMs, gapMs = 40, vol = 0.18) {
    const ctx = ensurePlayback();
    let t = ctx.currentTime + 0.01;
    for (const f of freqs) {
      const osc = ctx.createOscillator();
      const g = ctx.createGain();
      osc.type = 'sine';
      osc.frequency.value = f;
      g.gain.setValueAtTime(0, t);
      g.gain.linearRampToValueAtTime(vol, t + 0.005);
      g.gain.setValueAtTime(vol, t + durMs / 1000 - 0.01);
      g.gain.linearRampToValueAtTime(0, t + durMs / 1000);
      osc.connect(g).connect(ctx.destination);
      osc.start(t);
      osc.stop(t + durMs / 1000 + 0.02);
      t += (durMs + gapMs) / 1000;
    }
  }

  const tones = {
    permit: () => tone([1400, 1800], 50, 20),    // talk permit
    deny:   () => tone([420, 420], 160, 90, 0.25), // 嘟嘟：抢麦失败
    roger:  () => tone([1200], 70),               // end of someone's transmission
    alert:  () => tone([880, 660, 880, 660, 880, 660], 140, 20, 0.3),
    click:  () => tone([2000], 15, 0, 0.08),
  };

  // ---------- TTS ----------
  function speak(text) {
    if (!text || !('speechSynthesis' in window)) return;
    const u = new SpeechSynthesisUtterance(text);
    u.lang = 'zh-CN';
    u.rate = 1.15;
    speechSynthesis.speak(u);
  }
  function hush() {
    if ('speechSynthesis' in window) speechSynthesis.cancel();
  }

  // ---------- capture / encode ----------
  const WORKLET = `
    class Capture extends AudioWorkletProcessor {
      process(inputs) {
        const ch = inputs[0] && inputs[0][0];
        if (ch && ch.length) this.port.postMessage(ch.slice(0));
        return true;
      }
    }
    registerProcessor('ptt-capture', Capture);`;

  class Mic {
    constructor(onPacket) {
      this.onPacket = onPacket;
      this.active = false;
      this.ts = 0;
      this.level = 0;
    }

    async open() {
      if (this.opening) return this.opening;
      this.opening = this.doOpen().catch((e) => { this.opening = null; throw e; });
      return this.opening;
    }

    async doOpen() {
      this.stream = await navigator.mediaDevices.getUserMedia({
        audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
      });
      try {
        this.ctx = new AudioContext({ sampleRate: SAMPLE_RATE });
      } catch {
        this.ctx = new AudioContext(); // browser refuses custom rate: encode at native rate
      }
      const url = URL.createObjectURL(new Blob([WORKLET], { type: 'application/javascript' }));
      await this.ctx.audioWorklet.addModule(url);
      const src = this.ctx.createMediaStreamSource(this.stream);
      this.node = new AudioWorkletNode(this.ctx, 'ptt-capture');
      this.node.port.onmessage = (e) => this.onPCM(e.data);
      src.connect(this.node);
      // Keep the graph pulling without making noise.
      const mute = this.ctx.createGain();
      mute.gain.value = 0;
      this.node.connect(mute).connect(this.ctx.destination);
      await this.configureEncoder();
    }

    async configureEncoder() {
      const rate = this.ctx.sampleRate;
      const base = { codec: 'opus', sampleRate: rate, numberOfChannels: 1, bitrate: 24000 };
      const wanted = { ...base, opus: { frameDuration: FRAME_US, application: 'voip', complexity: 5 } };
      let config = base;
      try {
        if ((await AudioEncoder.isConfigSupported(wanted)).supported) config = wanted;
      } catch { /* older Chrome: no opus-specific config */ }
      this.encoder = new AudioEncoder({
        output: (chunk) => {
          const buf = new ArrayBuffer(chunk.byteLength);
          chunk.copyTo(buf);
          this.onPacket(buf);
        },
        error: (e) => console.error('encoder', e),
      });
      this.encoder.configure(config);
      this.config = config;
    }

    onPCM(samples) {
      let peak = 0;
      for (let i = 0; i < samples.length; i += 8) peak = Math.max(peak, Math.abs(samples[i]));
      this.level = this.level * 0.8 + peak * 0.2;
      if (!this.active || this.encoder.state !== 'configured') return;
      const data = new AudioData({
        format: 'f32', sampleRate: this.ctx.sampleRate, numberOfFrames: samples.length,
        numberOfChannels: 1, timestamp: this.ts, data: samples,
      });
      this.ts += Math.round(samples.length * 1e6 / this.ctx.sampleRate);
      this.encoder.encode(data);
      data.close();
    }

    start() {
      if (this.ctx.state === 'suspended') this.ctx.resume();
      this.active = true;
    }

    // Stop capturing and flush the encoder so the tail of speech is sent.
    async stop() {
      this.active = false;
      if (this.encoder && this.encoder.state === 'configured') {
        try { await this.encoder.flush(); } catch { /* reset mid-flush */ }
      }
    }
  }

  // ---------- decode / play ----------
  class Player {
    constructor() {
      this.nextTime = 0;
      this.ts = 0;
      this.level = 0;
      this.reset();
    }

    reset() {
      if (this.decoder && this.decoder.state !== 'closed') {
        try { this.decoder.close(); } catch { /* already closed */ }
      }
      this.decoder = new AudioDecoder({
        output: (ad) => this.play(ad),
        error: (e) => { console.warn('decoder', e); this.reset(); },
      });
      this.decoder.configure({ codec: 'opus', sampleRate: 48000, numberOfChannels: 1 });
      this.nextTime = 0;
    }

    push(buf) {
      if (this.decoder.state !== 'configured') this.reset();
      this.decoder.decode(new EncodedAudioChunk({ type: 'key', timestamp: this.ts, data: buf }));
      this.ts += FRAME_US;
    }

    play(ad) {
      const ctx = ensurePlayback();
      const n = ad.numberOfFrames;
      const pcm = new Float32Array(n);
      ad.copyTo(pcm, { planeIndex: 0, format: 'f32-planar' });
      const buf = ctx.createBuffer(1, n, ad.sampleRate);
      buf.copyToChannel(pcm, 0);
      ad.close();
      let peak = 0;
      for (let i = 0; i < n; i += 16) peak = Math.max(peak, Math.abs(pcm[i]));
      this.level = this.level * 0.7 + peak * 0.3;

      const src = ctx.createBufferSource();
      src.buffer = buf;
      src.connect(playGain);
      const now = ctx.currentTime;
      // Underrun (start of a talk or network hiccup): re-prime a 120 ms buffer.
      if (this.nextTime < now + 0.02) this.nextTime = now + 0.12;
      src.start(this.nextTime);
      this.nextTime += buf.duration;
    }
  }

  return { supported, ensurePlayback, tones, speak, hush, Mic, Player, SAMPLE_RATE };
})();
