'use strict';

const $ = (sel, root = document) => root.querySelector(sel);

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) e.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) if (c != null) e.append(c.nodeType ? c : document.createTextNode(c));
  return e;
}

const TYPE = { temp: '临时群', fixed: '固定群', emergency: '紧急频道' };
const VIS = { code: '房间号', nearby: '附近可见', route: '路线可见' };

function ago(iso) {
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 60) return '刚刚';
  if (s < 3600) return `${Math.floor(s / 60)}分钟前`;
  if (s < 86400) return `${Math.floor(s / 3600)}小时前`;
  return new Date(iso).toLocaleString('zh-CN');
}

let groups = [];
let busy = false;

async function refresh() {
  if (busy) return;
  try {
    groups = await api('GET', '/api/groups');
  } catch {
    return;
  }
  render();
}

function render() {
  const online = new Set();
  let speaking = 0;
  for (const g of groups) {
    for (const m of g.members) if (m.online) online.add(m.device_id);
    if (g.speaker) speaking++;
  }
  $('#stats').textContent = `${groups.length} 个群 · ${online.size} 台设备在线 · ${speaking} 个群正在通话`;

  const sel = $('#batch select[name=group]');
  const keep = sel.value;
  sel.innerHTML = '';
  for (const g of groups) sel.append(el('option', { value: g.id }, `${g.name}（${g.code}）`));
  if (keep) sel.value = keep;

  const box = $('#groups');
  // Preserve open state of member lists across re-renders.
  const open = new Set([...box.querySelectorAll('details[open]')].map((d) => d.dataset.id));
  box.innerHTML = '';
  if (!groups.length) {
    box.append(el('p', { class: 'empty' }, '还没有群。左侧新建一个，或在模拟对讲机里建群。'));
    return;
  }
  for (const g of [...groups].reverse()) box.append(groupCard(g, open.has(g.id)));
}

function groupCard(g, open) {
  const speaker = g.members.find((m) => m.speaking);
  const card = el('article', { class: 'card group-card' + (g.type === 'emergency' ? ' emergency' : '') + (speaker ? ' live' : '') },
    el('div', { class: 'gc-main' },
      el('div', { class: 'gc-title' },
        el('h3', {}, g.name),
        el('span', { class: 'tag' }, TYPE[g.type]),
        tplName(g.template) ? el('span', { class: 'tag' }, tplName(g.template) + '模板') : null,
        g.talk_policy === 'leaders' ? el('span', { class: 'tag warn' }, '仅队长可讲') : null,
        el('span', { class: 'tag' }, VIS[g.visibility]),
        g.route ? el('span', { class: 'tag' }, g.route) : null,
      ),
      el('div', { class: 'gc-code' }, '房间号 ', el('b', {}, g.code)),
      el('div', { class: 'gc-meta' },
        `${g.online_count} 人收听 / ${g.members.length} 人 · 最近活跃 ${ago(g.last_active_at)}`),
      speaker ? el('div', { class: 'gc-live' }, `🔴 ${speaker.name} 正在讲话`) : null,
      el('details', { 'data-id': g.id, open },
        el('summary', {}, '成员'),
        el('ul', { class: 'members' }, g.members.map((m) => memberRow(g, m))),
      ),
      el('div', { class: 'gc-actions' },
        el('a', { class: 'btn small', href: g.share_url, target: '_blank' }, '入群页'),
        el('button', { class: 'btn small', onclick: () => navigator.clipboard.writeText(g.share_url).then(() => toast('链接已复制')) }, '复制链接'),
        el('button', { class: 'btn small', onclick: () => showRecordings(g) }, '发言记录'),
        el('button', { class: 'btn small danger', onclick: () => dissolve(g) }, '解散'),
      ),
    ),
    el('a', { class: 'gc-qr', href: `/api/groups/${g.id}/qr.png`, target: '_blank', title: '扫码入群' },
      el('img', { src: `/api/groups/${g.id}/qr.png`, alt: '入群二维码', loading: 'lazy' })),
  );
  return card;
}

function tplName(id) {
  const t = templates.find((x) => x.id === id);
  return t ? t.name : '';
}

function memberRow(g, m) {
  const state = m.speaking ? '讲话中' : m.tuned ? '收听中' : m.online ? '在线（其他频道）' : '离线';
  const cls = m.speaking ? 'speaking' : m.tuned ? 'tuned' : m.online ? 'online' : 'offline';
  const leader = m.role === 'leader';
  return el('li', { class: cls },
    el('span', { class: 'm-dot' }),
    el('span', { class: 'm-name' }, (leader ? '★ ' : '') + (m.name || m.device_id)),
    el('span', { class: 'm-id' }, m.device_id),
    el('span', { class: 'm-state' }, state),
    el('button', {
      class: 'btn small',
      onclick: () => setRole(g, m, leader ? 'member' : 'leader'),
    }, leader ? '取消队长' : '设为队长'),
  );
}

async function setRole(g, m, role) {
  await act(() => api('PUT', `/api/groups/${g.id}/members/${encodeURIComponent(m.device_id)}/role`, { role }));
}

async function dissolve(g) {
  if (!confirm(`确定解散「${g.name}」？成员设备会收到通知。`)) return;
  await act(() => api('DELETE', `/api/groups/${g.id}`));
}

async function showRecordings(g) {
  const recs = await api('GET', `/api/groups/${g.id}/recordings?limit=30`);
  $('#rec-title').textContent = `${g.name} · 最近发言`;
  const list = $('#rec-list');
  list.innerHTML = '';
  if (!recs.length) list.append(el('li', { class: 'empty' }, '暂无发言'));
  for (const r of recs) {
    list.append(el('li', { class: r.alert && r.alert.length ? 'alert' : '' },
      el('div', { class: 'rec-head' },
        el('b', {}, r.speaker_name + (r.leader ? '（队长）' : '')),
        el('span', {}, `${new Date(r.started_at).toLocaleTimeString('zh-CN', { hour12: false })} · ${(r.duration_ms / 1000).toFixed(1)}s`),
        r.alert && r.alert.length ? el('span', { class: 'tag warn' }, '⚠ ' + r.alert.join('、')) : null,
      ),
      r.transcript ? el('p', { class: 'rec-text' }, r.transcript) : null,
      el('audio', { controls: true, preload: 'none', src: `/api/recordings/${r.id}/audio.ogg` }),
    ));
  }
  $('#rec-dialog').showModal();
}

async function act(fn) {
  busy = true;
  try {
    await fn();
  } catch (e) {
    toast('操作失败：' + e.message);
  } finally {
    busy = false;
    refresh();
  }
}

function toast(msg) {
  const t = el('div', { class: 'toast' }, msg);
  document.body.append(t);
  setTimeout(() => t.remove(), 2500);
}

// ---------- forms ----------
const createForm = $('#create');
createForm.visibility.onchange = () => {
  const v = createForm.visibility.value;
  createForm.route.hidden = v !== 'route';
  $('[data-geo]', createForm).hidden = v !== 'nearby';
  if (v === 'nearby') createForm.type.value = 'temp';
};
createForm.onsubmit = (e) => {
  e.preventDefault();
  const f = createForm;
  const body = { name: f.name.value, template: f.template.value, type: f.type.value, visibility: f.visibility.value, route: f.route.value, owner_id: f.owner_id.value.trim() };
  if (body.visibility === 'nearby') {
    body.lat = parseFloat(f.lat.value);
    body.lng = parseFloat(f.lng.value);
    if (Number.isNaN(body.lat) || Number.isNaN(body.lng)) return toast('附近群需要填写坐标');
  }
  act(async () => {
    const g = await api('POST', '/api/groups', body);
    toast(`已创建「${g.name}」，房间号 ${g.code}`);
    f.reset();
    f.visibility.onchange();
  });
};

$('#batch').onsubmit = (e) => {
  e.preventDefault();
  const f = e.target;
  const devices = f.devices.value.split('\n').map((l) => l.trim()).filter(Boolean).map((l) => {
    const [device_id, ...rest] = l.split(/[,，\t]/);
    return { device_id: device_id.trim(), name: rest.join(',').trim() };
  });
  if (!devices.length) return toast('请填写设备');
  act(async () => {
    const r = await api('POST', `/api/groups/${f.group.value}/members/batch`, { devices, pin: f.pin.checked });
    toast(`导入 ${r.joined.length} 台${r.failed.length ? `，失败 ${r.failed.length} 台` : ''}`);
    f.devices.value = '';
  });
};

let templates = [];
fetch('/api/templates').then((r) => r.json()).then((list) => {
  templates = list;
  for (const t of list) createForm.template.append(el('option', { value: t.id }, `${t.name} — ${t.description}`));
  createForm.template.onchange = () => {
    const t = list.find((x) => x.id === createForm.template.value);
    if (t) createForm.type.value = t.type;
  };
});

refresh();
setInterval(refresh, 2000);
