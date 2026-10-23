// Browser walkie-talkie: speaks the same WebSocket protocol as the ESP32
// firmware (docs/protocol.md).
'use strict';

const $ = (id) => document.getElementById(id);

const params = new URLSearchParams(location.search);
// sessionStorage is per tab, so every tab is its own device.
const deviceId = params.get('device') || sessionStorage.getItem('ptt.device') ||
  'web-' + Math.random().toString(36).slice(2, 8);
sessionStorage.setItem('ptt.device', deviceId);

const state = {
  ws: null,
  device: null,     // DeviceStatus from the API
  current: null,    // GroupStatus of the current channel
  talking: false,   // we hold the floor
  pressing: false,  // PTT physically held
  incoming: null,   // talk_start of the stream we're playing
  talkStarted: 0,
  mic: null,
  player: null,
  announced: false,
};

// ---------- helpers ----------
async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: { 'Content-Type': 'application/json', 'X-Device-Id': deviceId },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) throw Object.assign(new Error((data && data.error) || res.statusText), { status: res.status, data });
  return data;
}

function log(msg, cls = '') {
  const li = document.createElement('li');
  li.className = cls;
  li.textContent = `${new Date().toLocaleTimeString('zh-CN', { hour12: false })} ${msg}`;
  $('log').prepend(li);
  while ($('log').children.length > 60) $('log').lastChild.remove();
}

function send(msg) {
  if (state.ws && state.ws.readyState === WebSocket.OPEN) state.ws.send(JSON.stringify(msg));
}

function announce(text) {
  if (!text) return;
  log('🔊 ' + text);
  if (!state.incoming) PTTAudio.speak(text);
}

const TYPE_LABEL = { temp: '临时群', fixed: '固定群', emergency: '紧急频道' };

// ---------- rendering ----------
function render() {
  const d = state.device;
  $('dev-name').textContent = (d && d.name) || deviceId;
  $('dev-id').textContent = deviceId;
  const g = state.current;
  $('lcd').classList.toggle('emergency', !!(g && g.type === 'emergency'));
  $('lcd-channel').textContent = g ? g.name : '未加入群';
  $('lcd-code').textContent = g ? `房间 ${g.code}` : '';
  $('lcd-online').textContent = g ? `${g.online_count}人在线 / ${g.members.length}人` : '';
  $('lcd-type').textContent = g ? TYPE_LABEL[g.type] || '' : '';
  const muted = g && d && d.muted_until && d.muted_until[g.id] && new Date(d.muted_until[g.id]) > new Date();
  const leaderOnly = g && d && d.leader_only && d.leader_only[g.id];
  const isLeader = g && g.members.some((m) => m.device_id === deviceId && m.role === 'leader');
  $('lcd-flags').textContent = [isLeader ? '★队长' : '', muted ? '🔇静音' : '', leaderOnly ? '只听队长' : ''].filter(Boolean).join(' ');
  $('btn-mute').classList.toggle('on', !!muted);
  $('btn-mute').textContent = muted ? '取消静音' : '静音';
  $('btn-leader').classList.toggle('on', !!leaderOnly);
  $('ptt').disabled = !g || !state.ws || state.ws.readyState !== WebSocket.OPEN;

  const list = $('group-list');
  list.innerHTML = '';
  for (const it of (d && d.group_list) || []) {
    const li = document.createElement('li');
    li.className = it.id === (g && g.id) ? 'current' : '';
    li.innerHTML = `<span class="g-name"></span><span class="g-meta"></span>`;
    li.querySelector('.g-name').textContent = (it.type === 'emergency' ? '🚨 ' : '') + it.name;
    li.querySelector('.g-meta').textContent = `${it.code} · ${it.online_count}人在线`;
    li.onclick = () => send({ type: 'switch', group_id: it.id });
    list.append(li);
  }
  if (!list.children.length) list.innerHTML = '<li class="empty">还没有加入群：输入房间号加入，或新建一个</li>';
  renderStatus();
}

function renderStatus() {
  const el = $('lcd-status');
  const ptt = $('ptt');
  ptt.classList.toggle('talking', state.talking);
  ptt.classList.toggle('busy', !!state.incoming && !state.talking);
  if (state.talking) {
    const s = Math.floor((Date.now() - state.talkStarted) / 1000);
    el.textContent = `● 发射中 ${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`;
    el.className = 'lcd-status tx';
  } else if (state.incoming) {
    const m = state.incoming;
    const where = state.current && m.group_id !== state.current.id ? `[${m.group_name}] ` : '';
    el.textContent = `${m.replay ? '⟲ 回放 ' : '◀ '}${where}${m.speaker_name}${m.leader ? '（队长）' : ''} 正在讲话`;
    el.className = 'lcd-status rx' + (m.priority >= 3 ? ' sos' : '');
  } else {
    el.textContent = '待机';
    el.className = 'lcd-status';
  }
}
setInterval(() => { if (state.talking) renderStatus(); }, 500);

async function refreshDevice() {
  try {
    state.device = await api('GET', `/api/devices/${encodeURIComponent(deviceId)}`);
    state.current = state.device.current || null;
  } catch (e) {
    log('刷新失败：' + e.message, 'err');
  }
  render();
}

// ---------- websocket ----------
function connect() {
  const name = localStorage.getItem('ptt.name.' + deviceId) || '';
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${proto}://${location.host}/ws?device_id=${encodeURIComponent(deviceId)}&name=${encodeURIComponent(name)}`);
  ws.binaryType = 'arraybuffer';
  state.ws = ws;
  ws.onopen = () => { $('conn-dot').className = 'dot on'; log('已连接'); };
  ws.onclose = () => {
    $('conn-dot').className = 'dot';
    if (state.talking) stopTalk(false);
    state.incoming = null;
    render();
    log('连接断开，2 秒后重连', 'err');
    setTimeout(connect, 2000);
  };
  ws.onmessage = (ev) => {
    if (typeof ev.data !== 'string') {
      if (state.player) state.player.push(ev.data);
      return;
    }
    onMessage(JSON.parse(ev.data));
  };
}

function onMessage(m) {
  switch (m.type) {
    case 'welcome':
      state.device = m.device;
      state.current = m.device.current || null;
      render();
      if (!state.announced) { announce(m.announce); state.announced = true; }
      break;
    case 'channel':
      state.current = m.group;
      if (state.talking) stopTalk(false);
      PTTAudio.tones.click();
      announce(m.announce);
      refreshDevice();
      break;
    case 'presence':
      if (state.current && m.group_id === state.current.id) {
        state.current.online_count = m.online_count;
        render();
      }
      break;
    case 'floor':
      if (m.granted) {
        if (!state.pressing) { send({ type: 'ptt_stop' }); break; } // released before the grant arrived
        state.talking = true;
        state.talkStarted = Date.now();
        state.mic.start();
        PTTAudio.tones.permit();
        startRecognition(m.utterance_id);
      } else {
        PTTAudio.tones.deny();
        log(m.reason === 'busy' ? `抢麦失败：${m.holder_name} 正在讲话` : '当前没有群，无法讲话', 'warn');
      }
      renderStatus();
      break;
    case 'floor_revoked': {
      const why = { preempted: `${m.holder_name || '队长'} 插话`, max_duration: '超过最长讲话时间', idle: '长时间无声音' }[m.reason] || m.reason;
      log('话权被收回：' + why, 'warn');
      stopTalk(false);
      PTTAudio.tones.deny();
      break;
    }
    case 'talk_start':
      PTTAudio.hush(); // talk interrupts AI / TTS announcements
      state.incoming = m;
      if (state.player) state.player.reset();
      log(`${m.replay ? '回放' : '收到'}：${m.speaker_name}${m.group_id !== (state.current && state.current.id) ? ' @' + m.group_name : ''}`);
      renderStatus();
      break;
    case 'talk_stop':
      if (state.incoming && state.incoming.utterance_id === m.utterance_id) {
        state.incoming = null;
        if (m.reason !== 'preempted') PTTAudio.tones.roger();
      }
      renderStatus();
      break;
    case 'transcript': {
      const mine = m.speaker_id === deviceId;
      log(`💬 ${mine ? '我' : m.speaker_name}：${m.text}`, m.alert && m.alert.length ? 'warn' : '');
      if (!mine) showSubtitle(m);
      break;
    }
    case 'notice':
      if (m.level === 'alert') {
        PTTAudio.tones.alert();
        setTimeout(() => announce(m.text), 900);
      } else {
        announce(m.text);
      }
      if (m.text.includes('解散')) refreshDevice();
      break;
    case 'error':
      log('⚠ ' + m.message + (m.candidates ? '：' + m.candidates.join('、') : ''), 'err');
      PTTAudio.speak(m.message);
      break;
  }
}

// ---------- PTT ----------
async function pressPTT() {
  if (state.pressing || $('ptt').disabled) return;
  if (!state.mic) {
    PTTAudio.tones.deny();
    log('当前浏览器不支持 Opus 编码，无法讲话', 'err');
    return;
  }
  state.pressing = true;
  $('ptt').classList.add('pressed');
  try {
    await state.mic.open();
  } catch (e) {
    log('无法打开麦克风：' + e.message, 'err');
    state.pressing = false;
    $('ptt').classList.remove('pressed');
    return;
  }
  if (!state.pressing) return; // released while the permission prompt was up
  send({ type: 'ptt_start' });
}

async function releasePTT() {
  if (!state.pressing) return;
  state.pressing = false;
  $('ptt').classList.remove('pressed');
  if (state.talking) await stopTalk(true);
}

async function stopTalk(notify) {
  state.talking = false;
  stopRecognition();
  renderStatus();
  if (state.mic) await state.mic.stop();
  if (notify) send({ type: 'ptt_stop' });
}

function bindPTT() {
  const btn = $('ptt');
  btn.addEventListener('pointerdown', (e) => { e.preventDefault(); btn.setPointerCapture(e.pointerId); pressPTT(); });
  btn.addEventListener('pointerup', releasePTT);
  btn.addEventListener('pointercancel', releasePTT);
  btn.addEventListener('contextmenu', (e) => e.preventDefault());
  const typing = () => ['INPUT', 'SELECT', 'TEXTAREA'].includes(document.activeElement.tagName);
  window.addEventListener('keydown', (e) => {
    if (e.code === 'Space' && !e.repeat && !typing()) { e.preventDefault(); pressPTT(); }
  });
  window.addEventListener('keyup', (e) => {
    if (e.code === 'Space' && !typing()) { e.preventDefault(); releasePTT(); }
  });
  window.addEventListener('blur', releasePTT);
}

// ---------- client-side transcription ----------
// When the server has no ASR configured it accepts the speaker's own
// transcript; Chrome's Web Speech API gives us one for free.
const Recognition = window.SpeechRecognition || window.webkitSpeechRecognition;
let recog = null;

function startRecognition(utteranceId) {
  if (!Recognition || !utteranceId) return;
  const r = new Recognition();
  r.lang = 'zh-CN';
  r.continuous = true;
  r.interimResults = false;
  const parts = [];
  r.onresult = (e) => {
    for (let i = e.resultIndex; i < e.results.length; i++) {
      if (e.results[i].isFinal) parts.push(e.results[i][0].transcript);
    }
  };
  r.onerror = () => {};
  r.onend = () => {
    const text = parts.join('，').trim();
    if (text) send({ type: 'transcript', utterance_id: utteranceId, text });
  };
  try {
    r.start();
    recog = r;
  } catch { /* already running or blocked */ }
}

function stopRecognition() {
  if (!recog) return;
  // Give the recogniser a moment to finalise the last words.
  const r = recog;
  recog = null;
  setTimeout(() => { try { r.stop(); } catch { /* ended */ } }, 400);
}

let subtitleTimer = 0;
function showSubtitle(m) {
  const el = $('lcd-status');
  clearTimeout(subtitleTimer);
  el.textContent = `💬 ${m.speaker_name}：${m.text}`;
  el.className = 'lcd-status sub' + (m.alert && m.alert.length ? ' sos' : '');
  subtitleTimer = setTimeout(renderStatus, 6000);
}

// ---------- controls ----------
function bindControls() {
  $('prev').onclick = () => send({ type: 'switch', delta: -1 });
  $('next').onclick = () => send({ type: 'switch', delta: 1 });

  $('btn-replay').onclick = async () => {
    try {
      const recs = await api('POST', `/api/devices/${encodeURIComponent(deviceId)}/replay`, { n: 1 });
      log(`回放 ${recs.length} 条`);
    } catch (e) {
      PTTAudio.tones.deny();
      log(e.status === 409 ? '没有可以回放的语音' : '回放失败：' + e.message, 'warn');
    }
  };

  $('btn-summary').onclick = async () => {
    const g = state.current;
    if (!g) return;
    try {
      const sum = await api('GET', `/api/groups/${g.id}/summary?minutes=30&device_id=${encodeURIComponent(deviceId)}`);
      log('📝 ' + sum.text);
      PTTAudio.speak(sum.text);
    } catch (e) {
      log('摘要失败：' + e.message, 'err');
    }
  };

  $('btn-mute').onclick = async () => {
    const g = state.current;
    if (!g) return;
    const muted = $('btn-mute').classList.contains('on');
    await api('POST', `/api/groups/${g.id}/mute`, { device_id: deviceId, minutes: muted ? 0 : 60 });
    announce(muted ? '已取消静音' : `${g.name}已静音一小时`);
    refreshDevice();
  };

  $('btn-leader').onclick = async () => {
    const g = state.current;
    if (!g) return;
    const on = !$('btn-leader').classList.contains('on');
    await api('POST', `/api/groups/${g.id}/leader-only`, { device_id: deviceId, on });
    announce(on ? '只听车队长' : '恢复收听所有人');
    refreshDevice();
  };

  $('btn-locate').onclick = () => {
    if (!navigator.geolocation) return log('浏览器不支持定位', 'err');
    navigator.geolocation.getCurrentPosition(async (pos) => {
      const { latitude: lat, longitude: lng } = pos.coords;
      send({ type: 'location', lat, lng });
      log(`已上报位置 ${lat.toFixed(4)}, ${lng.toFixed(4)}`);
      const list = await api('GET', `/api/nearby?lat=${lat}&lng=${lng}`);
      const ul = $('nearby-list');
      ul.innerHTML = '';
      for (const g of list) {
        const li = document.createElement('li');
        li.innerHTML = '<span class="g-name"></span><span class="g-meta"></span>';
        li.querySelector('.g-name').textContent = g.name;
        li.querySelector('.g-meta').textContent = `${g.distance_km.toFixed(1)}km · ${g.members.length}人`;
        li.onclick = () => joinCode(g.code);
        ul.append(li);
      }
      if (!list.length) ul.innerHTML = '<li class="empty">附近没有群，可以建一个「附近可见」的群</li>';
      $('nearby-box').hidden = false;
      $('nearby-box').closest('details').open = true;
    }, (err) => log('定位失败：' + err.message, 'err'), { enableHighAccuracy: true, timeout: 10000 });
  };

  $('join-form').onsubmit = (e) => {
    e.preventDefault();
    const code = $('join-code').value.trim();
    if (code) joinCode(code);
  };

  $('create-vis').onchange = () => {
    const v = $('create-vis').value;
    $('create-route').hidden = v !== 'route';
    if (v === 'nearby') $('create-type').value = 'temp';
  };

  $('create-form').onsubmit = async (e) => {
    e.preventDefault();
    const name = $('create-name').value.trim();
    if (!name) return;
    const body = { name, owner_id: deviceId, type: $('create-type').value, visibility: $('create-vis').value, route: $('create-route').value };
    try {
      const g = await api('POST', '/api/groups', body);
      $('create-name').value = '';
      log(`已创建 ${g.name}，房间号 ${g.code}`);
    } catch (err) {
      log('建群失败：' + err.message + (err.status === 400 && body.visibility === 'nearby' ? '（先点「定位」）' : ''), 'err');
    }
  };

  $('dev-name').onclick = async () => {
    const name = prompt('设备名称', state.device ? state.device.name : '');
    if (!name) return;
    localStorage.setItem('ptt.name.' + deviceId, name);
    await api('PUT', `/api/devices/${encodeURIComponent(deviceId)}`, { name });
    refreshDevice();
  };
}

async function joinCode(code) {
  try {
    const g = await api('POST', `/api/codes/${encodeURIComponent(code)}/join`, { device_id: deviceId });
    $('join-code').value = '';
    log(`已加入 ${g.name}`);
  } catch (e) {
    PTTAudio.tones.deny();
    log(e.status === 404 ? `房间号 ${code} 不存在` : '入群失败：' + e.message, 'err');
  }
}

// ---------- boot ----------
function boot() {
  $('dev-id').textContent = deviceId;
  $('power-name').value = localStorage.getItem('ptt.name.' + deviceId) || params.get('name') || '';
  if (!PTTAudio.supported) {
    $('codec-hint').textContent = '⚠ 当前浏览器不支持 WebCodecs Opus，只能控制不能通话。请使用新版 Chrome / Edge。';
  }
  $('power-on').onclick = async () => {
    const name = $('power-name').value.trim();
    if (name) localStorage.setItem('ptt.name.' + deviceId, name);
    PTTAudio.ensurePlayback(); // must happen in a user gesture
    PTTAudio.tones.permit();
    if (PTTAudio.supported) {
      state.player = new PTTAudio.Player();
      state.mic = new PTTAudio.Mic((buf) => {
        if (state.ws && state.ws.readyState === WebSocket.OPEN) state.ws.send(buf);
      });
      // Open the mic now (inside the user gesture) so the first PTT press is instant.
      state.mic.open().then(
        () => log(`麦克风就绪（Opus ${state.mic.ctx.sampleRate / 1000}kHz${state.mic.config.opus ? ' / 60ms' : ''}）`),
        (e) => log('麦克风未授权：' + e.message + '，按下 PTT 时会再次请求', 'warn'),
      );
    }
    $('power').hidden = true;
    connect();
    const pending = params.get('join');
    if (pending) setTimeout(() => joinCode(pending), 500);
  };
  bindPTT();
  bindControls();
  render();
}

boot();
