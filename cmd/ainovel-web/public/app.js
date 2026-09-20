const $ = s => document.querySelector(s);
const events = [];
let streamBuffer = '';
let modelData = null;
let currentBookId = 'default';
let configProviders = {};
let bookSwitchPromise = Promise.resolve();

function escapeHtml(v) {
  return String(v ?? '').replace(/[&<>]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' }[c]));
}

async function api(path, method = 'GET', body) {
  const opts = { method, headers: { 'Content-Type': 'application/json' } };
  if (body !== undefined) opts.body = JSON.stringify(body);
  let r;
  try {
    r = await fetch(path, opts);
  } catch (e) {
    showApiToast(`[API ERROR] ${path}\n${e.message}`, true);
    return { ok: false, error: e.message };
  }
  const text = await r.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
  if (!r.ok) {
    const error = (data && data.error) || text || r.statusText;
    showApiToast(`[API ${r.status}] ${path}\n${error}`, true);
    return { ok: false, error };
  }
  if (method !== 'GET') showApiToast(`[API ${r.status}] ${path}\n${apiResultText(data)}`);
  return { ok: true, data };
}

function setStatus(text) { $('#status').textContent = text; }

let toastTimer;
function showApiToast(message, error = false) {
  const toast = $('#apiToast');
  if (!toast) return;
  toast.textContent = message;
  toast.className = `show${error ? ' error' : ''}`;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toast.className = ''; }, 7000);
}

function apiResultText(data) {
  if (data == null || data === '') return 'empty response';
  if (typeof data === 'string') return data;
  return JSON.stringify(data);
}

function addEventLine(text, cls = '') {
  events.push({ text, cls });
  if (events.length > 300) events.splice(0, events.length - 300);
  renderEvents();
}

function renderEvents() {
  $('#events').innerHTML = events.length
    ? events.slice(-120).map(e => `<p class="${e.cls}">${escapeHtml(e.text)}</p>`).join('')
    : '<p class="empty">等待事件</p>';
  $('#eventCount').textContent = events.length;
}

function appendStream(delta) {
  streamBuffer += delta;
  $('#stream').innerHTML = `<pre>${escapeHtml(streamBuffer)}</pre>`;
}

function clearStream() {
  streamBuffer = '';
  $('#stream').innerHTML = '<p class="empty">等待创作输出</p>';
}

async function loadBooks() {
  const res = await api('/api/books');
  if (res.ok) {
    applyBooks(res.data);
    return res;
  }
  renderBooks({ active: currentBookId, books: [{ id: currentBookId, title: currentBookId, active: true }] });
  return res;
}

function applyBooks(data) {
  currentBookId = data.active || currentBookId;
  renderBooks(data);
}

function renderBooks(data) {
  const list = data.books || [];
  const sel = $('#bookSelect');
  sel.innerHTML = list.map(b => `<option value="${escapeHtml(b.id)}" ${b.id === data.active ? 'selected' : ''}>${escapeHtml(b.title || b.id)}${b.running ? ' (running)' : ''}</option>`).join('');
}

async function createBook() {
  const name = $('#newBookName').value.trim();
  if (!name) { $('#bookMsg').textContent = 'Book name is required'; return; }
  const res = await api('/api/books', 'POST', { name });
  if (!res.ok) { $('#bookMsg').textContent = res.error; return; }
  $('#newBookName').value = '';
  $('#bookMsg').textContent = 'Created';
  await applyBooks(res.data);
  await resetBookViews();
}

async function performBookSwitch() {
  const id = $('#bookSelect').value;
  if (!id) return;
  const res = await api('/api/books/switch', 'POST', { id });
  if (!res.ok) { $('#bookMsg').textContent = res.error; return; }
  $('#bookMsg').textContent = 'Switched';
  await applyBooks(res.data);
  await resetBookViews();
}

function switchBook() {
  bookSwitchPromise = bookSwitchPromise.then(() => performBookSwitch());
  return bookSwitchPromise;
}

async function resetBookViews() {
  events.length = 0;
  streamBuffer = '';
  renderEvents();
  clearStream();
  $('#cocreatePanel').hidden = true;
  $('#cocreateChat').innerHTML = '';
  $('#prompt').value = '';
  $('#cocreateInput').value = '';
  await refreshStatus();
  await refreshModels();
  await refreshSnapshot();
  await refreshChapters();
}

async function refreshChapters() {
  const res = await api('/api/chapters');
  const chapters = Array.isArray(res.data) ? res.data : [];
  $('#chapters').innerHTML = chapters.length
    ? chapters.map((c, i) => `<button class="chapter" data-i="${i}"><strong>${escapeHtml(c.name)}</strong><small>${c.content.length} 字 · ${new Date(c.modified).toLocaleString()}</small></button>`).join('')
    : '<p class="empty">还没有章节</p>';
  document.querySelectorAll('.chapter').forEach(b => b.onclick = () => {
    const c = chapters[Number(b.dataset.i)];
    $('#chapterTitle').textContent = c.name;
    $('#chapterContent').textContent = c.content;
    $('#chapterDialog').showModal();
  });
}

function fmt(v) { return v == null ? '—' : String(v); }

function renderSnapshot(snap) {
  if (!snap) { $('#snapshot').innerHTML = '<p class="empty">未连接引擎</p>'; return; }
  const agents = (snap.Agents || []).map(a => `<li><b>${escapeHtml(a.Name)}</b> ${escapeHtml(a.State)}${a.Tool ? ' · ' + escapeHtml(a.Tool) : ''}${a.Summary ? ' · ' + escapeHtml(a.Summary) : ''}</li>`).join('');
  const outline = (snap.Outline || []).slice(-20).map(o => `<li>第${o.Chapter}章 ${escapeHtml(o.Title || '')} ${o.CoreEvent ? '· ' + escapeHtml(o.CoreEvent) : ''}</li>`).join('');
  const chars = (snap.Characters || []).join('、');
  $('#snapshot').innerHTML = `
    <div class="snap-grid">
      <div><span>状态</span><b>${escapeHtml(snap.RuntimeState)}${snap.IsRunning ? ' · 运行中' : ''}</b></div>
      <div><span>书名</span><b>${escapeHtml(snap.BookTitle || '—')}</b></div>
      <div><span>模型</span><b>${escapeHtml(snap.ModelName)}</b></div>
      <div><span>进度</span><b>${snap.CompletedCount}/${snap.TotalChapters} 章 · ${snap.TotalWordCount} 字</b></div>
      <div><span>当前</span><b>第${snap.CurrentChapter}章 · ${escapeHtml(snap.Phase)}</b></div>
      <div><span>验收</span><b>${escapeHtml(snap.AdvanceMode)}${snap.PendingRewrites && snap.PendingRewrites.length ? ' · 待重写 ' + snap.PendingRewrites.join(',') : ''}</b></div>
      <div><span>用量</span><b>${snap.TotalInputTokens} in / ${snap.TotalOutputTokens} out · $${Number(snap.TotalCostUSD || 0).toFixed(4)}</b></div>
      <div><span>缓存命中</span><b>${snap.TotalCacheReadTokens} · 省 $${Number(snap.TotalSavedUSD || 0).toFixed(4)}</b></div>
    </div>
    <div class="snap-section"><h3>Agent</h3><ul>${agents || '<li class="muted">无</li>'}</ul></div>
    <div class="snap-section"><h3>大纲（最近）</h3><ul>${outline || '<li class="muted">无</li>'}</ul></div>
    <div class="snap-section"><h3>角色</h3><p>${escapeHtml(chars || '无')}</p></div>
    ${snap.CompassDirection ? `<div class="snap-section"><h3>指针</h3><p>${escapeHtml(snap.CompassDirection)} · ${escapeHtml(snap.CompassScale)}</p></div>` : ''}
  `;
}

async function refreshSnapshot() {
  const res = await api('/api/snapshot');
  if (res.ok) renderSnapshot(res.data && res.data.data);
}

async function refreshStatus() {
  const res = await api('/api/status');
  if (res.ok) {
    const d = res.data;
    setStatus(d.configured ? `运行中 · ${d.dir || ''}` : '未配置');
  }
}

async function refreshModels() {
  const res = await api('/api/engine/models');
  if (!res.ok) return;
  modelData = res.data.data;
  const providerSel = $('#providerSel');
  providerSel.innerHTML = (modelData.providers || []).map(p => `<option value="${escapeHtml(p)}">${escapeHtml(p)}</option>`).join('');
  const role = $('#roleSel').value;
  const roleInfo = modelData.roles[role] || {};
  if (roleInfo.provider) providerSel.value = roleInfo.provider;
  $('#modelInput').value = roleInfo.model || '';
  fillThinking(roleInfo);
  fillModelList(providerSel.value);
}

function fillModelList(provider) {
  const list = $('#modelList');
  const models = (modelData && modelData.provider_models && modelData.provider_models[provider]) || [];
  list.innerHTML = models.map(m => `<option value="${escapeHtml(m)}"></option>`).join('');
}

function fillThinking(roleInfo) {
  const sel = $('#thinkingSel');
  const levels = roleInfo.available_thinking || [];
  sel.innerHTML = levels.map(l => `<option value="${escapeHtml(l)}" ${l === roleInfo.thinking ? 'selected' : ''}>${escapeHtml(l || 'auto')}</option>`).join('');
}

async function loadConfig() {
  const res = await api('/api/config');
  if (!res.ok) return;
  const c = res.data;
  configProviders = c.provider_configs || {};
  $('#cfgProvider').innerHTML = Object.keys(configProviders).map(name => `<option value="${escapeHtml(name)}">${escapeHtml(name)}</option>`).join('');
  $('#cfgProvider').value = c.provider || '';
  $('#cfgModels').value = (c.models || []).join('\n');
  fillConfigModels(c.models || [], c.model || '');
  $('#cfgBaseUrl').value = c.base_url || '';
  $('#cfgApiKey').placeholder = c.api_key_set ? `已配置（${c.api_key_masked}，留空保持不变）` : 'sk-...';
  $('#configPath').textContent = c.path || '';
}

function fillConfigModels(models, selected) {
  const names = [...new Set((models || []).map(v => String(v).trim()).filter(Boolean))];
  if (selected && !names.includes(selected)) names.unshift(selected);
  $('#cfgModel').innerHTML = names.length
    ? names.map(name => `<option value="${escapeHtml(name)}">${escapeHtml(name)}</option>`).join('')
    : '<option value="">请先添加模型</option>';
  $('#cfgModel').value = selected || names[0] || '';
}

function selectConfigProvider(provider) {
  const info = configProviders[provider] || {};
  $('#cfgModels').value = (info.models || []).join('\n');
  fillConfigModels(info.models || [], '');
  $('#cfgBaseUrl').value = info.base_url || '';
  $('#cfgApiKey').value = '';
  $('#cfgApiKey').placeholder = info.api_key_set ? `已配置（${info.api_key_masked}，留空保持不变）` : 'sk-...';
}

async function saveConfig() {
  const msg = $('#configMsg');
  msg.textContent = '保存中…';
  const res = await api('/api/config', 'POST', {
    provider: $('#cfgProvider').value.trim(),
    api_key: $('#cfgApiKey').value.trim(),
    base_url: $('#cfgBaseUrl').value.trim(),
    model: $('#cfgModel').value.trim(),
    models: $('#cfgModels').value.split(/\r?\n/).map(v => v.trim()).filter(Boolean)
  });
  if (res.ok) {
    msg.textContent = res.data.engine_ready ? `已保存，引擎已就绪：${res.data.path}` : `已保存，但引擎未就绪：${res.data.engine_error || ''}`;
    $('#cfgApiKey').value = '';
    loadConfig();
    refreshStatus();
    refreshModels();
    refreshSnapshot();
  } else {
    msg.textContent = '保存失败：' + res.error;
  }
}

function connectSSE() {
  const source = new EventSource('/api/events');
  source.onmessage = e => {
    let msg;
    try { msg = JSON.parse(e.data); } catch (err) { return; }
    if (msg.book && currentBookId && msg.book !== currentBookId) return;
    switch (msg.type) {
      case 'event': {
        const ev = msg.data || {};
        const time = ev.Time ? new Date(ev.Time).toLocaleTimeString() : '';
        // Huabot 兼容模式返回完整消息，没有 token 级 SSE；把引擎进度同步到创作流。
        if (ev.Summary && ['DISPATCH', 'TOOL', 'ERROR', 'SYSTEM'].includes(ev.Category)) {
          appendStream(`${time ? `[${time}] ` : ''}[${ev.Category}] ${ev.Summary}${ev.Failed ? ' (失败)' : ''}\n`);
        }
        addEventLine(`[${ev.Category || 'EVENT'}] ${ev.Summary || ''}${ev.Failed ? ' (失败)' : ''}`, ev.Level === 'error' ? 'err' : '');
        break;
      }
      case 'stream': appendStream(msg.data || ''); break;
      case 'clear': clearStream(); break;
      case 'import': {
        const ev = msg.data || {};
        addEventLine(`[导入·${ev.Stage || ''}] ${ev.Message || ''}`, ev.Level === 'warn' ? 'warn' : '');
        break;
      }
      case 'sim': {
        const ev = msg.data || {};
        addEventLine(`[仿写·${ev.Stage || ''}] ${ev.Message || ''}`);
        break;
      }
      case 'cocreate_delta': {
        const ev = msg.data || {};
        if (ev.kind === 'reply') appendCocreate(ev.text);
        break;
      }
      case 'done': refreshSnapshot(); refreshChapters(); break;
      case 'import_done':
      case 'sim_done': addEventLine('后台任务结束'); refreshSnapshot(); refreshChapters(); break;
    }
  };
  return source;
}

function appendCocreate(text) {
  $('#cocreateChat').innerHTML = `<pre>${escapeHtml(text)}</pre>`;
}

async function applyModel() {
  const role = $('#roleSel').value;
  const provider = $('#providerSel').value;
  const model = $('#modelInput').value.trim();
  const thinking = $('#thinkingSel').value;
  if (provider) {
    const res = await api('/api/engine/model', 'POST', { role, provider, model });
    if (!res.ok) addEventLine('切模型失败：' + res.error, 'err');
  }
  if (thinking !== undefined && thinking !== '') {
    await api('/api/engine/thinking', 'POST', { role, level: thinking });
  }
  refreshModels();
}

function wireCocreate(stage) {
  $('#cocreatePanel').hidden = false;
  $('#cocreateChat').innerHTML = '';
  return api('/api/engine/cocreate/start', 'POST', { stage, initial: $('#prompt').value.trim() });
}

async function init() {
  await loadBooks();
  loadConfig();
  refreshStatus();
  refreshModels();
  refreshSnapshot();
  refreshChapters();
  connectSSE();

  $('#saveConfig').onclick = saveConfig;
  $('#cfgProvider').onchange = () => selectConfigProvider($('#cfgProvider').value);
  $('#cfgModels').oninput = () => fillConfigModels($('#cfgModels').value.split(/\r?\n/), $('#cfgModel').value);
  $('#refresh').onclick = refreshChapters;
  $('#close').onclick = () => $('#chapterDialog').close();
  $('#bookSelect').onchange = switchBook;
  $('#switchBook').onclick = switchBook;
  $('#createBook').onclick = createBook;
  $('#roleSel').onchange = refreshModels;
  $('#providerSel').onchange = () => fillModelList($('#providerSel').value);
  $('#applyModel').onclick = applyModel;

  $('#start').onclick = async () => {
    const prompt = $('#prompt').value.trim();
    if (!prompt) return;
    const button = $('#start');
    button.disabled = true;
    button.textContent = '创作请求处理中...';
    showApiToast('[API REQUEST] /api/engine/start\nrequest sent, waiting for engine...');
    await bookSwitchPromise;
    const res = await api('/api/engine/start', 'POST', { prompt });
    if (!res.ok) addEventLine('启动失败：' + res.error, 'err');
    refreshSnapshot();
    button.disabled = false;
    button.textContent = '开始创作';
  };
  $('#resume').onclick = async () => {
    const res = await api('/api/engine/resume', 'POST');
    if (!res.ok) addEventLine('恢复失败：' + res.error, 'err');
    refreshSnapshot();
  };
  $('#steerBtn').onclick = async () => {
    const text = $('#steerInput').value.trim();
    if (!text) return;
    const res = await api('/api/engine/steer', 'POST', { text });
    if (!res.ok) addEventLine('干预失败：' + res.error, 'err');
    $('#steerInput').value = '';
  };
  $('#continueBtn').onclick = async () => {
    const text = $('#steerInput').value.trim();
    if (!text) return;
    const res = await api('/api/engine/continue', 'POST', { text });
    if (!res.ok) addEventLine('继续失败：' + res.error, 'err');
    $('#steerInput').value = '';
  };
  $('#reviewBtn').onclick = async () => {
    const res = await api('/api/engine/review', 'POST', { mode: 'on' });
    if (!res.ok) addEventLine('切换验收失败：' + res.error, 'err');
    refreshSnapshot();
  };
  $('#nextBtn').onclick = async () => {
    const res = await api('/api/engine/next', 'POST');
    if (!res.ok) addEventLine('放行失败：' + res.error, 'err');
    refreshSnapshot();
  };
  $('#stopBtn').onclick = async () => {
    const res = await api('/api/engine/stop', 'POST');
    if (!res.ok) addEventLine('停止失败：' + res.error, 'err');
  };
  $('#reopenBtn').onclick = async () => {
    const res = await api('/api/engine/reopen', 'POST', { direction: $('#reopenInput').value.trim() });
    if (!res.ok) addEventLine('重开失败：' + res.error, 'err');
  };

  $('#importBtn').onclick = async () => {
    const res = await api('/api/engine/import', 'POST', {
      path: $('#importPath').value.trim(),
      guidance: $('#importGuide').value.trim(),
      auto_confirm: $('#importAuto').checked,
      continue_after: $('#importContinue').checked
    });
    if (!res.ok) addEventLine('导入失败：' + res.error, 'err');
  };
  $('#exportBtn').onclick = async () => {
    const res = await api('/api/engine/export', 'POST', {
      path: $('#exportPath').value.trim(),
      format: $('#exportFormat').value,
      from: Number($('#exportFrom').value) || 0,
      to: Number($('#exportTo').value) || 0,
      overwrite: $('#exportOverwrite').checked
    });
    if (res.ok) addEventLine('导出完成：' + JSON.stringify(res.data.data || res.data), 'ok');
    else addEventLine('导出失败：' + res.error, 'err');
  };
  $('#syncBtn').onclick = async () => {
    const res = await api('/api/engine/sync', 'POST', { check: false });
    if (!res.ok) addEventLine('同步失败：' + res.error, 'err');
    else addEventLine('同步完成', 'ok');
  };
  $('#syncCheckBtn').onclick = async () => {
    const res = await api('/api/engine/sync', 'POST', { check: true });
    if (res.ok) addEventLine('有外部修改的章节：' + JSON.stringify(res.data.data), 'warn');
    else addEventLine('检查失败：' + res.error, 'err');
  };
  $('#diagBtn').onclick = async () => {
    const res = await api('/api/engine/diag', 'POST');
    if (res.ok) addEventLine('诊断报告：' + JSON.stringify(res.data.data), 'ok');
    else addEventLine('诊断失败：' + res.error, 'err');
  };
  $('#simulateBtn').onclick = async () => {
    const res = await api('/api/engine/simulate', 'POST');
    if (!res.ok) addEventLine('仿写失败：' + res.error, 'err');
  };
  $('#importsimBtn').onclick = async () => {
    const res = await api('/api/engine/importsim', 'POST', { path: $('#importsimPath').value.trim() });
    if (!res.ok) addEventLine('导入画像失败：' + res.error, 'err');
  };

  $('#cocreateOpen').onclick = async () => {
    const res = await wireCocreate(false);
    if (!res.ok) addEventLine('共创启动失败：' + res.error, 'err');
  };
  $('#stageCocreate').onclick = async () => {
    const res = await wireCocreate(true);
    if (!res.ok) addEventLine('阶段共创失败：' + res.error, 'err');
    else addEventLine('已暂停创作，进入阶段共创');
  };
  $('#cocreateSend').onclick = async () => {
    const res = await api('/api/engine/cocreate/send', 'POST', { text: $('#cocreateInput').value.trim() });
    if (res.ok) {
      const d = res.data.data || {};
      appendCocreate(d.message || '');
      $('#cocreateInput').value = '';
      if (d.prompt) $('#prompt').value = d.prompt;
    } else {
      addEventLine('共创失败：' + res.error, 'err');
    }
  };
  $('#cocreateApply').onclick = async () => {
    const res = await api('/api/engine/cocreate/apply', 'POST', { draft: $('#prompt').value.trim() });
    if (!res.ok) addEventLine('应用失败：' + res.error, 'err');
    else { $('#cocreatePanel').hidden = true; refreshSnapshot(); }
  };
  $('#cocreateCancel').onclick = async () => {
    await api('/api/engine/cocreate/cancel', 'POST');
    $('#cocreatePanel').hidden = true;
  };
}

init();
