'use strict';

const $ = (id) => document.getElementById(id);
const code = decodeURIComponent(location.pathname.split('/').pop());
const DIGITS = { 0: '零', 2: '二', 3: '三', 4: '四', 5: '五', 6: '六', 8: '八', 9: '九', 1: '幺', 7: '拐' };
const TYPE = { temp: '临时群', fixed: '固定群', emergency: '紧急频道' };

async function load() {
  const res = await fetch(`/api/codes/${encodeURIComponent(code)}`);
  if (!res.ok) {
    $('g-name').textContent = '群不存在或已解散';
    document.querySelectorAll('.join-opt, .join-code, .join-tip').forEach((e) => (e.hidden = true));
    return;
  }
  const g = await res.json();
  document.title = `加入「${g.name}」`;
  $('g-name').textContent = g.name;
  $('g-meta').textContent = `${TYPE[g.type] || ''} · ${g.members.length} 位成员 · ${g.online_count} 人在线`;
  $('g-code').textContent = g.code;
  $('g-say').textContent = '加入房间' + [...g.code].map((d) => DIGITS[d]).join('');
  $('use-browser').href = `/device?join=${encodeURIComponent(g.code)}`;
}

$('dev-id').value = localStorage.getItem('ptt.bound.device') || '';
$('dev-name').value = localStorage.getItem('ptt.bound.name') || '';

$('bind').onsubmit = async (e) => {
  e.preventDefault();
  const device_id = $('dev-id').value.trim();
  const name = $('dev-name').value.trim();
  const res = await fetch(`/api/codes/${encodeURIComponent(code)}/join`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ device_id, name }),
  });
  const data = await res.json();
  if (!res.ok) {
    $('result').textContent = '加入失败：' + data.error;
    $('result').className = 'join-result err';
    return;
  }
  localStorage.setItem('ptt.bound.device', device_id);
  if (name) localStorage.setItem('ptt.bound.name', name);
  const me = data.members.find((m) => m.device_id === device_id);
  $('result').textContent = me && me.online
    ? `✅ 已加入「${data.name}」，对讲机已切到这个频道，按住 PTT 就能说话。`
    : `✅ 已加入「${data.name}」。对讲机开机后会自动进入这个频道。`;
  $('result').className = 'join-result ok';
};

load();
