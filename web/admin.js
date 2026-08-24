'use strict';

const $ = id => document.getElementById(id);
const pages = ['pool', 'add', 'keys', 'logs', 'cache', 'settings'];
const pageMeta = {
  pool: ['账号池', '账号路由、流量与健康状态'],
  add: ['添加账号', '通过 Microsoft OAuth 安全接入账号'],
  keys: ['访问配置', '管理 API 密钥及其完整生命周期'],
  logs: ['运行日志', '查看请求结果、耗时与脱敏详情'],
  cache: ['会话缓存', '检查跨轮上下文与响应快照持久化'],
  settings: ['系统设置', '运行参数、环境锁定与管理员安全']
};
let noticeTimer;
let logTimer;
let forcedPasswordChange = false;

function esc(value) {
  return String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

function note(message, kind = 'info', ms = 3500) {
  const n = $('notice');
  n.textContent = message;
  n.className = 'notice show';
  n.style.borderLeftColor = kind === 'error' ? 'var(--rose)' : kind === 'success' ? 'var(--emerald)' : 'var(--indigo)';
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => { n.classList.remove('show'); n.style.borderLeftColor = ''; }, ms);
}

function showAuthenticated(authenticated) {
  $('login').style.display = authenticated ? 'none' : 'flex';
  $('app').style.display = authenticated ? 'flex' : 'none';
  if (!authenticated) $('adminPassword').focus();
}

async function api(url, options = {}) {
  const request = {...options, credentials: 'same-origin'};
  request.headers = {'Content-Type': 'application/json', ...(options.headers || {})};
  const response = await fetch(url, request);
  const raw = await response.text();
  let data = {};
  try { data = raw ? JSON.parse(raw) : {}; } catch { data = {error: {message: raw}}; }
  if (!response.ok) {
    const type = data?.error?.type;
    if (response.status === 401) showAuthenticated(false);
    if (response.status === 403 && type === 'password_change_required') openPasswordModal(true);
    throw new Error(data?.error?.message || raw || `请求失败（${response.status}）`);
  }
  return data;
}

async function login(event) {
  event.preventDefault();
  $('loginError').textContent = '';
  try {
    const data = await api('/api/admin/login', {method: 'POST', body: JSON.stringify({password: $('adminPassword').value})});
    showAuthenticated(true);
    if (data.must_change_password) {
      openPasswordModal(true);
      return;
    }
    $('adminPassword').value = '';
    showPage(sessionStorage.getItem('m365.currentPage') || 'pool');
  } catch (error) {
    $('loginError').textContent = error.message;
  }
}

async function checkSession() {
  try {
    const response = await fetch('/api/admin/session', {credentials: 'same-origin'});
    const data = await response.json();
    if (!data.authenticated) { showAuthenticated(false); return; }
    showAuthenticated(true);
    if (data.must_change_password) { openPasswordModal(true); return; }
    showPage(sessionStorage.getItem('m365.currentPage') || 'pool');
  } catch {
    showAuthenticated(false);
  }
}

async function logout() {
  clearInterval(logTimer);
  try { await fetch('/api/admin/logout', {method: 'POST', credentials: 'same-origin'}); } finally { location.reload(); }
}

function openPasswordModal(forced = false) {
  forcedPasswordChange = Boolean(forced);
  $('passwordModalTitle').textContent = forced ? '首次登录必须修改密码' : '修改管理员密码';
  $('passwordModalHelp').textContent = forced
    ? '当前密码是初始化凭据。设置至少 12 个字符的新密码后才能使用管理控制台。'
    : '新密码至少 12 个字符。修改成功后，所有管理会话都会被注销，需要使用新密码重新登录。';
  $('passwordCancelButton').style.display = forced ? 'none' : 'inline-flex';
  $('passwordChangeError').textContent = '';
  $('passwordModal').classList.add('open');
  setTimeout(() => $('currentAdminPassword').focus(), 30);
}

function closePasswordModal() {
  if (forcedPasswordChange) return;
  $('passwordModal').classList.remove('open');
}

async function changeAdminPassword(event) {
  event.preventDefault();
  const current = $('currentAdminPassword').value;
  const next = $('newAdminPassword').value;
  const confirmPassword = $('confirmAdminPassword').value;
  const errorBox = $('passwordChangeError');
  errorBox.textContent = '';
  if (next !== confirmPassword) { errorBox.textContent = '两次输入的新密码不一致'; return; }
  if (next.length < 12) { errorBox.textContent = '新密码至少需要 12 个字符'; return; }
  const button = $('passwordSaveButton');
  button.disabled = true;
  try {
    await api('/api/admin/change-password', {method: 'POST', body: JSON.stringify({current_password: current, new_password: next})});
    note('管理员密码已修改，请重新登录', 'success');
    setTimeout(() => location.reload(), 500);
  } catch (error) {
    errorBox.textContent = error.message;
  } finally {
    button.disabled = false;
  }
}

function showPage(page) {
  if (!pages.includes(page)) page = 'pool';
  sessionStorage.setItem('m365.currentPage', page);
  pages.forEach(name => {
    const section = $('page-' + name);
    if (section) section.style.display = name === page ? 'block' : 'none';
    const nav = document.querySelector(`[data-page="${name}"]`);
    if (nav) nav.classList.toggle('active', name === page);
  });
  $('topTitle').textContent = pageMeta[page][0];
  $('topSub').textContent = pageMeta[page][1];
  if (page === 'pool') loadAccounts();
  if (page === 'keys') loadKeys();
  if (page === 'logs') loadLogs();
  if (page === 'cache') loadSessionCache();
  if (page === 'settings') loadSettings();
}

function fmtTok(value) {
  const n = Number(value || 0);
  if (n < 1000) return String(n);
  if (n < 1e6) return (n / 1000).toFixed(n < 1e4 ? 1 : 0) + 'k';
  return (n / 1e6).toFixed(n < 1e7 ? 1 : 0) + 'M';
}

function fmtBytes(value) {
  const n = Number(value || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}

function fmtDate(value, empty = '—') {
  if (!value) return empty;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? empty : date.toLocaleString();
}

function healthBadge(health) {
  if (!health) return '<span class="hpill hpill-ok" title="当前没有限流或鉴权告警">正常</span>';
  if (health.authFail) return '<span class="hpill hpill-fail" title="401、403、令牌过期或刷新被拒绝">鉴权失败</span>';
  if (health.rateLimited) {
    let label = '限流冷却';
    if (health.cooldownUntil) {
      const seconds = Math.ceil((new Date(health.cooldownUntil) - Date.now()) / 1000);
      if (seconds > 0) label += ` ${seconds}s`;
    }
    return `<span class="hpill hpill-cool">${esc(label)}</span>`;
  }
  return '<span class="hpill hpill-ok">正常</span>';
}

async function loadAccounts() {
  try {
    const data = await api('/api/accounts');
    const accounts = data.accounts || [];
    $('total').textContent = accounts.length;
    $('online').textContent = accounts.filter(a => a.status === 'online').length;
    $('requestTotal').textContent = data.totalRequestCount || 0;
    $('tokenIn').textContent = fmtTok(data.totalTokenIn);
    $('tokenOut').textContent = fmtTok(data.totalTokenOut);
    $('tokenTotal').textContent = fmtTok(Number(data.totalTokenIn || 0) + Number(data.totalTokenOut || 0));
    $('accountRows').innerHTML = accounts.map(account => `<tr data-account-id="${esc(account.id)}">
      <td><div class="account-cell"><div class="avatar">${esc((account.displayName || account.email || 'M').slice(0,1).toUpperCase())}</div><div><b>${esc(account.displayName || account.email || account.id)}</b><div class="muted">序号 ${esc(account.position || '—')} · ${esc(account.email || '')}</div></div></div></td>
      <td><span class="status"><i></i>${account.active ? '当前使用' : '已隔离'}</span></td>
      <td><div class="proxy-cell"><input class="proxy-input" value="${esc(account.proxy || '')}" placeholder="默认直连"><div class="proxy-actions"><button class="btn" onclick="testProxy(this)">测试</button><button class="btn" onclick="saveProxy(this)">保存</button></div></div><span class="proxy-status muted"></span></td>
      <td class="health-cell">${healthBadge(account.health)}</td>
      <td style="text-align:center;font-weight:600;color:var(--indigo)">${Number(account.requestCount || 0)}</td>
      <td><div class="token-split"><span class="tnum"><span class="lab up">↑</span>${fmtTok(account.tokenIn)}</span><span class="tnum"><span class="lab down">↓</span>${fmtTok(account.tokenOut)}</span></div></td>
      <td><button class="btn" onclick="refreshAccount(this)" ${account.active ? '' : 'disabled title="隔离账号不可刷新"'}>刷新令牌</button> <button class="btn danger" onclick="deleteAccount(this)">删除</button></td>
    </tr>`).join('') || '<tr><td colspan="7" class="empty">暂无账号，点击“添加账号”开始。</td></tr>';
    $('lastUpdate').textContent = '刚刚更新';
    $('serviceStatus').textContent = '服务正常';
    $('health').textContent = '正常';
  } catch (error) {
    $('accountRows').innerHTML = `<tr><td colspan="7" class="empty">${esc(error.message)}</td></tr>`;
    $('serviceStatus').textContent = '需要检查';
    $('health').textContent = '—';
  }
}

function accountRow(button) { return button.closest('tr[data-account-id]'); }

async function refreshAccount(button) {
  const row = accountRow(button);
  if (!row || !confirm('立即刷新当前账号的令牌？')) return;
  button.disabled = true;
  const previous = button.textContent;
  button.textContent = '刷新中…';
  try {
    const data = await api('/api/accounts/refresh', {method: 'POST', body: JSON.stringify({id: row.dataset.accountId})});
    note('令牌刷新成功，有效期至 ' + fmtDate(data.account?.expiresAt), 'success');
    await loadAccounts();
  } catch (error) { note('令牌刷新失败：' + error.message, 'error'); }
  finally { button.disabled = false; button.textContent = previous; }
}

async function deleteAccount(button) {
  const row = accountRow(button);
  if (!row || !confirm('确定删除这个账号？相关会话绑定也会同步清理，操作不可撤销。')) return;
  button.disabled = true;
  try {
    await api('/api/accounts/delete', {method: 'POST', body: JSON.stringify({id: row.dataset.accountId})});
    note('账号已删除', 'success');
    await loadAccounts();
  } catch (error) { note('删除失败：' + error.message, 'error'); }
  finally { button.disabled = false; }
}

async function saveProxy(button) {
  const row = accountRow(button);
  const input = row?.querySelector('.proxy-input');
  const status = row?.querySelector('.proxy-status');
  if (!row || !input || !status) return;
  status.textContent = '保存中…';
  try {
    await api('/api/accounts/proxy', {method: 'POST', body: JSON.stringify({id: row.dataset.accountId, proxy: input.value.trim()})});
    status.textContent = '已保存';
    note('该账号的出口配置已保存', 'success');
  } catch (error) { status.textContent = '失败：' + error.message; }
}

async function testProxy(button) {
  const row = accountRow(button);
  const input = row?.querySelector('.proxy-input');
  const status = row?.querySelector('.proxy-status');
  if (!row || !input || !status) return;
  button.disabled = true;
  status.textContent = '测试中…';
  try {
    const data = await api('/api/admin/test-proxy', {method: 'POST', body: JSON.stringify({proxy: input.value.trim()})});
    status.textContent = data.ok ? `✓ ${data.ip || '直连'} · 公网 HTTP ${data.egressHttpMs ?? data.latencyMs ?? '—'}ms` : `✗ ${data.error || '不可用'}`;
    status.className = 'proxy-status ' + (data.ok ? 'proxy-ok' : 'proxy-fail');
  } catch (error) { status.textContent = '✗ ' + error.message; status.className = 'proxy-status proxy-fail'; }
  finally { button.disabled = false; }
}

async function testAllProxies() {
  const button = $('testAllProxiesBtn');
  const summary = $('proxyTestSummaryInline');
  button.disabled = true;
  summary.textContent = '批量检测中…';
  try {
    const data = await api('/api/admin/test-all-proxies', {method: 'POST'});
    let success = 0;
    for (const result of data.results || []) {
      const row = Array.from(document.querySelectorAll('#accountRows tr[data-account-id]')).find(item => item.dataset.accountId === result.accountId);
      if (!row) continue;
      const status = row.querySelector('.proxy-status');
      const input = row.querySelector('.proxy-input');
      if (result.ok) success++;
      status.textContent = result.ok ? `✓ ${result.ip || '直连'} · 公网 HTTP ${result.egressHttpMs ?? result.latencyMs ?? '—'}ms` : `✗ ${result.error || '不可用'}`;
      status.className = 'proxy-status ' + (result.ok ? 'proxy-ok' : 'proxy-fail');
      input.classList.toggle('proxy-input-ok', result.ok);
      input.classList.toggle('proxy-input-fail', !result.ok);
    }
    const total = (data.results || []).length;
    summary.textContent = `检测完成：${success}/${total} 正常 · ${data.uniqueEgress || 0} 个固定出口`;
    summary.className = 'proxy-summary-inline ' + (success === total ? 'proxy-summary-ok' : 'proxy-summary-fail');
  } catch (error) { summary.textContent = '检测失败：' + error.message; summary.className = 'proxy-summary-inline proxy-summary-fail'; }
  finally { button.disabled = false; }
}

async function resetStats() {
  if (!confirm('确定重置所有账号的请求数与 Token 统计？此操作不可恢复。')) return;
  const button = $('resetStatsBtn');
  button.disabled = true;
  try { await api('/api/admin/reset-stats', {method: 'POST'}); note('统计已重置', 'success'); await loadAccounts(); }
  catch (error) { note('重置失败：' + error.message, 'error'); }
  finally { button.disabled = false; }
}

async function startPKCE() {
  try {
    const data = await api('/api/auth/start');
    if (!data.url) throw new Error('授权链接生成失败');
    window.open(data.url, '_blank', 'noopener');
    $('callbackInput').dataset.state = data.state || '';
    note('授权页面已打开；完成后粘贴完整回调地址');
  } catch (error) { note(error.message, 'error'); }
}

async function submitCallback() {
  const raw = $('callbackInput').value.trim();
  if (!raw) { note('请粘贴回调地址或授权码', 'error'); return; }
  try {
    let url = '/api/auth/callback?';
    if (raw.includes('://')) url += 'url=' + encodeURIComponent(raw);
    else {
      const query = raw.replace(/^\?/, '');
      url += query;
      if (!query.includes('state=') && $('callbackInput').dataset.state) url += '&state=' + encodeURIComponent($('callbackInput').dataset.state);
    }
    await api(url);
    $('callbackInput').value = '';
    note('授权成功，账号已加入有序账号池', 'success');
    showPage('pool');
  } catch (error) { note(error.message, 'error'); }
}

async function copyText(value) {
  try { await navigator.clipboard.writeText(value); note('已复制到剪贴板', 'success'); return; } catch {}
  const textarea = document.createElement('textarea');
  textarea.value = value;
  textarea.style.position = 'fixed';
  textarea.style.opacity = '0';
  document.body.appendChild(textarea);
  textarea.select();
  document.execCommand('copy');
  textarea.remove();
  note('已复制到剪贴板', 'success');
}

function openKeyModal() {
  $('keyCreated').style.display = 'none';
  $('newKeyValue').textContent = '';
  $('newKeyName').value = 'default';
  $('newKeyDays').value = '';
  document.querySelector('input[name="keyExpire"][value="0"]').checked = true;
  $('keyModal').classList.add('open');
}
function closeKeyModal() { $('keyModal').classList.remove('open'); }
async function copyCreatedKey() { await copyText($('newKeyValue').textContent); }

async function createKey(event) {
  event.preventDefault();
  const mode = document.querySelector('input[name="keyExpire"]:checked')?.value;
  const days = mode === 'custom' ? Number.parseInt($('newKeyDays').value, 10) : 0;
  if (mode === 'custom' && (!Number.isInteger(days) || days < 1)) { note('请输入正确的有效天数', 'error'); return; }
  try {
    const data = await api('/api/admin/keys', {method: 'POST', body: JSON.stringify({name: $('newKeyName').value.trim() || 'API key', days})});
    $('newKeyValue').textContent = data.key;
    $('keyCreated').style.display = 'block';
    await copyText(data.key);
    await loadKeys();
  } catch (error) { note('创建失败：' + error.message, 'error'); }
}

function keyState(key) {
  if (key.revoked) return 'revoked';
  if (key.expiresAt && new Date(key.expiresAt).getTime() <= Date.now()) return 'expired';
  return 'active';
}

async function loadKeys() {
  try {
    const data = await api('/api/admin/keys');
    const keys = data.keys || [];
    const filter = $('keyFilter').value;
    const visible = keys.filter(key => filter === 'all' || keyState(key) === filter);
    $('keyRows').innerHTML = visible.map(key => {
      const state = keyState(key);
      const stateLabel = state === 'active' ? '有效' : state === 'expired' ? '已过期' : '已撤销';
      const expiry = key.expiresAt ? fmtDate(key.expiresAt) : '永久有效';
      const actions = state === 'revoked'
        ? '<button class="btn danger" onclick="purgeKey(this)">永久删除</button>'
        : '<button class="btn" onclick="openExpiryModal(this)">改有效期</button> <button class="btn danger" onclick="revokeKey(this)">撤销</button>';
      return `<tr data-key-id="${esc(key.id)}"><td><b>${esc(key.name)}</b></td><td><code>${esc(key.prefix)}…</code></td><td class="muted">${fmtDate(key.createdAt)}</td><td class="muted">${fmtDate(key.lastUsedAt, '未使用')}</td><td>${esc(expiry)}</td><td>${stateLabel}</td><td>${actions}</td></tr>`;
    }).join('') || '<tr><td colspan="7" class="empty">此筛选条件下没有 API Key</td></tr>';
    const counts = {active: 0, expired: 0, revoked: 0};
    keys.forEach(key => counts[keyState(key)]++);
    $('keySummary').textContent = `共 ${keys.length} 条 · 有效 ${counts.active} · 已过期 ${counts.expired} · 已撤销 ${counts.revoked}`;
  } catch (error) { note(error.message, 'error'); }
}

function openExpiryModal(button) {
  $('expiryKeyId').value = button.closest('tr').dataset.keyId;
  $('expiryDays').value = '';
  document.querySelector('input[name="expMode"][value="0"]').checked = true;
  $('keyExpiryModal').classList.add('open');
}
function closeExpiryModal() { $('keyExpiryModal').classList.remove('open'); }

async function setKeyExpiry() {
  const mode = document.querySelector('input[name="expMode"]:checked')?.value;
  const days = mode === 'custom' ? Number.parseInt($('expiryDays').value, 10) : 0;
  if (mode === 'custom' && (!Number.isInteger(days) || days < 1)) { note('请输入正确的有效天数', 'error'); return; }
  try {
    await api('/api/admin/keys', {method: 'PATCH', body: JSON.stringify({id: $('expiryKeyId').value, days})});
    closeExpiryModal();
    note('密钥有效期已更新', 'success');
    await loadKeys();
  } catch (error) { note(error.message, 'error'); }
}

async function revokeKey(button) {
  const id = button.closest('tr').dataset.keyId;
  if (!confirm('撤销这个 API Key？使用它的客户端将立即失去访问权限。')) return;
  try { await api('/api/admin/keys?id=' + encodeURIComponent(id), {method: 'DELETE'}); note('密钥已撤销', 'success'); await loadKeys(); }
  catch (error) { note(error.message, 'error'); }
}

async function purgeKey(button) {
  const id = button.closest('tr').dataset.keyId;
  if (!confirm('永久删除这条已撤销的密钥记录？此操作不可恢复。')) return;
  try { await api('/api/admin/keys?id=' + encodeURIComponent(id) + '&purge=true', {method: 'DELETE'}); note('密钥记录已永久删除', 'success'); await loadKeys(); }
  catch (error) { note(error.message, 'error'); }
}

async function loadLogs() {
  try {
    const data = await api('/api/admin/debug/logs');
    const filter = $('logLevelFilter').value;
    const rows = (data.records || []).filter(record => !filter || record.level === filter);
    $('logRows').innerHTML = rows.map(record => `<tr><td><span class="log-${esc(record.level || 'info')}">${esc(record.level || 'info')}</span></td><td class="muted">${fmtDate(record.at)}</td><td><code>${esc(record.method)} ${esc(record.path)}</code></td><td>${Number(record.status || 0)}</td><td>${Number(record.durationMs || 0)} ms</td><td><code>${esc(record.requestId || record.id || '')}</code></td><td><button class="btn" data-log-id="${esc(record.id)}" onclick="openLogDetail(this)">详情</button></td></tr>`).join('') || '<tr><td colspan="7" class="empty">暂无日志记录</td></tr>';
    $('logSummary').textContent = `${rows.length} 条记录 · 请求与上游字段已脱敏并限制长度`;
  } catch (error) { note(error.message, 'error'); }
}

function setLogAutoRefresh(enabled) {
  clearInterval(logTimer);
  if (enabled) logTimer = setInterval(() => {
    if ($('page-logs').style.display !== 'none' && document.visibilityState === 'visible') loadLogs();
  }, 5000);
}

async function openLogDetail(button) {
  $('logDetailModal').classList.add('open');
  $('logDetailBody').textContent = '加载中…';
  try {
    const data = await api('/api/admin/debug/detail?id=' + encodeURIComponent(button.dataset.logId));
    $('logDetailBody').textContent = JSON.stringify(data, null, 2);
  } catch (error) { $('logDetailBody').textContent = '加载失败：' + error.message; }
}
function closeLogDetail() { $('logDetailModal').classList.remove('open'); }

async function loadSessionCache() {
  try {
    const data = await api('/api/admin/session-cache');
    const stats = data.stats || {};
    $('sessionTotal').textContent = stats.totalRecords ?? 0;
    $('sessionStable').textContent = stats.stableRecords ?? 0;
    $('sessionAliases').textContent = stats.responseAliases ?? 0;
    $('sessionPinned').textContent = stats.pinnedRecords ?? 0;
    $('sessionBytes').textContent = fmtBytes(stats.serializedBytes);
    $('sessionOldest').textContent = fmtDate(stats.oldestUpdatedAt);
    $('sessionLatest').textContent = fmtDate(stats.latestUpdatedAt);
  } catch (error) { note(error.message, 'error'); }
}

async function pruneSessionCache() {
  if (!confirm('执行安全清理？只会移除过期、无效或超过上限的快照，当前请求链会保留。')) return;
  const button = $('pruneSessionButton');
  button.disabled = true;
  try {
    const data = await api('/api/admin/session-cache', {method: 'POST', body: JSON.stringify({action: 'prune'})});
    const removed = Number(data.before?.totalRecords || 0) - Number(data.after?.totalRecords || 0);
    note(`安全清理完成，移除 ${Math.max(0, removed)} 条记录`, 'success');
    await loadSessionCache();
  } catch (error) { note(error.message, 'error'); }
  finally { button.disabled = false; }
}

const settingFields = [
  ['maxToolCallsPerTurn','每轮最大工具调用数','number',false], ['maxToolRounds','最大工具轮次','number',false],
  ['contextWindow','上下文窗口','number',false], ['maxOutputTokens','最大输出 Token','number',false],
  ['chatTimeoutSeconds','聊天超时（秒）','number',false], ['imageTimeoutSeconds','图片超时（秒）','number',false],
  ['logLevel','日志等级','select',false], ['debugLogPath','调试日志路径','text',true],
  ['listenAddress','监听地址','text',true], ['configPath','账号配置路径','text',true],
  ['tokenCachePath','Token 缓存路径','text',true], ['sessionCachePath','会话缓存路径','text',true],
  ['clientId','OAuth Client ID','text',true], ['authority','OAuth Authority','text',true],
  ['redirectUri','OAuth 回调地址','text',true], ['scope','OAuth Scope','text',true]
];

async function loadSettings() {
  try {
    const data = await api('/api/admin/settings');
    const values = data.settings || {};
    const locked = new Set(data.environmentOverrides || []);
    $('settingsForm').innerHTML = settingFields.map(([key, label, type, restart]) => {
      const isLocked = locked.has(key);
      const badge = isLocked ? '<span class="lock-label">环境变量锁定</span>' : '';
      const suffix = restart ? ' <span class="muted">（重启生效）</span>' : '';
      let control;
      if (type === 'select') {
        const options = [['silent','静默'],['error','错误'],['warn','警告'],['info','信息'],['debug','调试']];
        control = `<select class="input setting-input ${isLocked ? 'setting-locked' : ''}" data-key="${key}" ${isLocked ? 'disabled' : ''}>${options.map(([value, text]) => `<option value="${value}" ${values[key] === value ? 'selected' : ''}>${text}</option>`).join('')}</select>`;
      } else {
        control = `<input class="input setting-input ${isLocked ? 'setting-locked' : ''}" data-key="${key}" type="${type}" value="${esc(values[key] ?? '')}" ${isLocked ? 'disabled' : ''}>`;
      }
      return `<div class="form-row"><label>${label}${suffix}${badge}</label>${control}</div>`;
    }).join('');
  } catch (error) { note(error.message, 'error'); }
}

async function saveSettings() {
  try {
    const current = await api('/api/admin/settings');
    const values = {...(current.settings || {})};
    document.querySelectorAll('.setting-input:not(:disabled)').forEach(input => {
      values[input.dataset.key] = input.type === 'number' ? Number(input.value) : input.value;
    });
    await api('/api/admin/settings', {method: 'PUT', body: JSON.stringify(values)});
    note('设置已保存；标注为重启生效的项目将在服务重启后应用', 'success');
    await loadSettings();
  } catch (error) { note(error.message, 'error'); }
}

$('baseUrl').value = location.origin.replace(/\/$/, '') + '/v1';
$('baseUrl2').value = location.origin.replace(/\/$/, '') + '/v1';
checkSession();
