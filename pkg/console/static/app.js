'use strict';
// Paper Farm Console. Credentials live only in this browser's localStorage;
// the local server holds a session key in memory only while a run uses it.

const TOKEN = document.querySelector('meta[name=console-token]').content;
const LIVE = document.querySelector('meta[name=console-live]').content === 'true';
const SYMBOLS = ['BTC', 'ETH'];
const SCALE = {0: 10, 1: 100}; // raw price scale by instrument id
const SYM = {0: 'BTC', 1: 'ETH'};
const LS = {account: 'pf.account', key: 'pf.sessionKey', keyAddr: 'pf.sessionAddr', prefs: 'pf.prefs', run: 'pf.run.'};

const $ = (id) => document.getElementById(id);
const store = {
  get(k) { try { return localStorage.getItem(k) || ''; } catch { return ''; } },
  set(k, v) { try { v ? localStorage.setItem(k, v) : localStorage.removeItem(k); } catch {} },
  json(k, d) { try { return JSON.parse(localStorage.getItem(k)) ?? d; } catch { return d; } },
};
const state = {market: null, wallet: null, runs: [], mids: {}};

// --- helpers -------------------------------------------------------------

async function api(path, body) {
  const opt = {headers: {'X-Console-Token': TOKEN}};
  if (body !== undefined) {
    opt.method = 'POST';
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const res = await fetch(path, opt);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
  return data;
}
const raw18 = (s) => (s ? Number(s) / 1e18 : 0);
const usd = (v, d = 2) => (v == null || isNaN(v) ? '–' : '$' + Number(v).toLocaleString('en-US', {minimumFractionDigits: d, maximumFractionDigits: d}));
const num = (v, d = 2) => (v == null || isNaN(v) ? '–' : Number(v).toLocaleString('en-US', {maximumFractionDigits: d}));
const pct = (v, d = 3) => (v == null || isNaN(v) ? '–' : (v * 100).toFixed(d) + '%');
const dur = (h) => (!isFinite(h) || h <= 0 ? '–' : h < 1 ? Math.round(h * 60) + ' 分钟' : h.toFixed(1) + ' 小时');
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
const frac = (p) => String(Number((p / 100).toFixed(8))); // percent input → decimal string

let toastTimer;
function toast(msg, err) {
  const t = $('toast');
  t.textContent = msg;
  t.className = err ? 'err' : '';
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), err ? 9000 : 4000);
}
async function act(btn, fn) {
  if (btn) btn.disabled = true;
  try { return await fn(); } catch (e) { toast(e.message, true); } finally { if (btn) btn.disabled = false; }
}
function creds() {
  const account = store.get(LS.account), sessionKey = store.get(LS.key);
  if (!account) throw new Error('先填写或连接 Paper 账户地址');
  if (!sessionKey) throw new Error('没有 session key，先生成并注册');
  return {account, sessionKey};
}
function credsLog(line) {
  const el = $('credsLog');
  el.hidden = false;
  el.textContent += line + '\n';
  el.scrollTop = el.scrollHeight;
}

// --- wallet & session key -------------------------------------------------

function provider() {
  const p = window.okxwallet || window.ethereum;
  if (!p) throw new Error('没有检测到 OKX 钱包扩展');
  return p;
}
async function connectWallet() {
  const p = provider();
  const [address] = await p.request({method: 'eth_requestAccounts'});
  try {
    await p.request({method: 'wallet_switchEthereumChain', params: [{chainId: '0x3e7'}]});
  } catch (e) {
    if (e.code !== 4902) throw e;
    await p.request({method: 'wallet_addEthereumChain', params: [{chainId: '0x3e7', chainName: 'HyperEVM',
      nativeCurrency: {name: 'HYPE', symbol: 'HYPE', decimals: 18}, rpcUrls: ['https://rpc.hyperliquid.xyz/evm'],
      blockExplorerUrls: ['https://hyperevmscan.io']}]});
  }
  setAccount(address);
  return {p, address};
}
function setAccount(a) {
  store.set(LS.account, a);
  $('account').value = a;
  refreshAccount();
}
async function newSession() {
  const {p, address} = await connectWallet();
  const key = await api('/api/session/new', {});
  // Keep the key before asking for a signature so a rejected or failed
  // registration never loses a key the relayer might already know.
  store.set(LS.key, key.privateKey);
  store.set(LS.keyAddr, key.address);
  renderSession();
  credsLog('新 session key: ' + key.address + '（已存入本浏览器）');
  const td = await api('/api/session/typed-data', {account: address, sessionKey: key.address, days: 30});
  credsLog('请在钱包里核对 key 地址并签名 RegisterSessionKey…');
  const signature = await p.request({method: 'eth_signTypedData_v4', params: [address, JSON.stringify(td)]});
  const res = await api('/api/session/register', {account: address, typedData: td, signature});
  credsLog((res.result && res.result.dryRun ? '[dry-run] 未提交: ' : '已提交: ') + res.intentId);
  setTimeout(refreshAccount, 3000);
}
async function importKey() {
  const k = prompt('粘贴 session key 私钥（0x…）。不要粘贴主钱包私钥。');
  if (!k) return;
  const {address} = await api('/api/session/address', {sessionKey: k.trim()});
  if (address.toLowerCase() === store.get(LS.account).toLowerCase()) throw new Error('这是主钱包私钥，拒绝保存');
  store.set(LS.key, k.trim());
  store.set(LS.keyAddr, address);
  renderSession();
  toast('已导入 ' + address);
}
function exportKey() {
  const k = store.get(LS.key);
  if (!k) throw new Error('本地没有 session key');
  if (confirm('显示 session key 私钥？它能代表账户交易和质押（不能提现）。')) prompt('session key（复制后妥善保管）', k);
}
function forgetKey() {
  if (!confirm('从本浏览器删除账户地址和 session key？已注册的 key 在链上仍有效直到过期。')) return;
  store.set(LS.key, '');
  store.set(LS.keyAddr, '');
  store.set(LS.account, '');
  $('account').value = '';
  state.wallet = null;
  renderSession();
  renderAccount();
}
function sessionExpiry() {
  const addr = store.get(LS.keyAddr).toLowerCase();
  const keys = (state.wallet && state.wallet.sessionKeys) || [];
  const hit = keys.find((k) => k[0] && k[0].toLowerCase() === addr);
  return hit ? Number(hit[1]) * 1000 : 0;
}
function renderSession() {
  const addr = store.get(LS.keyAddr);
  $('sessionAddr').value = addr;
  const s = $('sessionStatus');
  if (!addr) { s.textContent = '无'; s.className = 'pill'; return; }
  if (!state.wallet) { s.textContent = '未查询'; s.className = 'pill'; return; }
  const exp = sessionExpiry();
  if (!exp) { s.textContent = '未注册'; s.className = 'pill bad'; return; }
  const days = (exp - Date.now()) / 864e5;
  if (days <= 0) { s.textContent = '已过期'; s.className = 'pill bad'; return; }
  s.textContent = '有效 ' + (days < 1 ? Math.round(days * 24) + ' 小时' : Math.floor(days) + ' 天');
  s.className = days < 1 ? 'pill warn' : 'pill good';
}

// --- account --------------------------------------------------------------

async function refreshAccount() {
  const a = store.get(LS.account);
  if (!a) { renderSession(); return; }
  try {
    const res = await api('/api/account?address=' + encodeURIComponent(a));
    state.wallet = res.wallet;
    state.trades = res.trades || [];
  } catch (e) {
    toast('账户查询失败: ' + e.message, true);
  }
  renderSession();
  renderAccount();
}
function renderAccount() {
  const w = state.wallet;
  $('balance').textContent = w ? usd(raw18(w.balance)) : '–';
  $('available').textContent = w ? usd(raw18(w.funds.availableBalance)) : '–';
  $('paperBal').textContent = w ? num(raw18(w.paper.balance), 4) : '–';
  $('paperStaked').textContent = w ? num(raw18(w.paper.staked), 4) : '–';
  $('pending').textContent = w ? usd(raw18(w.paper.pendingBase), 4) : '–';
  const body = $('positions').tBodies[0];
  const pos = (w && w.openPositions) || [];
  body.innerHTML = pos.length ? '' : '<tr><td colspan="10" class="muted">无持仓</td></tr>';
  for (const p of pos) {
    const sc = SCALE[p.InstrumentID] || 1, sym = SYM[p.InstrumentID] || p.InstrumentID;
    const entry = Number(p.EntryPriceRaw) / sc, bust = Number(p.BustPriceRaw) / sc, margin = raw18(p.MarginRaw);
    const mid = state.mids[sym];
    const move = mid ? (mid - entry) / entry * (p.IsLong ? 1 : -1) : NaN;
    const toBust = mid ? Math.abs(mid - bust) / mid : NaN;
    const tr = document.createElement('tr');
    tr.innerHTML = `<td><input type="checkbox" value="${esc(p.ID)}"></td><td>${esc(p.ID)}</td><td>${esc(sym)}</td>
      <td class="${p.IsLong ? 'good' : 'bad'}">${p.IsLong ? '多' : '空'}</td><td>${p.Leverage}x</td><td>${usd(margin)}</td>
      <td>${num(entry, 2)}</td><td>${mid ? num(mid, 2) : '–'}</td>
      <td class="${move >= 0 ? 'good' : 'bad'}">${isNaN(move) ? '–' : usd(margin * p.Leverage * move)}</td>
      <td class="${toBust < 0.0005 ? 'bad' : toBust < 0.002 ? 'warn' : ''}">${pct(toBust)}</td>`;
    body.appendChild(tr);
  }
  const tb = $('trades').tBodies[0];
  const trades = state.trades || [];
  tb.innerHTML = trades.length ? '' : '<tr><td colspan="10" class="muted">无记录</td></tr>';
  for (const t of trades) {
    const sc = SCALE[t.instrumentId] || 1, pnl = raw18(t.adjustedPnlRaw);
    const tr = document.createElement('tr');
    tr.innerHTML = `<td>${new Date(t.timestampMs).toLocaleString()}</td><td>${esc(t.positionId)}</td><td>${esc(t.symbol)}</td>
      <td class="${t.isLong ? 'good' : 'bad'}">${t.isLong ? '多' : '空'}${t.kind && t.kind !== 'close' ? ' · ' + esc(t.kind) : ''}</td>
      <td>${t.leverage}x</td><td>${num(Number(t.entryPriceRaw) / sc, 2)}</td><td>${num(Number(t.exitPriceRaw) / sc, 2)}</td>
      <td class="${pnl >= 0 ? 'good' : 'bad'}">${usd(pnl, 4)}</td><td>${usd(raw18(t.userPaidFeeRaw), 4)}</td>
      <td>${num(raw18(t.paperMintedRaw), 4)}</td>`;
    tb.appendChild(tr);
  }
}
async function closeSelected() {
  const ids = [...document.querySelectorAll('#positions tbody input:checked')].map((c) => c.value);
  if (!ids.length) throw new Error('先勾选要平的仓位');
  if (!confirm(`用一个 Close intent 平掉 ${ids.length} 个仓位？` + (LIVE ? '' : '\n(dry-run，不会提交)'))) return;
  const res = await api('/api/close', {...creds(), ids});
  toast((res.dryRun ? '[dry-run] ' : '已提交 ') + res.intentId);
  setTimeout(refreshAccount, 3000);
}
async function stakeAll() {
  if (!confirm('质押全部未质押的 PAPER？' + (LIVE ? '' : '\n(dry-run)'))) return;
  const res = await api('/api/stake', {...creds(), amount: ''});
  toast((res.dryRun ? '[dry-run] ' : '已提交 ') + res.intentId);
  setTimeout(refreshAccount, 3000);
}
async function claim() {
  const res = await api('/api/claim', creds());
  toast((res.result && res.result.dryRun ? '[dry-run] ' : '已提交 ') + res.intentId);
  setTimeout(refreshAccount, 3000);
}

// --- market & recommendations ---------------------------------------------

function prefs() {
  return {capital: $('capital').value, hours: $('hours').value, window: $('window').value};
}
function savePrefs() { store.set(LS.prefs, JSON.stringify(prefs())); }
async function refreshMarket() {
  const p = prefs();
  const q = new URLSearchParams({capital: p.capital || '0', hours: p.hours, window: p.window});
  try {
    state.market = await api('/api/market?' + q);
  } catch (e) {
    $('recoCards').innerHTML = `<div class="sub bad">${esc(e.message)}</div>`;
    return;
  }
  const m = state.market;
  for (const v of m.markets) if (v.mid) state.mids[v.symbol] = v.mid;
  const paused = $('paused');
  paused.textContent = m.paused ? 'Paper 已暂停' : 'Paper 交易中';
  paused.className = 'badge ' + (m.paused ? 'bad' : 'good');
  $('mintRate').textContent = num(m.mintRate, 3);
  $('updated').textContent = '行情 ' + new Date(m.at).toLocaleTimeString();
  renderReco();
  renderAccount();
}
function renderReco() {
  const m = state.market;
  if (!m) return;
  const box = $('recoCards');
  box.innerHTML = '';
  const total = Number($('capital').value) || 0;
  for (const v of m.markets) {
    const pl = v.plan || {}, e = pl.estimate || {};
    const div = document.createElement('div');
    div.className = 'sub';
    const ratio = v.sigmaDayWindow ? v.sigmaPerSqrtHour / v.sigmaDayWindow : 0;
    let body = `<dl>
      <dt>中间价</dt><dd>${num(v.mid, 2)}</dd>
      <dt>σ（${esc(m.window)} 窗口）</dt><dd>${pct(v.sigmaPerSqrtHour)} / √h
        ${ratio ? `<span class="${ratio > 1.2 ? 'warn' : 'muted'}">（24h 的 ${ratio.toFixed(2)}×）</span>` : ''}</dd>
      <dt>σ（24h）</dt><dd>${pct(v.sigmaDayWindow)} / √h</dd>
      <dt>最大杠杆</dt><dd>${v.maxLeverage || '–'}x</dd>`;
    if (v.error) body += `<dt class="bad">错误</dt><dd class="bad">${esc(v.error)}</dd>`;
    if (!total) {
      body += `</dl><div class="note">填写总资金后给出推荐</div>`;
    } else if (!pl.feasible) {
      body += `<dt>可用资金</dt><dd>${usd(total / m.markets.length)}</dd></dl>
        <div class="note bad">不可行：${esc(pl.reason || '')}${pl.capitalForPlan ? `；至少需要每品种 ${usd(pl.capitalForPlan)}` : ''}</div>`;
    } else {
      body += `
      <dt><b>推荐杠杆</b></dt><dd><b>${pl.leverage}x</b></dd>
      <dt><b>平仓波动</b></dt><dd><b>${pct(pl.closeMove)}</b></dd>
      <dt><b>爆仓保护</b></dt><dd><b>${pct(pl.bustGuard)}</b></dd>
      <dt>爆仓距离</dt><dd>${pct(pl.bustDistance)}</dd>
      <dt>每腿保证金 / 名义</dt><dd>${usd(pl.marginPerLeg)} / ${usd(pl.notionalPerLeg, 0)}</dd>
      <dt>预计每轮耗时</dt><dd>${dur(pl.expectedHours)}</dd>
      <dt>每轮成本 / PAPER</dt><dd>${usd(e.netCost, 3)} / ${num(e.paperMinted, 2)}</dd>
      <dt>成本 / PAPER</dt><dd>${usd(e.costPerPaper, 4)}</dd>
      <dt>PAPER / 小时</dt><dd>${num(pl.paperPerHour, 2)}（${usd(pl.costPerHour, 3)}/h）</dd></dl>
      ${pl.reason ? `<div class="note warn">${esc(pl.reason)}${pl.capitalForTarget ? `；达到目标等待约需每品种 ${usd(pl.capitalForTarget)}` : ''}</div>` : ''}
      <div class="row"><button data-apply="${esc(v.symbol)}">应用到 ${esc(v.symbol)} 策略</button></div>`;
    }
    div.innerHTML = `<h3>${esc(v.symbol)}</h3>` + body;
    box.appendChild(div);
  }
  box.querySelectorAll('[data-apply]').forEach((b) => b.addEventListener('click', () => applyPlan(b.dataset.apply)));
}
function applyPlan(sym) {
  const v = state.market.markets.find((x) => x.symbol === sym);
  if (!v || !v.plan.feasible) return;
  const pl = v.plan, f = form(sym);
  f.margin.value = Math.floor(pl.marginPerLeg * 100) / 100;
  f.leverage.value = pl.leverage;
  f.closeMove.value = (pl.closeMove * 100).toFixed(4);
  f.bustGuard.value = (pl.bustGuard * 100).toFixed(4);
  f.maxHold.value = Math.max(30, Math.ceil(pl.expectedHours * 60 * 3));
  saveForm(sym);
  toast(`已应用 ${sym} 推荐参数`);
}

// --- runs -----------------------------------------------------------------

const FIELDS = [
  ['margin', '每腿保证金 $', '20', '1'],
  ['leverage', '杠杆', '500', '1'],
  ['closeMove', '平仓波动 %', '0.1', '0.001'],
  ['bustGuard', '爆仓保护 %', '0.05', '0.001'],
  ['maxHold', '最长持有 分钟（0=不限）', '120', '1'],
  ['cycles', '轮数', '1', '1'],
];
function form(sym) {
  const out = {};
  for (const [k] of FIELDS) out[k] = $(`${sym}-${k}`);
  out.stake = $(`${sym}-stake`);
  return out;
}
function saveForm(sym) {
  const f = form(sym), o = {};
  for (const [k] of FIELDS) o[k] = f[k].value;
  o.stake = f.stake.checked;
  store.set(LS.run + sym, JSON.stringify(o));
}
function buildRunCards() {
  const box = $('runCards');
  for (const sym of SYMBOLS) {
    const saved = store.json(LS.run + sym, {});
    const div = document.createElement('div');
    div.className = 'sub';
    div.id = 'run-' + sym;
    div.innerHTML = `<h3><span>${sym}</span><span class="pill" id="${sym}-state">空闲</span></h3>
      <div class="form">${FIELDS.map(([k, label, def, step]) =>
        `<label>${label}<input type="number" min="0" step="${step}" id="${sym}-${k}" value="${esc(saved[k] ?? def)}"></label>`).join('')}
        <label class="check"><input type="checkbox" id="${sym}-stake" ${saved.stake ? 'checked' : ''}> 每轮质押 PAPER</label></div>
      <div class="note" id="${sym}-hint"></div>
      <div class="row wrap">
        <button class="primary" id="${sym}-start">${LIVE ? '开始' : '预览签名（dry-run）'}</button>
        <button id="${sym}-stop">停止（保留仓位）</button>
        <button class="danger" id="${sym}-stopClose">停止并平仓</button>
      </div>
      <dl id="${sym}-summary"></dl>
      <pre class="log" id="${sym}-log"></pre>`;
    box.appendChild(div);
    for (const [k] of FIELDS) $(`${sym}-${k}`).addEventListener('input', () => { saveForm(sym); hint(sym); });
    $(`${sym}-stake`).addEventListener('change', () => saveForm(sym));
    $(`${sym}-start`).addEventListener('click', (e) => act(e.target, () => startRun(sym)));
    $(`${sym}-stop`).addEventListener('click', (e) => act(e.target, () => stopRun(sym, false)));
    $(`${sym}-stopClose`).addEventListener('click', (e) => act(e.target, () => stopRun(sym, true)));
    hint(sym);
  }
}
function hint(sym) {
  const f = form(sym);
  const lev = Number(f.leverage.value), move = Number(f.closeMove.value) / 100, guard = Number(f.bustGuard.value) / 100;
  const bust = lev ? 1 / lev - 0.0005 : 0; // safe distance after the venue's bust buffer
  const notional = Number(f.margin.value) * lev;
  const v = state.market && state.market.markets.find((x) => x.symbol === sym);
  const parts = [`名义 ${usd(notional, 0)}`, `爆仓距离 ${pct(bust)}`];
  if (v && v.sigmaPerSqrtHour && move) parts.push(`预计每轮 ${dur((move / v.sigmaPerSqrtHour) ** 2)}`);
  let cls = 'note';
  if (move + guard > bust) { parts.push('⚠ 平仓波动 + 保护 超过爆仓距离，会先爆仓'); cls = 'note bad'; }
  if (state.market && notional < state.market.minNotional) { parts.push(`⚠ 名义低于最小 ${usd(state.market.minNotional, 0)}`); cls = 'note bad'; }
  const el = $(`${sym}-hint`);
  el.textContent = parts.join(' · ');
  el.className = cls;
}
async function startRun(sym) {
  const f = form(sym);
  const req = {...creds(), symbol: sym, margin: String(f.margin.value), leverage: Number(f.leverage.value),
    closeMove: frac(Number(f.closeMove.value)), bustGuard: frac(Number(f.bustGuard.value)),
    maxHoldMinutes: Number(f.maxHold.value), cycles: Number(f.cycles.value), stakeEachCycle: f.stake.checked};
  if (LIVE && !confirm(`实盘开始 ${sym}：每腿 $${req.margin} × ${req.leverage}x，平仓波动 ${f.closeMove.value}%，${req.cycles} 轮。确认？`)) return;
  const res = await api('/api/runs', req);
  if (res.dryRun) {
    $(`${sym}-log`).textContent = '[dry-run] 将提交的两笔开仓 intent：\n' + JSON.stringify(res.openIntents, null, 2);
    toast('dry-run：已签名但未提交');
    return;
  }
  toast(`${sym} 已开始`);
  refreshRuns();
}
async function stopRun(sym, close) {
  if (close && !confirm(`停止 ${sym} 并平掉当前一对仓位？`)) return;
  await api(`/api/runs/${sym}/stop${close ? '?close=1' : ''}`, {});
  toast(`${sym} 已停止`);
  refreshRuns();
  setTimeout(refreshAccount, 3000);
}
const STATE_LABEL = {running: ['运行中', 'good'], stopping: ['停止中', 'warn'], stopped: ['已停止', ''], done: ['完成', 'good'], failed: ['失败', 'bad']};
async function refreshRuns() {
  try { state.runs = await api('/api/runs'); } catch { return; }
  let changed = false;
  for (const sym of SYMBOLS) {
    const r = state.runs.find((x) => x.symbol === sym);
    const active = r && (r.state === 'running' || r.state === 'stopping');
    const [label, cls] = r ? STATE_LABEL[r.state] || [r.state, ''] : ['空闲', ''];
    const pill = $(`${sym}-state`);
    pill.textContent = label + (r && r.state === 'running' ? ` · 第 ${r.cycle}/${r.config.cycles} 轮` : '');
    pill.className = 'pill ' + cls;
    $(`${sym}-start`).disabled = !!active;
    $(`${sym}-stop`).disabled = !active;
    $(`${sym}-stopClose`).disabled = !active;
    if (!r) continue;
    const results = r.results || [];
    $(`${sym}-summary`).innerHTML = `<dt>已完成轮数</dt><dd>${results.length}</dd>
      <dt>净成本</dt><dd>${usd(Number(r.netCost), 4)}</dd><dt>铸造 PAPER</dt><dd>${num(Number(r.paperMinted), 4)}</dd>
      <dt>成本 / PAPER</dt><dd>${Number(r.paperMinted) > 0 ? usd(Number(r.netCost) / Number(r.paperMinted), 4) : '–'}</dd>
      ${r.openPair && r.openPair.length ? `<dt class="warn">保留的仓位</dt><dd class="warn">${r.openPair.map(esc).join(', ')}</dd>` : ''}
      ${r.error ? `<dt class="bad">错误</dt><dd class="bad">${esc(r.error)}</dd>` : ''}`;
    const log = $(`${sym}-log`);
    const text = (r.logs || []).join('\n');
    if (log.textContent !== text) {
      const atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 4;
      log.textContent = text;
      if (atBottom) log.scrollTop = log.scrollHeight;
      changed = true;
    }
  }
  if (changed) refreshAccount();
}

// --- boot -----------------------------------------------------------------

function init() {
  const mode = $('mode');
  mode.textContent = LIVE ? '实盘 LIVE' : 'DRY-RUN（不提交）';
  mode.className = 'badge ' + (LIVE ? 'live' : 'dry');
  $('account').value = store.get(LS.account);
  const p = store.json(LS.prefs, {});
  if (p.capital) $('capital').value = p.capital;
  if (p.hours) $('hours').value = p.hours;
  if (p.window) $('window').value = p.window;

  $('account').addEventListener('change', (e) => {
    const v = e.target.value.trim();
    if (v && !/^0x[0-9a-fA-F]{40}$/.test(v)) return toast('地址格式不对', true);
    setAccount(v);
  });
  $('connect').addEventListener('click', (e) => act(e.target, connectWallet));
  $('newSession').addEventListener('click', (e) => act(e.target, newSession));
  $('importKey').addEventListener('click', (e) => act(e.target, importKey));
  $('exportKey').addEventListener('click', (e) => act(e.target, exportKey));
  $('forgetKey').addEventListener('click', (e) => act(e.target, forgetKey));
  $('refreshAcct').addEventListener('click', (e) => act(e.target, refreshAccount));
  $('closeSelected').addEventListener('click', (e) => act(e.target, closeSelected));
  $('stakeAll').addEventListener('click', (e) => act(e.target, stakeAll));
  $('claim').addEventListener('click', (e) => act(e.target, claim));
  let debounce;
  for (const id of ['capital', 'hours', 'window']) {
    $(id).addEventListener('input', () => {
      savePrefs();
      clearTimeout(debounce);
      debounce = setTimeout(refreshMarket, 400);
    });
  }

  buildRunCards();
  renderSession();
  refreshAccount();
  refreshMarket().then(() => SYMBOLS.forEach(hint));
  refreshRuns();
  setInterval(refreshRuns, 2000);
  setInterval(refreshMarket, 30000);
  setInterval(refreshAccount, 20000);
}
init();
