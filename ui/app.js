document.addEventListener('DOMContentLoaded', () => {
  const userBadge = document.getElementById('user-badge');
  const userName = document.getElementById('user-name');
  const userMode = document.getElementById('user-mode');
  const connBadge = document.getElementById('conn-badge');
  const infoMode = document.getElementById('info-mode');
  const infoUser = document.getElementById('info-user');
  const infoUpstream = document.getElementById('info-upstream');
  const infoKey = document.getElementById('info-key');
  const infoFilters = document.getElementById('info-filters');
  const infoUptime = document.getElementById('info-uptime');
  const usageBody = document.getElementById('usage-body');
  const usageTotal = document.getElementById('usage-total');
  const logsBody = document.getElementById('logs-body');
  const refreshLogs = document.getElementById('refresh-logs');
  const apiResponse = document.getElementById('api-response');

  function escapeHtml(value) {
    const div = document.createElement('div');
    div.textContent = value == null ? '' : String(value);
    return div.innerHTML;
  }

  async function fetchJSON(url, options) {
    const res = await fetch(url, options);
    const data = await res.json();
    if (!res.ok) {
      throw new Error((data && data.error && data.error.message) || `Request to ${url} failed (${res.status})`);
    }
    return data;
  }

  function formatNumber(n) {
    return Number(n || 0).toLocaleString();
  }

  function formatTime(value) {
    if (!value) return '-';
    const d = new Date(value);
    return Number.isNaN(d.getTime()) ? escapeHtml(value) : d.toLocaleString();
  }

  function statusBadge(status) {
    const cls = status >= 500 || status >= 400 ? 'text-bg-danger'
      : status >= 300 ? 'text-bg-warning'
      : 'text-bg-success';
    return `<span class="badge ${cls}">${escapeHtml(status)}</span>`;
  }

  async function loadUser() {
    try {
      const data = await fetchJSON('api/v1/user');
      userName.textContent = data.username;
      userMode.textContent = `${data.mode} mode (${data.auth_type})`;
      infoUser.textContent = data.username;
      connBadge.classList.remove('text-bg-danger');
      connBadge.classList.add('text-bg-success');
      connBadge.textContent = 'connected';
      userBadge.style.opacity = '1';
    } catch (err) {
      userName.textContent = 'Offline / Disconnected';
      userMode.textContent = 'connection failed';
      infoUser.textContent = '-';
      connBadge.classList.remove('text-bg-success');
      connBadge.classList.add('text-bg-danger');
      connBadge.textContent = 'disconnected';
    }
  }

  async function loadSystemInfo() {
    try {
      const data = await fetchJSON('api/v1/info');
      infoMode.textContent = data.mode;
      infoUptime.textContent = data.uptime;
      infoUpstream.textContent = data.upstream || 'not configured';
      if (data.api_key_configured) {
        infoKey.textContent = 'configured';
        infoKey.classList.remove('text-danger');
      } else {
        infoKey.textContent = 'not set (client keys pass through)';
        infoKey.classList.add('text-danger');
      }
      if (!data.allowed_models && !data.blocked_models) {
        infoFilters.textContent = 'none (all models allowed)';
      } else {
        infoFilters.textContent = `${data.allowed_models} allowed / ${data.blocked_models} blocked`;
      }
    } catch (err) {
      infoMode.textContent = '-';
      infoUptime.textContent = '-';
      infoUpstream.textContent = '-';
      infoKey.textContent = '-';
      infoFilters.textContent = '-';
    }
  }

  async function loadUsage() {
    try {
      const data = await fetchJSON('api/v1/usage');
      usageTotal.textContent = `${formatNumber(data.total.requests)} request${data.total.requests === 1 ? '' : 's'}`;
      usageBody.innerHTML = '';
      if (!data.users || data.users.length === 0) {
        usageBody.innerHTML = '<tr><td colspan="6" class="text-center text-secondary">No activity recorded yet.</td></tr>';
        return;
      }
      data.users.forEach((u) => {
        const tr = document.createElement('tr');
        tr.innerHTML =
          `<td class="fw-semibold">${escapeHtml(u.user)}</td>` +
          `<td class="text-end">${formatNumber(u.requests)}</td>` +
          `<td class="text-end">${formatNumber(u.prompt_tokens)}</td>` +
          `<td class="text-end">${formatNumber(u.completion_tokens)}</td>` +
          `<td class="text-end">${formatNumber(u.total_tokens)}</td>` +
          `<td>${formatTime(u.last_activity)}</td>`;
        usageBody.appendChild(tr);
      });
    } catch (err) {
      usageBody.innerHTML = '<tr><td colspan="6" class="text-center text-secondary">Failed to load usage.</td></tr>';
    }
  }

  async function loadLogs() {
    try {
      const data = await fetchJSON('api/v1/logs?limit=50');
      logsBody.innerHTML = '';
      if (!data.entries || data.entries.length === 0) {
        logsBody.innerHTML = '<tr><td colspan="7" class="text-center text-secondary">No proxied requests yet.</td></tr>';
        return;
      }
      data.entries.forEach((e) => {
        const tr = document.createElement('tr');
        const endpoint = `${escapeHtml(e.method)} ${escapeHtml(e.path)}`;
        const model = e.model ? escapeHtml(e.model) : '<span class="text-secondary">-</span>';
        const stream = e.stream ? ' <span class="badge text-bg-info">stream</span>' : '';
        tr.innerHTML =
          `<td>${formatTime(e.time)}</td>` +
          `<td>${escapeHtml(e.user)}</td>` +
          `<td class="font-monospace small">${endpoint}</td>` +
          `<td>${model}${stream}</td>` +
          `<td>${statusBadge(e.status)}</td>` +
          `<td class="text-end">${formatNumber(e.duration_ms)} ms</td>` +
          `<td class="text-end">${formatNumber(e.total_tokens)}</td>`;
        logsBody.appendChild(tr);
      });
    } catch (err) {
      logsBody.innerHTML = '<tr><td colspan="7" class="text-center text-secondary">Failed to load logs.</td></tr>';
    }
  }

  document.querySelectorAll('[data-endpoint]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const endpoint = btn.dataset.endpoint;
      apiResponse.textContent = 'Fetching API response...';
      try {
        const data = await fetchJSON(endpoint);
        apiResponse.textContent = JSON.stringify(data, null, 2);
      } catch (err) {
        apiResponse.textContent = `Error calling ${endpoint}: ${err.message}`;
      }
    });
  });

  refreshLogs.addEventListener('click', loadLogs);

  loadUser();
  loadSystemInfo();
  loadUsage();
  loadLogs();

  // Keep the dashboard current as requests stream through the proxy.
  setInterval(loadUsage, 5000);
  setInterval(loadLogs, 5000);
});
