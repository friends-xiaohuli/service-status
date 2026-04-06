let INTERVAL = 60;
let countdown = INTERVAL;
let allData = [];
let rangeKey = '24h';
let lastRefreshedAt = 0;
let fetching = false;
let historyRequestSeq = 0;

const API_URL = '/api/status';
const SUMMARY_CACHE_KEY = 'status:web:summary';
const NEXT_CHECK_CACHE_KEY = 'status:web:next-check';
const NO_UPDATE_INTERVAL = 60;
const UPDATED_INTERVAL = 120;
const RANGE_LABEL = { '24h': '近 24 小时', '7d': '近 7 天', '30d': '近 30 天' };
const SLOTS = window.innerWidth <= 600 ? 30 : 40;
const HISTORY_CHUNK_SIZE = 24;
const STATUS_LABELS = { online: '在线', offline: '离线', unknown: '降级' };

const historyStore = {
  '24h': {},
  '7d': {},
  '30d': {},
};

const historyStamp = {
  '24h': 0,
  '7d': 0,
  '30d': 0,
};

const typeLabels = {
  http: 'HTTP',
  tcp: 'TCP',
  ping: 'PING',
  mysql: 'MySQL',
  napcat_qq: 'NapCat QQ',
};

const TYPE_CODES = ['http', 'tcp', 'ping', 'mysql', 'napcat_qq'];
const STATUS_CODES = ['online', 'offline', 'unknown'];

function saveLocalCache(key, value) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch (_) {}
}

function readLocalCache(key) {
  try {
    const raw = localStorage.getItem(key);
    return raw ? JSON.parse(raw) : null;
  } catch (_) {
    return null;
  }
}

function saveNextCheck(interval) {
  saveLocalCache(NEXT_CHECK_CACHE_KEY, {
    interval,
    dueAt: Date.now() + (interval * 1000),
  });
}

function hydrateNextCheck() {
  const cached = readLocalCache(NEXT_CHECK_CACHE_KEY);
  if (!cached || !cached.dueAt) {
    return false;
  }
  const remaining = Math.ceil((Number(cached.dueAt) - Date.now()) / 1000);
  if (!Number.isFinite(remaining) || remaining <= 0) {
    return false;
  }
  INTERVAL = Number(cached.interval) || NO_UPDATE_INTERVAL;
  countdown = Math.max(1, remaining);
  return true;
}

function uptimeClass(p) {
  if (p >= 95) return 'good';
  if (p >= 80) return 'warn';
  return 'bad';
}

const METRIC_STOPS = {
  online: [
    [0, 34, 197, 94],
    [300, 74, 185, 84],
    [700, 245, 158, 11],
    [1200, 245, 140, 8],
  ],
  unknown: [
    [0, 245, 158, 11],
    [260, 249, 115, 22],
    [1200, 234, 88, 12],
  ],
  offline: [
    [0, 249, 115, 22],
    [260, 245, 140, 8],
    [1200, 234, 88, 12],
  ],
};

function lerpMetricColor(stops, ms) {
  if (ms <= stops[0][0]) {
    const [, r, g, b] = stops[0];
    return `rgb(${r},${g},${b})`;
  }
  const last = stops[stops.length - 1];
  if (ms >= last[0]) {
    const [, r, g, b] = last;
    return `rgb(${r},${g},${b})`;
  }
  for (let i = 1; i < stops.length; i += 1) {
    if (ms <= stops[i][0]) {
      const [ms0, r0, g0, b0] = stops[i - 1];
      const [ms1, r1, g1, b1] = stops[i];
      const t = (ms - ms0) / (ms1 - ms0);
      const r = Math.round(r0 + (r1 - r0) * t);
      const g = Math.round(g0 + (g1 - g0) * t);
      const b = Math.round(b0 + (b1 - b0) * t);
      return `rgb(${r},${g},${b})`;
    }
  }
  return '';
}

function responseMetricStyle(status, ms) {
  if (!ms || ms <= 0) return '';
  const stops = METRIC_STOPS[status] || METRIC_STOPS.unknown;
  return `color:${lerpMetricColor(stops, ms)}`;
}

function calcUptime(hist) {
  if (!hist.length) return null;
  return hist.filter(item => item.status === 'online').length / hist.length * 100;
}

const HISTORY_STOPS = {
  online: [
    [0, 34, 197, 94, 0.95],
    [300, 74, 185, 84, 0.92],
    [700, 245, 158, 11, 0.90],
    [1200, 245, 140, 8, 0.90],
  ],
  unknown: [
    [0, 249, 115, 22, 0.88],
    [1200, 234, 88, 12, 0.92],
  ],
};

function lerpHistoryColor(stops, ms) {
  if (ms <= stops[0][0]) {
    const [, r, g, b, a] = stops[0];
    return `rgba(${r},${g},${b},${a})`;
  }
  const last = stops[stops.length - 1];
  if (ms >= last[0]) {
    const [, r, g, b, a] = last;
    return `rgba(${r},${g},${b},${a})`;
  }
  for (let i = 1; i < stops.length; i += 1) {
    if (ms <= stops[i][0]) {
      const [ms0, r0, g0, b0, a0] = stops[i - 1];
      const [ms1, r1, g1, b1, a1] = stops[i];
      const t = (ms - ms0) / (ms1 - ms0);
      const r = Math.round(r0 + (r1 - r0) * t);
      const g = Math.round(g0 + (g1 - g0) * t);
      const b = Math.round(b0 + (b1 - b0) * t);
      const a = (a0 + (a1 - a0) * t).toFixed(2);
      return `rgba(${r},${g},${b},${a})`;
    }
  }
  return '';
}

function histColor(item) {
  if (item.status === 'offline') return 'rgba(239,68,68,0.88)';
  if (item.status !== 'online') return lerpHistoryColor(HISTORY_STOPS.unknown, item.response_time || 0);
  return lerpHistoryColor(HISTORY_STOPS.online, item.response_time || 0);
}

function decodeSummaryRow(row) {
  return {
    key: row[0] || '',
    name: row[1] || '--',
    subtitle: row[2] || '',
    group: row[3] || '',
    type: TYPE_CODES[row[4]] || 'unknown',
    status: STATUS_CODES[row[5]] || 'unknown',
    response_time: Number(row[6] || 0),
    message: row[7] || '',
    uptimes: {
      '24h': Number.isFinite(row[8]) ? Number(row[8]) : -1,
      '7d': Number.isFinite(row[9]) ? Number(row[9]) : -1,
      '30d': Number.isFinite(row[10]) ? Number(row[10]) : -1,
    },
    meta: row[11] || '',
  };
}

function hydrateSummaryCache() {
  const cached = readLocalCache(SUMMARY_CACHE_KEY);
  if (!cached || !Array.isArray(cached.s)) {
    return false;
  }
  allData = cached.s.map(decodeSummaryRow);
  lastRefreshedAt = cached.t || 0;
  renderAll();
  return allData.length > 0;
}

function decodeHistoryPoints(points) {
  return Array.isArray(points) ? points.map(point => ({
    ts: Number(point[0] || 0),
    status: STATUS_CODES[point[1]] || 'unknown',
    response_time: Number(point[2] || 0),
  })) : [];
}

function formatTime(ts) {
  if (!ts) return '--';
  return new Date(ts * 1000).toLocaleString('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  });
}

function formatRefreshTime(ts) {
  if (!ts) return '--';
  return new Date(ts * 1000).toLocaleTimeString('zh-CN', { hour12: false });
}

function historyForCurrentRange(key) {
  return historyStore[rangeKey][key] || [];
}

function uptimeForRange(service) {
  const value = service?.uptimes?.[rangeKey];
  if (value >= 0) {
    return value / 100;
  }
  return calcUptime(historyForCurrentRange(service.key));
}

window.setRange = function setRange(btn, key) {
  rangeKey = key;
  document.querySelectorAll('.range-btn').forEach(item => item.classList.remove('active'));
  btn.classList.add('active');
  document.getElementById('ovPeriod').textContent = RANGE_LABEL[key];
  renderAll();
  syncRangeHistory(key);
};

function renderAll() {
  if (!allData.length) return;
  renderGroups(allData);
  updateOverall(allData);
}

function groupServices(list) {
  const groups = [];
  const seen = new Map();
  list.forEach(service => {
    const name = (service.group || '未分组').trim() || '未分组';
    if (!seen.has(name)) {
      const group = { name, services: [] };
      seen.set(name, group);
      groups.push(group);
    }
    seen.get(name).services.push(service);
  });
  return groups;
}

function groupStatus(list) {
  if (!list.length) return 'loading';
  if (list.some(service => service.status === 'offline')) return 'offline';
  if (list.every(service => service.status === 'online')) return 'online';
  return 'unknown';
}

function renderCard(service) {
  const status = service.status || 'unknown';
  const responseTime = service.response_time || 0;
  const responseText = responseTime > 0 ? `${responseTime} ms` : '--';
  const badgeText = STATUS_LABELS[status] || '未知';

  const points = historyForCurrentRange(service.key);
  const uptime = uptimeForRange(service);
  const uptimeText = uptime != null ? `${uptime.toFixed(1)}%` : '--';
  const uptimeCls = uptime != null ? uptimeClass(uptime) : '';

  const pad = Math.max(0, SLOTS - points.length);
  const blocks = [
    ...Array(pad).fill('<div class="hb empty"></div>'),
    ...points.map(point => {
      const tip = `${STATUS_LABELS[point.status] || '未知'} · ${formatTime(point.ts)} · ${point.response_time > 0 ? `${point.response_time}ms` : '--'}`;
      return `<div class="hb" style="background:${histColor(point)}" data-tip="${tip}"></div>`;
    }),
  ].join('');

  const historyStart = points.length ? formatTime(points[0].ts) : RANGE_LABEL[rangeKey];
  const historyEnd = points.length ? formatTime(points[points.length - 1].ts) : '等待加载';
  const metaText = formatRefreshTime(lastRefreshedAt);

  return `
    <div class="card ${status}">
      <div class="card-info">
        <div class="card-header">
          <div class="card-title-wrap">
            <span class="card-name">${service.name}</span>
            ${service.subtitle ? `<div class="card-title-sub">${service.subtitle}</div>` : ''}
          </div>
          <span class="badge ${status}"><span class="dot"></span>${badgeText}</span>
        </div>
        <div class="card-sub">${typeLabels[service.type] || service.type} · ${metaText}</div>
        ${service.message ? `<div class="card-msg">· ${service.message}</div>` : ''}
      </div>
      <div>
        <div class="card-metrics">
          <div class="metric">
            <div class="metric-val" style="${responseMetricStyle(status, responseTime)}">${responseText}</div>
            <div class="metric-label">响应延迟</div>
          </div>
          <div class="metric">
            <div class="metric-val ${uptimeCls}">${uptimeText}</div>
            <div class="metric-label">可用率</div>
          </div>
        </div>
      </div>
      <div class="history-col">
        <div class="history-top">
          <span class="history-time">${historyStart}</span>
          <span class="history-pct ${uptimeCls}">${uptimeText}</span>
          <span class="history-time">${historyEnd}</span>
        </div>
        <div class="history-blocks" style="--slots:${SLOTS}">${blocks}</div>
      </div>
    </div>`;
}

function renderGroups(list) {
  const groups = groupServices(list);
  document.getElementById('grid').innerHTML = groups.map(group => {
    const total = group.services.length;
    const online = group.services.filter(service => service.status === 'online').length;
    const issues = group.services.filter(service => service.status !== 'online').length;
    const groupState = groupStatus(group.services);
    const meta = issues === 0
      ? `${online} / ${total} 在线`
      : `${online} / ${total} 在线 · ${issues} 项需关注`;

    return `
      <section class="group-block">
        <div class="group-head">
          <div class="group-title">
            <span class="group-mark ${groupState}"></span>
            <span class="group-name">${group.name}</span>
          </div>
          <div class="group-meta">${meta}</div>
        </div>
        <div class="group-list">
          ${group.services.map(service => renderCard(service)).join('')}
        </div>
      </section>`;
  }).join('');
}

function updateOverall(list) {
  const total = list.length;
  const online = list.filter(service => service.status === 'online').length;
  const offline = list.filter(service => service.status === 'offline').length;

  const uptimes = list
    .map(service => uptimeForRange(service))
    .filter(value => value != null);
  const avg = uptimes.length
    ? uptimes.reduce((sum, value) => sum + value, 0) / uptimes.length
    : null;

  const valueEl = document.getElementById('ovUptime');
  valueEl.textContent = avg != null ? `${avg.toFixed(2)}%` : '--%';
  valueEl.className = 'ov-val' + (avg != null ? ` ${uptimeClass(avg)}` : '');

  const dot = document.getElementById('summaryBreath');
  dot.className = 'breath ' + (offline > 0 ? 'red' : online === total ? 'green' : 'yellow');
  document.getElementById('summaryText').textContent = offline === 0 ? '全部正常' : `${offline} 个异常`;
  document.getElementById('summaryMeta').textContent = `${online} / ${total} 在线`;
  document.getElementById('ovPeriod').textContent = RANGE_LABEL[rangeKey];
}

function summarySignature(service) {
  return JSON.stringify([
    service.name,
    service.subtitle,
    service.group,
    service.type,
    service.status,
    service.response_time,
    service.message,
    service.uptimes['24h'],
    service.uptimes['7d'],
    service.uptimes['30d'],
    service.meta,
  ]);
}

function diffChangedKeys(prevList, nextList) {
  const prevMap = new Map(prevList.map(service => [service.key, summarySignature(service)]));
  const nextKeys = new Set(nextList.map(service => service.key));
  const changed = nextList
    .filter(service => prevMap.get(service.key) !== summarySignature(service))
    .map(service => service.key);

  Object.keys(historyStore).forEach(range => {
    Object.keys(historyStore[range]).forEach(key => {
      if (!nextKeys.has(key)) {
        delete historyStore[range][key];
      }
    });
  });

  return changed;
}

function chunk(array, size) {
  const result = [];
  for (let index = 0; index < array.length; index += size) {
    result.push(array.slice(index, index + size));
  }
  return result;
}

async function requestJSON(url) {
  return fetch(url, {
    method: 'GET',
    cache: 'no-store',
  });
}

async function syncRangeHistory(range, changedKeys = [], forceAll = false) {
  if (!allData.length) return;

  const seq = ++historyRequestSeq;
  const keys = allData.map(service => service.key).filter(Boolean);
  const nextState = { ...historyStore[range] };
  const stale = historyStamp[range] !== lastRefreshedAt;
  const wanted = (forceAll || stale)
    ? keys.slice()
    : Array.from(new Set([
      ...changedKeys.filter(key => keys.includes(key)),
      ...keys.filter(key => !nextState[key]),
    ]));

  Object.keys(nextState).forEach(key => {
    if (!keys.includes(key)) {
      delete nextState[key];
    }
  });

  if (!wanted.length) {
    if (range === rangeKey) {
      renderAll();
    }
    return;
  }

  try {
    for (const keysChunk of chunk(wanted, HISTORY_CHUNK_SIZE)) {
      const query = new URLSearchParams({
        view: 'history',
        range,
        slots: String(SLOTS),
        keys: keysChunk.join(','),
      });
      const response = await requestJSON(`${API_URL}?${query.toString()}`);
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }

      const payload = await response.json();
      if (!payload || !Array.isArray(payload.h)) {
        throw new Error('history 数据格式异常');
      }

      payload.h.forEach(row => {
        nextState[row[0]] = decodeHistoryPoints(row[1]);
      });

      if (seq !== historyRequestSeq) {
        return;
      }

      historyStore[range] = { ...nextState };
      if (range === rangeKey) {
        renderAll();
      }
    }
  } catch (_) {
    if (seq === historyRequestSeq && range === rangeKey) {
      renderAll();
    }
    return;
  }

  if (seq !== historyRequestSeq) {
    return;
  }

  historyStore[range] = nextState;
  historyStamp[range] = lastRefreshedAt;
  if (range === rangeKey) {
    renderAll();
  }
}

function fetchStatus(force = false) {
  if (fetching) return;
  fetching = true;

  const params = new URLSearchParams({ view: 'summary' });
  if (!force && lastRefreshedAt) {
    params.set('since', String(lastRefreshedAt));
  }

  return requestJSON(`${API_URL}?${params.toString()}`)
    .then(response => {
      if (response.status === 304) {
        INTERVAL = NO_UPDATE_INTERVAL;
        countdown = INTERVAL;
        saveNextCheck(INTERVAL);
        return null;
      }
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      return response.json();
    })
    .then(payload => {
      if (payload == null) {
        return syncRangeHistory(rangeKey);
      }
      if (!payload || !Array.isArray(payload.s)) {
        throw new Error('summary 数据格式异常');
      }

      INTERVAL = UPDATED_INTERVAL;
      const nextRefreshedAt = payload.t || 0;
      countdown = INTERVAL;
      const nextData = payload.s.map(decodeSummaryRow);
      const changedKeys = diffChangedKeys(allData, nextData);
      const forceAll = nextRefreshedAt !== lastRefreshedAt;

      allData = nextData;
      lastRefreshedAt = nextRefreshedAt;
      saveLocalCache(SUMMARY_CACHE_KEY, payload);
      saveNextCheck(INTERVAL);

      renderAll();
      return syncRangeHistory(rangeKey, changedKeys, forceAll);
    })
    .catch(() => {
      countdown = Math.max(INTERVAL, NO_UPDATE_INTERVAL);
      saveNextCheck(NO_UPDATE_INTERVAL);
      document.getElementById('summaryText').textContent = '获取失败';
    })
    .finally(() => {
      fetching = false;
    });
}

setInterval(() => {
  if (fetching) return;
  countdown = Math.max(0, countdown - 1);
  document.getElementById('countdownFill').style.width = `${(countdown / INTERVAL) * 100}%`;
  document.getElementById('countdownLabel').textContent = countdown > 0 ? `${countdown} 秒` : '刷新中...';
  if (countdown === 0) fetchStatus();
}, 1000);

document.getElementById('ovPeriod').textContent = RANGE_LABEL[rangeKey];
document.getElementById('fyear').textContent = new Date().getFullYear();

const hasSummaryCache = hydrateSummaryCache();
const hasNextCheckCache = hydrateNextCheck();
if (hasSummaryCache) {
  syncRangeHistory(rangeKey);
}
if (!hasSummaryCache || !hasNextCheckCache) {
  fetchStatus();
}
