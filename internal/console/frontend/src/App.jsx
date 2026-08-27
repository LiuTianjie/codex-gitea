import { useCallback, useEffect, useMemo, useState } from 'react'
import ReactEChartsCore from 'echarts-for-react/lib/core'
import { BarChart, LineChart as EChartsLineChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import * as echarts from 'echarts/core'
import { CanvasRenderer } from 'echarts/renderers'
import {
  Activity,
  BellRing,
  BarChart3,
  BookOpen,
  ChevronLeft,
  ChevronRight,
  Copy,
  Download,
  ExternalLink,
  LoaderCircle,
  MessageSquare,
  RefreshCw,
  RotateCcw,
  Save,
  Send,
  Settings,
  ShieldAlert,
  Sparkles,
  Trash2,
  Users,
  XCircle
} from 'lucide-react'

echarts.use([BarChart, EChartsLineChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

const REDACTED = '***set***'

const FIELDS = [
  'gitea_url',
  'gitea_token',
  'gitea_timeout',
  'webhook_secret',
  'model',
  'codex_reasoning_effort',
  'codex_base_url',
  'trigger_keywords',
  'concurrency',
  'codex_auth_mode',
  'codex_cc_switch_provider_id',
  'codex_api_key',
  'codex_sandbox_mode',
  'claude_enabled',
  'claude_model',
  'claude_api_key',
  'claude_base_url',
  'claude_home',
  'cc_switch_config_dir',
  'cc_switch_provider_id',
  'claude_max_budget_usd',
  'minimax_enabled',
  'minimax_model',
  'minimax_provider_id',
  'minimax_api_key',
  'minimax_base_url',
  'minimax_max_budget_usd',
  'repo_allowlist',
  'timeout',
  'analysis_git_fetch_depth',
  'analysis_cache_max_repositories',
  'analysis_cache_max_mb',
  'analysis_cache_max_idle',
  'analysis_worktree_ttl',
  'analysis_cache_cleanup_interval',
  'analysis_min_free_mb'
]

const FIELD_GROUPS = {
  common: ['gitea_url', 'gitea_token', 'gitea_timeout', 'webhook_secret', 'trigger_keywords', 'repo_allowlist', 'concurrency', 'timeout'],
  codex: ['model', 'codex_reasoning_effort', 'codex_base_url', 'codex_auth_mode', 'codex_cc_switch_provider_id', 'codex_sandbox_mode', 'codex_api_key'],
  claude: ['claude_enabled', 'claude_model', 'claude_api_key', 'claude_base_url', 'claude_home', 'cc_switch_config_dir', 'cc_switch_provider_id', 'claude_max_budget_usd'],
  minimax: ['minimax_enabled', 'minimax_model', 'minimax_provider_id', 'minimax_api_key', 'minimax_base_url', 'minimax_max_budget_usd'],
  analysisCache: ['analysis_git_fetch_depth', 'analysis_cache_max_repositories', 'analysis_cache_max_mb', 'analysis_cache_max_idle', 'analysis_worktree_ttl', 'analysis_cache_cleanup_interval', 'analysis_min_free_mb']
}

const DEFAULT_SETTINGS = {
  codex_auth_mode: 'ccswitch',
  codex_reasoning_effort: 'high',
  claude_model: 'sonnet',
  claude_home: '/claude-home',
  cc_switch_config_dir: '/cc-switch',
  claude_max_budget_usd: '0.3',
  minimax_max_budget_usd: '0.3',
  analysis_git_fetch_depth: '200',
  analysis_cache_max_repositories: '3',
  analysis_cache_max_mb: '5120',
  analysis_cache_max_idle: '168h',
  analysis_worktree_ttl: '1h',
  analysis_cache_cleanup_interval: '10m',
  analysis_min_free_mb: '1024'
}

const DEFAULT_REASONING_EFFORTS = ['minimal', 'low', 'medium', 'high', 'xhigh']

const SECRET_FIELDS = new Set(['gitea_token', 'webhook_secret', 'codex_api_key', 'claude_api_key', 'minimax_api_key'])

const SETTING_META = {
  gitea_url: { label: 'Gitea URL', placeholder: 'https://gcode.example.com' },
  gitea_token: { label: 'Gitea Token', secret: true },
  gitea_timeout: { label: 'Gitea Timeout', placeholder: '90s' },
  webhook_secret: { label: 'Webhook Secret', secret: true },
  model: { label: 'Codex Model', placeholder: 'gpt-5-codex' },
  codex_reasoning_effort: { label: 'Codex 思考强度', type: 'select', options: DEFAULT_REASONING_EFFORTS },
  codex_base_url: { label: 'Codex Base URL', placeholder: 'https://relay.example.com/v1' },
  trigger_keywords: { label: '触发关键词', placeholder: '/review,@review' },
  repo_allowlist: { label: '仓库白名单', placeholder: 'owner/repo,owner/repo2' },
  concurrency: { label: 'Worker 并发', placeholder: '5' },
  timeout: { label: 'Review Timeout', placeholder: '30m' },
  codex_auth_mode: { label: 'Codex Auth Mode', type: 'select', options: ['ccswitch', 'authfile', 'apikey'] },
  codex_cc_switch_provider_id: { label: 'Codex cc-switch Provider' },
  codex_sandbox_mode: { label: 'Codex Sandbox', placeholder: 'read-only' },
  codex_api_key: { label: 'Codex API Key', secret: true },
  claude_enabled: { label: '启用 Claude', type: 'select', options: ['false', 'true'] },
  claude_model: { label: 'Claude Model' },
  claude_api_key: { label: 'Claude API Key', secret: true },
  claude_base_url: { label: 'Claude Base URL' },
  claude_home: { label: 'Claude Home' },
  cc_switch_config_dir: { label: 'cc-switch 配置目录' },
  cc_switch_provider_id: { label: 'cc-switch Provider' },
  claude_max_budget_usd: { label: 'Claude 预算 USD' },
  minimax_enabled: { label: '启用 MiniMax', type: 'select', options: ['false', 'true'] },
  minimax_model: { label: 'MiniMax Model' },
  minimax_provider_id: { label: 'MiniMax Provider' },
  minimax_api_key: { label: 'MiniMax API Key', secret: true },
  minimax_base_url: { label: 'MiniMax Base URL' },
  minimax_max_budget_usd: { label: 'MiniMax 预算 USD' },
  analysis_git_fetch_depth: { label: '浅拉提交深度', placeholder: '200' },
  analysis_cache_max_repositories: { label: '最多缓存仓库数', placeholder: '3' },
  analysis_cache_max_mb: { label: '缓存容量上限（MB）', placeholder: '5120' },
  analysis_cache_max_idle: { label: '仓库最大闲置时间', placeholder: '168h' },
  analysis_worktree_ttl: { label: '残留 Worktree TTL', placeholder: '1h' },
  analysis_cache_cleanup_interval: { label: '自动清理周期', placeholder: '10m' },
  analysis_min_free_mb: { label: '最低磁盘余量（MB）', placeholder: '1024；0 表示关闭水位保护' }
}

const TABS = [
  { id: 'jobs', label: '任务', icon: Activity },
  { id: 'alert_tasks', label: '告警分析', icon: ShieldAlert },
  { id: 'alert_configs', label: '告警配置', icon: BellRing },
  { id: 'chat', label: '对话', icon: MessageSquare },
  { id: 'analytics', label: '分析', icon: BarChart3 },
  { id: 'skills', label: 'Skill', icon: BookOpen },
  { id: 'config', label: '配置', icon: Settings }
]

async function fetchJSON(url, options = {}, timeoutMs = 10000) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const response = await fetch(url, { credentials: 'same-origin', ...options, signal: controller.signal })
    const text = await response.text()
    let body = null
    if (text.trim()) {
      try {
        body = JSON.parse(text)
      } catch {
        body = { error: text }
      }
    }
    if (!response.ok) {
      if (response.status === 401) {
        const error = new Error('登录已过期，请重新登录')
        error.status = response.status
        throw error
      }
      throw new Error(body?.error || body?.status || `${response.status} ${response.statusText}`)
    }
    return body
  } finally {
    clearTimeout(timer)
  }
}

function prettyTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return String(value).replace('T', ' ').replace('Z', '')
  return date.toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false
  }).replaceAll('/', '-')
}

function percent(value) {
  if (typeof value !== 'number') return '--'
  return `${Math.round(value * 1000) / 10}%`
}

function byPreferredOrder(order) {
  const rank = new Map(order.map((item, index) => [item, index]))
  return ([a], [b]) => (rank.get(a) ?? 999) - (rank.get(b) ?? 999) || a.localeCompare(b)
}

function uniqueValues(values) {
  const out = []
  values.forEach((value) => {
    const item = String(value || '').trim()
    if (item && !out.includes(item)) out.push(item)
  })
  return out
}

function encodePath(path) {
  return String(path || '').split('/').map(encodeURIComponent).join('/')
}

function sourceURL(baseURL, finding) {
  if (!baseURL || !finding?.owner || !finding?.repo || !finding?.path || !finding?.last_seen_sha) return ''
  let url = `${String(baseURL).replace(/\/+$/, '')}/${encodeURIComponent(finding.owner)}/${encodeURIComponent(finding.repo)}/src/commit/${encodeURIComponent(finding.last_seen_sha)}/${encodePath(finding.path)}`
  if (finding.line) url += `#L${encodeURIComponent(finding.line)}`
  return url
}

const STATUS_LABELS = {
  queued: '排队中', pending: '等待中', running: '运行中', cancel_requested: '取消中',
  succeeded: '成功', failed: '失败', canceled: '已取消', suppressed: '重复告警', superseded: '已替代',
  critical: '严重', high: '高', medium: '中', low: '低', info: '提示',
  open: '待处理', fixed: '已修复', dismissed: '已忽略', completed: '已完成', unknown: '未知',
  enabled: '已启用', disabled: '已停用'
}

const PHASE_LABELS = {
  queued: '等待执行', fetching_logs: '获取原始日志', logs_ready: '原始日志已获取',
  preparing_repository: '准备代码仓库', repository_ready: '代码版本已准备',
  analyzing: '分析代码与提交', succeeded: '分析完成', failed: '分析失败',
  cancel_requested: '正在取消', canceled: '已取消', suppressed: '重复告警，已分析'
}

function StatusBadge({ status }) {
  return <span className={`badge status-${status || 'unknown'}`}>{STATUS_LABELS[status] || status || '-'}</span>
}

function Message({ message }) {
  if (!message) return null
  return <div className={`message ${message.ok ? 'ok' : 'err'}`}>{message.text}</div>
}

function IconButton({ icon: Icon, children, className = '', ...props }) {
  return (
    <button className={`button ${className}`.trim()} type="button" {...props}>
      {Icon ? <Icon size={17} strokeWidth={2.2} /> : null}
      <span>{children}</span>
    </button>
  )
}

function StatCard({ label, value, hint }) {
  return (
    <div className="stat-card">
      <span>{label}</span>
      <strong>{value}</strong>
      <small>{hint}</small>
    </div>
  )
}

function App() {
  const initialAnalysisTask = new URLSearchParams(window.location.search).get('analysis_task')
  const [activeTab, setActiveTab] = useState(initialAnalysisTask ? 'alert_tasks' : 'jobs')

  return (
    <div className="app-shell">
      <header className="topbar">
        <div>
          <p className="eyebrow">gitea-review-agent</p>
          <h1>控制台</h1>
        </div>
        <nav className="tabs" aria-label="控制台导航">
          {TABS.map((tab) => {
            const Icon = tab.icon
            return (
              <button key={tab.id} className={activeTab === tab.id ? 'active' : ''} type="button" onClick={() => setActiveTab(tab.id)}>
                <Icon size={17} />
                <span>{tab.label}</span>
              </button>
            )
          })}
        </nav>
        <a className="logout" href="/admin/logout">退出</a>
      </header>

      <main>
        {activeTab === 'jobs' ? <JobsPanel /> : null}
        {activeTab === 'alert_tasks' ? <AnalysisTasksPanel initialTaskId={initialAnalysisTask} /> : null}
        {activeTab === 'alert_configs' ? <AnalysisConfigsPanel /> : null}
        {activeTab === 'chat' ? <ChatProbePanel /> : null}
        {activeTab === 'analytics' ? <AnalyticsPanel /> : null}
        {activeTab === 'skills' ? <SkillsPanel /> : null}
        {activeTab === 'config' ? <ConfigPanel /> : null}
      </main>
    </div>
  )
}

function JobsPanel() {
  const [jobs, setJobs] = useState([])
  const [stats, setStats] = useState(null)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(false)
  const [message, setMessage] = useState(null)
  const [drawerJobId, setDrawerJobId] = useState(null)
  const [selectedJob, setSelectedJob] = useState(null)
  const [detailLoading, setDetailLoading] = useState(false)

  const loadStats = useCallback(async () => {
    const payload = await fetchJSON('/admin/api/jobs/stats', {}, 12000)
    setStats(payload)
  }, [])

  const loadJobs = useCallback(async (showBusy = false) => {
    if (showBusy) setLoading(true)
    try {
      const params = new URLSearchParams({ page: String(page), page_size: String(pageSize) })
      const payload = await fetchJSON(`/admin/api/jobs?${params.toString()}`, {}, 8000)
      setJobs(payload.jobs || [])
      setHasMore(Boolean(payload.has_more))
      setMessage(null)
    } catch (error) {
      setMessage({ ok: false, text: `加载任务失败：${error.message}` })
    } finally {
      if (showBusy) setLoading(false)
    }
  }, [page, pageSize])

  useEffect(() => {
    loadJobs(true)
    loadStats().catch((error) => setMessage({ ok: false, text: `加载统计失败：${error.message}` }))
  }, [loadJobs, loadStats])

  useEffect(() => {
    const timer = setInterval(() => {
      loadJobs(false)
      loadStats().catch(() => {})
    }, 10000)
    return () => clearInterval(timer)
  }, [loadJobs, loadStats])

  useEffect(() => {
    if (!drawerJobId) {
      setSelectedJob(null)
      return
    }
    let cancelled = false
    setDetailLoading(true)
    fetchJSON(`/admin/api/jobs/${encodeURIComponent(drawerJobId)}`, {}, 8000)
      .then((payload) => {
        if (!cancelled) setSelectedJob(payload)
      })
      .catch((error) => {
        if (!cancelled) setMessage({ ok: false, text: `加载任务详情失败：${error.message}` })
      })
      .finally(() => {
        if (!cancelled) setDetailLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [drawerJobId])

  const refresh = async () => {
    await Promise.all([loadJobs(true), loadStats()])
    if (drawerJobId) {
      const payload = await fetchJSON(`/admin/api/jobs/${encodeURIComponent(drawerJobId)}`, {}, 8000)
      setSelectedJob(payload)
    }
  }

  const rerunJob = async (id) => {
    try {
      const payload = await fetchJSON(`/admin/api/jobs/${encodeURIComponent(id)}/rerun`, { method: 'POST' }, 8000)
      setMessage({ ok: true, text: `已重新运行任务 #${payload.job?.id || id}` })
      setDrawerJobId(payload.job?.id || id)
      await refresh()
    } catch (error) {
      setMessage({ ok: false, text: `重新运行失败：${error.message}` })
    }
  }

  const cancelJob = async (id) => {
    try {
      const payload = await fetchJSON(`/admin/api/jobs/${encodeURIComponent(id)}/cancel`, { method: 'POST' }, 8000)
      setMessage({ ok: true, text: `已取消任务 #${payload.job?.id || id}` })
      setSelectedJob(payload.job || null)
      await refresh()
    } catch (error) {
      setMessage({ ok: false, text: `取消失败：${error.message}` })
    }
  }

  return (
    <section className="panel">
      <div className="section-head">
        <div>
          <h2>近期任务</h2>
          <p>当前页 {jobs.length} 条，自动刷新中</p>
        </div>
        <div className="toolbar">
          <IconButton icon={RefreshCw} onClick={refresh} disabled={loading}>立即刷新</IconButton>
          <IconButton icon={ChevronLeft} onClick={() => setPage((v) => Math.max(1, v - 1))} disabled={page <= 1}>上一页</IconButton>
          <span className="pager">{page}</span>
          <IconButton icon={ChevronRight} onClick={() => setPage((v) => v + 1)} disabled={!hasMore}>下一页</IconButton>
          <select value={pageSize} onChange={(event) => { setPage(1); setPageSize(Number(event.target.value)) }}>
            <option value="20">20 / 页</option>
            <option value="50">50 / 页</option>
            <option value="100">100 / 页</option>
          </select>
        </div>
      </div>

      <JobStats stats={stats} />
      <Message message={message} />

      <div className="table-shell">
        <table className="data-table job-table">
          <thead>
            <tr>
              <th>ID</th>
              <th>PR</th>
              <th>事件</th>
              <th>动作</th>
              <th>状态</th>
              <th>次数</th>
              <th>创建时间</th>
              <th>开始时间</th>
              <th>会话</th>
              <th>日志</th>
            </tr>
          </thead>
          <tbody>
            {jobs.length ? jobs.map((job) => (
              <tr key={job.id}>
                <td>{job.id}</td>
                <td>{job.owner}/{job.repo}#{job.number}</td>
                <td>{job.event}</td>
                <td>{job.action || '-'}</td>
                <td><StatusBadge status={job.status} /></td>
                <td>{job.attempts}</td>
                <td>{prettyTime(job.created_at)}</td>
                <td>{prettyTime(job.started_at)}</td>
                <td><code>{job.session_id || '-'}</code></td>
                <td>
                  <button
                    className={job.error ? 'log-pill has-error' : 'log-pill'}
                    type="button"
                    onClick={() => setDrawerJobId(job.id)}
                  >
                    {job.log_count || 0} logs{job.error ? ' + error' : ''}
                  </button>
                </td>
              </tr>
            )) : (
              <tr><td colSpan="10" className="empty-cell">{loading ? '加载中...' : '暂无任务'}</td></tr>
            )}
          </tbody>
        </table>
      </div>

      <JobLogsDrawer
        open={Boolean(drawerJobId)}
        job={selectedJob}
        loading={detailLoading}
        onClose={() => setDrawerJobId(null)}
        onRerun={rerunJob}
        onCancel={cancelJob}
      />
    </section>
  )
}

function JobStats({ stats }) {
  const successRate = stats ? percent(stats.success_rate) : '--'
  return (
    <div className="stats-grid">
      <StatCard label="成功率" value={successRate} hint="done / done+failed" />
      <StatCard label="审核总数" value={stats?.review_jobs ?? '-'} hint="pull_request jobs" />
      <StatCard label="成功审核" value={stats?.done ?? '-'} hint="done" />
      <StatCard label="失败" value={stats?.failed ?? '-'} hint="需要排查" />
      <StatCard label="运行/等待" value={(stats?.running ?? 0) + (stats?.pending ?? 0)} hint="running + pending" />
      <StatCard label="可重试等待" value={stats?.retryable_pending ?? '-'} hint="retryable pending" />
      <StatCard label="已替换/取消" value={(stats?.superseded ?? 0) + (stats?.canceled ?? 0)} hint="superseded + canceled" />
    </div>
  )
}

function JobLogsDrawer({ open, job, loading, onClose, onRerun, onCancel }) {
  useEffect(() => {
    if (!open) return undefined
    const onKeyDown = (event) => {
      if (event.key === 'Escape') onClose?.()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [open, onClose])

  if (!open) return null
  const logs = [...(job?.logs || [])]
  if (job?.error) logs.push({ stage: 'error', message: job.error, created_at: job.finished_at || job.created_at })

  return (
    <div className="drawer-layer" role="dialog" aria-modal="true" aria-label="任务日志">
      <button className="drawer-backdrop" type="button" aria-label="关闭日志" onClick={onClose} />
      <aside className="log-drawer">
        <div className="log-drawer-head">
          <div>
            <span className="drawer-eyebrow">Job Logs</span>
            <h2>{job ? `任务 #${job.id}` : '任务日志'}</h2>
            <p>{job ? `${job.owner}/${job.repo}#${job.number}` : '正在读取任务详情...'}</p>
          </div>
          <button className="drawer-close" type="button" aria-label="关闭日志" onClick={onClose}>
            <XCircle size={20} />
          </button>
        </div>

        {loading ? (
          <div className="drawer-loading">日志加载中...</div>
        ) : job ? (
          <>
            <div className="log-drawer-summary">
              <StatusBadge status={job.status} />
              <span>{job.event}</span>
              <span>{job.action || '-'}</span>
              <span>{job.attempts} attempts</span>
              {job.error_type ? <span>{job.error_type}</span> : null}
            </div>

            <div className="log-drawer-actions">
              {job.status === 'pending' ? <IconButton icon={XCircle} className="danger" onClick={() => onCancel(job.id)}>取消</IconButton> : null}
              <IconButton icon={RotateCcw} onClick={() => onRerun(job.id)}>重新运行</IconButton>
            </div>
            <dl className="log-meta">
              <div><dt>创建</dt><dd>{prettyTime(job.created_at)}</dd></div>
              <div><dt>开始</dt><dd>{prettyTime(job.started_at)}</dd></div>
              <div><dt>完成</dt><dd>{prettyTime(job.finished_at)}</dd></div>
              <div><dt>下次重试</dt><dd>{prettyTime(job.next_attempt_at)}</dd></div>
              <div className="wide"><dt>会话</dt><dd><code>{job.session_id || '-'}</code></dd></div>
            </dl>

            <div className="log-stream" role="log" aria-live="polite">
              {logs.length ? logs.map((log, index) => (
                <div className={log.stage === 'error' ? 'log-line error' : 'log-line'} key={`${log.stage}-${log.created_at}-${index}`}>
                  <time>{prettyTime(log.created_at)}</time>
                  <strong>{log.stage}</strong>
                  <pre>{log.message}</pre>
                </div>
              )) : <p className="muted">暂无日志</p>}
            </div>
          </>
        ) : (
          <div className="drawer-loading">未找到任务详情。</div>
        )}
      </aside>
    </div>
  )
}

const EMPTY_ANALYSIS_CONFIG = {
  name: '', enabled: true, repository_url: '', repository_ref: 'main',
  sls_endpoint: '', sls_project: '', sls_logstore: '',
  sls_access_key_id: '', sls_access_key_secret: '',
  feishu_mode: 'webhook', feishu_webhook: '', feishu_app_id: '', feishu_app_secret: '', feishu_chat_id: '',
  model: '', reasoning_effort: 'high', concurrency: 2, timeout_seconds: 1800,
  log_window_seconds: 180, prompt: '', throttle_enabled: true,
  throttle_threshold: 1, throttle_cooldown_seconds: 0,
  throttle_fields: 'method,endpoint,error_code,error_message'
}

function AnalysisTasksPanel({ initialTaskId }) {
  const [tasks, setTasks] = useState([])
  const [configs, setConfigs] = useState([])
  const [status, setStatus] = useState('')
  const [configId, setConfigId] = useState('')
  const [page, setPage] = useState(1)
  const [hasMore, setHasMore] = useState(false)
  const [selectedId, setSelectedId] = useState(initialTaskId || null)
  const [detail, setDetail] = useState(null)
  const [message, setMessage] = useState(null)

  const load = useCallback(async () => {
    const params = new URLSearchParams({ page: String(page), page_size: '30' })
    if (status) params.set('status', status)
    if (configId) params.set('config_id', configId)
    const [taskPayload, configPayload] = await Promise.all([
      fetchJSON(`/admin/api/alert-analysis/tasks?${params.toString()}`, {}, 8000),
      fetchJSON('/admin/api/alert-analysis/configs', {}, 8000)
    ])
    setTasks(taskPayload.tasks || [])
    setHasMore(Boolean(taskPayload.has_more))
    setConfigs(configPayload.configs || [])
  }, [page, status, configId])

  const loadDetail = useCallback(async () => {
    if (!selectedId) {
      setDetail(null)
      return
    }
    const payload = await fetchJSON(`/admin/api/alert-analysis/tasks/${encodeURIComponent(selectedId)}`, {}, 8000)
    setDetail(payload)
  }, [selectedId])

  useEffect(() => {
    load().catch((error) => setMessage({ ok: false, text: `加载告警任务失败：${error.message}` }))
  }, [load])

  useEffect(() => {
    loadDetail().catch((error) => setMessage({ ok: false, text: `加载任务详情失败：${error.message}` }))
    const timer = setInterval(() => {
      load().catch(() => {})
      loadDetail().catch(() => {})
    }, 2000)
    return () => clearInterval(timer)
  }, [load, loadDetail])

  const cancelTask = async (id) => {
    try {
      await fetchJSON(`/admin/api/alert-analysis/tasks/${encodeURIComponent(id)}/cancel`, { method: 'POST' }, 8000)
      setMessage({ ok: true, text: `已请求取消告警分析 #${id}` })
      await Promise.all([load(), loadDetail()])
    } catch (error) {
      setMessage({ ok: false, text: `取消失败：${error.message}` })
    }
  }

  const retryTask = async (id) => {
    try {
      const payload = await fetchJSON(`/admin/api/alert-analysis/tasks/${encodeURIComponent(id)}/retry`, { method: 'POST' }, 8000)
      const nextId = payload.task?.id
      setMessage({ ok: true, text: `已创建重试任务 #${nextId}` })
      setSelectedId(nextId)
      await load()
    } catch (error) {
      setMessage({ ok: false, text: `重试失败：${error.message}` })
    }
  }

  return (
    <section className="panel">
      <div className="section-head">
        <div><h2>告警分析</h2><p>阶段进度每 2 秒刷新；被节流的告警也会保留记录</p></div>
        <div className="toolbar">
          <select value={configId} onChange={(event) => { setPage(1); setConfigId(event.target.value) }}>
            <option value="">全部配置</option>
            {configs.map((cfg) => <option key={cfg.id} value={cfg.id}>{cfg.name}</option>)}
          </select>
          <select value={status} onChange={(event) => { setPage(1); setStatus(event.target.value) }}>
            <option value="">全部状态</option>
            {['queued', 'running', 'cancel_requested', 'succeeded', 'failed', 'canceled', 'suppressed'].map((item) => <option key={item} value={item}>{STATUS_LABELS[item]}</option>)}
          </select>
          <IconButton icon={RefreshCw} onClick={load}>刷新</IconButton>
          <IconButton icon={ChevronLeft} onClick={() => setPage((value) => Math.max(1, value - 1))} disabled={page <= 1}>上一页</IconButton>
          <span className="pager">{page}</span>
          <IconButton icon={ChevronRight} onClick={() => setPage((value) => value + 1)} disabled={!hasMore}>下一页</IconButton>
        </div>
      </div>
      <Message message={message} />
      <div className="table-shell">
        <table className="data-table job-table analysis-task-table">
          <thead><tr><th>ID</th><th>配置</th><th>环境</th><th>告警</th><th>接口/服务</th><th>状态</th><th>阶段</th><th>时间</th></tr></thead>
          <tbody>
            {tasks.length ? tasks.map((task) => (
              <tr key={task.id} onClick={() => setSelectedId(task.id)}>
                <td>#{task.id}</td><td>{task.config_name}</td><td>{task.alert?.environment || '-'}</td>
                <td>{task.alert?.title || task.alert?.error_code || '-'}</td>
                <td><code>{[task.alert?.method, task.alert?.endpoint || task.alert?.service].filter(Boolean).join(' ') || '-'}</code></td>
                <td><StatusBadge status={task.status} /></td><td>{PHASE_LABELS[task.phase] || task.phase}</td><td>{prettyTime(task.created_at)}</td>
              </tr>
            )) : <tr><td colSpan="8" className="empty-cell">暂无告警分析任务</td></tr>}
          </tbody>
        </table>
      </div>
      <AnalysisTaskDrawer task={detail} open={Boolean(selectedId)} onClose={() => setSelectedId(null)} onCancel={cancelTask} onRetry={retryTask} />
    </section>
  )
}

function AnalysisTaskDrawer({ task, open, onClose, onCancel, onRetry }) {
  if (!open) return null
  const running = task && ['queued', 'running', 'cancel_requested'].includes(task.status)
  const result = task?.result || null
  return (
    <div className="drawer-layer" role="dialog" aria-modal="true" aria-label="告警分析详情">
      <button className="drawer-backdrop" type="button" aria-label="关闭" onClick={onClose} />
      <aside className="log-drawer analysis-drawer">
        <div className="log-drawer-head">
          <div><span className="drawer-eyebrow">告警分析</span><h2>{task ? `告警分析 #${task.id}` : '加载中...'}</h2><p>{task?.config_name || ''}</p></div>
          <button className="drawer-close" type="button" onClick={onClose}><XCircle size={20} /></button>
        </div>
        {!task ? <div className="drawer-loading">正在读取任务...</div> : (
          <div className="analysis-detail-scroll">
            <div className="log-drawer-summary"><StatusBadge status={task.status} /><span>{PHASE_LABELS[task.phase] || task.phase}</span><span>已尝试 {task.attempts} 次</span></div>
            <div className="log-drawer-actions">
              {running && task.status !== 'cancel_requested' ? <IconButton icon={XCircle} className="danger" onClick={() => onCancel(task.id)}>取消分析</IconButton> : null}
              {!running ? <IconButton icon={RotateCcw} onClick={() => onRetry(task.id)}>重新分析</IconButton> : null}
            </div>
            {task.duplicate_of_task_id ? (
              <section className="analysis-card-block">
                <h3>重复报错，已分析</h3>
                <p>相同接口与错误信息已由任务 <a href={`/admin/?analysis_task=${task.duplicate_of_task_id}`}>#{task.duplicate_of_task_id}</a> 分析，本次未重复运行模型。</p>
              </section>
            ) : null}
            <section className="analysis-card-block">
              <h3>告警信息</h3>
              <dl className="log-meta analysis-meta">
                <div><dt>环境</dt><dd>{task.alert?.environment || '-'}</dd></div>
                <div><dt>服务</dt><dd>{task.alert?.service || '-'}</dd></div>
                <div className="wide"><dt>接口</dt><dd><code>{[task.alert?.method, task.alert?.endpoint].filter(Boolean).join(' ') || '-'}</code></dd></div>
                <div className="wide"><dt>Trace ID</dt><dd><code>{task.alert?.trace_id || '-'}</code></dd></div>
              </dl>
            </section>
            {result ? <AnalysisResultView result={result} /> : null}
            {task.error ? <section className="analysis-card-block error-block"><h3>失败信息</h3><pre>{task.error}</pre></section> : null}
            <section className="analysis-card-block">
              <h3>进度事件</h3>
              <div className="analysis-timeline">
                {(task.events || []).map((event) => <div key={event.id} className={`timeline-item ${event.level}`}><time>{prettyTime(event.created_at)}</time><strong>{event.phase}</strong><p>{event.message}</p></div>)}
              </div>
            </section>
          </div>
        )}
      </aside>
    </div>
  )
}

function AnalysisResultView({ result }) {
  return (
    <section className="analysis-card-block result-block">
      <div className="subsection-title"><h3>分析结论</h3><StatusBadge status={result.confidence} /></div>
      <p className="analysis-summary">{result.summary}</p>
      <div className="analysis-assessment">
        <div><span>AI 评估严重程度</span><StatusBadge status={result.assessed_severity || 'low'} /></div>
        <p>{result.severity_reason || '暂无评估依据'}</p>
      </div>
      <AnalysisStringList title="影响面" items={result.impact_scope} />
      <AnalysisStringList title="事实" items={result.facts} />
      <AnalysisStringList title="假设" items={result.hypotheses} />
      {(result.code_evidence || []).length ? <div className="analysis-evidence"><h4>代码证据</h4>{result.code_evidence.map((item, index) => <p key={`${item.path}-${index}`}><code>{item.path}{item.line ? `:${item.line}` : ''}</code> — {item.reason}</p>)}</div> : null}
      {(result.suspect_commits || []).length ? <div className="analysis-evidence"><h4>相关提交</h4>{result.suspect_commits.map((item, index) => <p key={`${item.sha}-${index}`}><code>{item.sha?.slice(0, 10)}</code> {item.title} · {item.author} · <StatusBadge status={item.confidence} /><br /><span>{item.reason}</span></p>)}</div> : null}
      <AnalysisStringList title="建议联系" items={result.suggested_contacts} />
      <AnalysisStringList title="证据缺口" items={result.evidence_gaps} />
      <AnalysisStringList title="建议操作" items={result.recommended_actions} />
    </section>
  )
}

function AnalysisStringList({ title, items }) {
  if (!items?.length) return null
  return <div className="analysis-evidence"><h4>{title}</h4><ul>{items.map((item, index) => <li key={`${title}-${index}`}>{item}</li>)}</ul></div>
}

function AnalysisConfigsPanel() {
  const [configs, setConfigs] = useState([])
  const [selectedId, setSelectedId] = useState(null)
  const [form, setForm] = useState(EMPTY_ANALYSIS_CONFIG)
  const [message, setMessage] = useState(null)
  const [ingestURL, setIngestURL] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    const payload = await fetchJSON('/admin/api/alert-analysis/configs', {}, 8000)
    setConfigs(payload.configs || [])
    return payload.configs || []
  }, [])

  useEffect(() => { load().catch((error) => setMessage({ ok: false, text: `加载配置失败：${error.message}` })) }, [load])

  const selectConfig = (config) => {
    setSelectedId(config.id)
    setForm({ ...EMPTY_ANALYSIS_CONFIG, ...config })
    setIngestURL('')
    setMessage(null)
  }
  const newConfig = () => { setSelectedId(null); setForm({ ...EMPTY_ANALYSIS_CONFIG }); setIngestURL(''); setMessage(null) }
  const setField = (key, value) => setForm((current) => ({ ...current, [key]: value }))

  const save = async () => {
    setBusy(true)
    try {
      const url = selectedId ? `/admin/api/alert-analysis/configs/${selectedId}` : '/admin/api/alert-analysis/configs'
      const payload = await fetchJSON(url, { method: selectedId ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(form) }, 12000)
      setMessage({ ok: true, text: selectedId ? '告警配置已更新' : '告警配置已创建，请立即复制专属接收地址' })
      if (payload.config?.ingest_url) setIngestURL(payload.config.ingest_url)
      const next = await load()
      const id = payload.config?.id || selectedId
      const config = next.find((item) => item.id === id)
      if (config) { setSelectedId(id); setForm({ ...EMPTY_ANALYSIS_CONFIG, ...config }) }
    } catch (error) {
      setMessage({ ok: false, text: `保存失败：${error.message}` })
    } finally { setBusy(false) }
  }

  const toggle = async () => {
    if (!selectedId) return
    try {
      const payload = await fetchJSON(`/admin/api/alert-analysis/configs/${selectedId}/enabled`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: !form.enabled }) }, 8000)
      setForm({ ...EMPTY_ANALYSIS_CONFIG, ...payload.config })
      await load()
    } catch (error) { setMessage({ ok: false, text: `切换失败：${error.message}` }) }
  }

  const remove = async () => {
    if (!selectedId || !window.confirm(`删除配置“${form.name}”？历史任务会保留，但密钥将被删除。`)) return
    try {
      await fetchJSON(`/admin/api/alert-analysis/configs/${selectedId}`, { method: 'DELETE' }, 8000)
      newConfig()
      setMessage({ ok: true, text: '配置已删除，历史任务仍保留' })
      await load()
    } catch (error) { setMessage({ ok: false, text: `删除失败：${error.message}` }) }
  }

  const rotate = async () => {
    if (!selectedId) return
    try {
      const payload = await fetchJSON(`/admin/api/alert-analysis/configs/${selectedId}/rotate-token`, { method: 'POST' }, 8000)
      setIngestURL(payload.ingest_url || '')
      setMessage({ ok: true, text: '接收 Token 已轮换，旧地址立即失效，请更新 SLS 行动策略' })
    } catch (error) { setMessage({ ok: false, text: `轮换失败：${error.message}` }) }
  }

  const test = async (kind) => {
    if (!selectedId) { setMessage({ ok: false, text: '请先保存配置再测试连接' }); return }
    setBusy(true)
    try {
      await fetchJSON(`/admin/api/alert-analysis/configs/${selectedId}/test/${kind}`, { method: 'POST' }, 35000)
      setMessage({ ok: true, text: `${kind.toUpperCase()} 连接测试成功` })
    } catch (error) { setMessage({ ok: false, text: `连接测试失败：${error.message}` }) }
    finally { setBusy(false) }
  }

  return (
    <section className="panel">
      <div className="section-head"><div><h2>告警配置</h2><p>每条配置生成独立 SLS 接收地址；仓库访问复用全局 Gitea Token</p></div><IconButton icon={BellRing} onClick={newConfig}>新建配置</IconButton></div>
      <Message message={message} />
      {ingestURL ? <div className="ingest-banner"><code>{ingestURL}</code><IconButton icon={Copy} onClick={() => navigator.clipboard.writeText(ingestURL)}>复制地址</IconButton></div> : null}
      <div className="analysis-config-layout">
        <aside className="analysis-config-list">
          {configs.map((config) => <button key={config.id} className={selectedId === config.id ? 'active' : ''} type="button" onClick={() => selectConfig(config)}><strong>{config.name}</strong><span>{config.repository_ref} · {config.sls_logstore}</span><StatusBadge status={config.enabled ? 'enabled' : 'disabled'} /></button>)}
          {!configs.length ? <div className="empty-state compact">还没有告警配置</div> : null}
        </aside>
        <div className="analysis-config-form">
          <div className="config-form-head"><div><h3>{selectedId ? `编辑配置 #${selectedId}` : '新建告警配置'}</h3><p>密钥字段留空或显示 ***set*** 时保持原值</p></div><div className="toolbar"><IconButton icon={Save} onClick={save} disabled={busy}>保存</IconButton>{selectedId ? <><IconButton icon={RefreshCw} onClick={toggle}>{form.enabled ? '停用' : '启用'}</IconButton><IconButton icon={Trash2} className="danger" onClick={remove}>删除</IconButton></> : null}</div></div>
          <ConfigSection title="基本与仓库">
            <AnalysisField label="配置名称" value={form.name} onChange={(v) => setField('name', v)} />
            <AnalysisField label="仓库 Clone URL" value={form.repository_url} onChange={(v) => setField('repository_url', v)} />
            <AnalysisField label="目标分支 / SHA" value={form.repository_ref} onChange={(v) => setField('repository_ref', v)} />
          </ConfigSection>
          <ConfigSection title="阿里云 SLS">
            <AnalysisField label="Endpoint" value={form.sls_endpoint} onChange={(v) => setField('sls_endpoint', v)} />
            <AnalysisField label="Project" value={form.sls_project} onChange={(v) => setField('sls_project', v)} />
            <AnalysisField label="查询 Logstore（多个用逗号分隔）" value={form.sls_logstore} placeholder="function-log-prod-flat,taskiq-log-prod-flat" onChange={(v) => setField('sls_logstore', v)} />
            <AnalysisField label="AccessKey ID" value={form.sls_access_key_id} secret onChange={(v) => setField('sls_access_key_id', v)} />
            <AnalysisField label="AccessKey Secret" value={form.sls_access_key_secret} secret onChange={(v) => setField('sls_access_key_secret', v)} />
            <AnalysisField label="查询窗口（秒）" value={form.log_window_seconds} type="number" onChange={(v) => setField('log_window_seconds', Number(v))} />
          </ConfigSection>
          <ConfigSection title="飞书通知">
            <label className="field"><span>机器人类型</span><select value={form.feishu_mode || 'webhook'} onChange={(event) => setField('feishu_mode', event.target.value)}><option value="webhook">群 Webhook（完成后只发一张）</option><option value="app">应用机器人（实时更新同一张）</option></select></label>
            {form.feishu_mode === 'app' ? <>
              <AnalysisField label="应用机器人 App ID" value={form.feishu_app_id} placeholder="cli_xxx" onChange={(v) => setField('feishu_app_id', v)} />
              <AnalysisField label="应用机器人 App Secret" value={form.feishu_app_secret} secret onChange={(v) => setField('feishu_app_secret', v)} />
              <AnalysisField label="目标群 Chat ID" value={form.feishu_chat_id} placeholder="oc_xxx" onChange={(v) => setField('feishu_chat_id', v)} />
            </> : <AnalysisField label="群机器人 Webhook" value={form.feishu_webhook} secret onChange={(v) => setField('feishu_webhook', v)} />}
          </ConfigSection>
          <ConfigSection title="分析模型">
            <AnalysisField label="模型（留空复用全局）" value={form.model} onChange={(v) => setField('model', v)} />
            <AnalysisField label="思考强度" value={form.reasoning_effort} onChange={(v) => setField('reasoning_effort', v)} />
            <AnalysisField label="此配置并发数（1-16）" value={form.concurrency} type="number" onChange={(v) => setField('concurrency', Number(v))} />
            <AnalysisField label="超时（秒）" value={form.timeout_seconds} type="number" onChange={(v) => setField('timeout_seconds', Number(v))} />
          </ConfigSection>
          <ConfigSection title="连续相同告警节流">
            <label className="field"><span>是否启用</span><select value={String(form.throttle_enabled)} onChange={(event) => setField('throttle_enabled', event.target.value === 'true')}><option value="true">启用</option><option value="false">关闭</option></select></label>
            <AnalysisField label="相同报错分析次数" value={form.throttle_threshold} type="number" onChange={(v) => setField('throttle_threshold', Number(v))} />
            <AnalysisField label="重新分析间隔（秒，0=直到报错变化）" value={form.throttle_cooldown_seconds} type="number" onChange={(v) => setField('throttle_cooldown_seconds', Number(v))} />
            <AnalysisField label="指纹字段" value={form.throttle_fields} onChange={(v) => setField('throttle_fields', v)} />
          </ConfigSection>
          <label className="field full-field"><span>项目补充 Prompt</span><textarea rows="5" value={form.prompt || ''} onChange={(event) => setField('prompt', event.target.value)} /></label>
          <div className="toolbar config-test-toolbar"><IconButton icon={Send} onClick={() => test('repo')} disabled={busy}>测试仓库</IconButton><IconButton icon={Send} onClick={() => test('sls')} disabled={busy}>测试 SLS</IconButton><IconButton icon={Send} onClick={() => test('feishu')} disabled={busy}>测试飞书</IconButton>{selectedId ? <IconButton icon={RefreshCw} onClick={rotate}>轮换接收地址</IconButton> : null}</div>
        </div>
      </div>
    </section>
  )
}

function ConfigSection({ title, children }) {
  return <section className="analysis-config-section"><h4>{title}</h4><div className="form-grid">{children}</div></section>
}

function AnalysisField({ label, value, onChange, secret = false, type = 'text', placeholder = '' }) {
  return <label className="field"><span>{label}</span><input type={secret && value !== REDACTED ? 'password' : type} value={value ?? ''} placeholder={placeholder} onChange={(event) => onChange(event.target.value)} /></label>
}

function SkillsPanel() {
  const [projects, setProjects] = useState([])
  const [selected, setSelected] = useState(null)
  const [detail, setDetail] = useState(null)
  const [loading, setLoading] = useState(false)
  const [generating, setGenerating] = useState(false)
  const [generationTask, setGenerationTask] = useState(null)
  const [message, setMessage] = useState(null)
  const [copied, setCopied] = useState(false)

  const loadProjects = useCallback(async () => {
    try {
      const payload = await fetchJSON('/admin/api/skills/projects', {}, 10000)
      const list = payload.projects || []
      setProjects(list)
      setSelected((current) => current || list[0] || null)
      setMessage(null)
    } catch (error) {
      setMessage({ ok: false, text: `加载失败：${error.message}` })
    }
  }, [])

  useEffect(() => {
    loadProjects()
  }, [loadProjects])

  useEffect(() => {
    if (!selected) {
      setDetail(null)
      return
    }
    let cancelled = false
    async function loadDetail() {
      setLoading(true)
      try {
        const payload = await fetchJSON(`/admin/api/skills/${encodeURIComponent(selected.owner)}/${encodeURIComponent(selected.repo)}`, {}, 10000)
        if (!cancelled) setDetail(payload.skill || null)
      } catch (error) {
        if (!cancelled) setMessage({ ok: false, text: `读取 Skill 失败：${error.message}` })
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    loadDetail()
    return () => {
      cancelled = true
    }
  }, [selected])

  const generate = async () => {
    if (!selected) return
    setGenerating(true)
    setGenerationTask(null)
    setMessage({ ok: true, text: '已提交项目 Skill 生成任务，正在等待结果...' })
    try {
      const payload = await fetchJSON(`/admin/api/skills/${encodeURIComponent(selected.owner)}/${encodeURIComponent(selected.repo)}/generate`, { method: 'POST' }, 15000)
      const task = payload.task || null
      if (!task?.id) throw new Error('生成任务没有返回 task id')
      setGenerationTask(task)
    } catch (error) {
      setMessage({ ok: false, text: `生成失败：${error.message}` })
      setGenerationTask(null)
      setGenerating(false)
    }
  }

  useEffect(() => {
    if (!generationTask || generationTask.status !== 'running') return undefined
    let cancelled = false
    const poll = async () => {
      try {
        const payload = await fetchJSON(`/admin/api/skills/${encodeURIComponent(generationTask.owner)}/${encodeURIComponent(generationTask.repo)}/generate/${encodeURIComponent(generationTask.id)}`, {}, 10000)
        if (cancelled) return
        const task = payload.task || null
        if (task?.status === 'done') {
          if (task.skill) setDetail(task.skill)
          setMessage({ ok: true, text: 'Skill 已更新' })
          setGenerating(false)
          setGenerationTask(null)
          loadProjects()
        } else if (task?.status === 'failed') {
          setMessage({ ok: false, text: `生成失败：${task.error || '未知错误'}` })
          setGenerating(false)
          setGenerationTask(null)
        } else if (task?.status === 'running') {
          setGenerationTask(task)
        } else {
          setMessage({ ok: false, text: '生成任务状态异常，请刷新后重试' })
          setGenerating(false)
          setGenerationTask(null)
        }
      } catch (error) {
        if (!cancelled) {
          setMessage({ ok: false, text: `轮询生成状态失败：${error.message}` })
          setGenerating(false)
          setGenerationTask(null)
        }
      }
    }
    const timer = setInterval(poll, 2000)
    poll()
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [generationTask?.id, generationTask?.owner, generationTask?.repo, generationTask?.status, loadProjects])

  const skill = detail || {}
  const ctx = skill.context || {}
  const evidenceSignals = buildSkillSignals(ctx.patterns || [])
  const busy = loading || generating
  const downloadPath = selected ? `/skills/${encodeURIComponent(selected.owner)}/${encodeURIComponent(selected.repo)}/SKILL.md` : ''
  const origin = typeof window === 'undefined' ? '' : window.location.origin
  const installCommand = selected
    ? `请为 ${selected.owner}/${selected.repo} 安装并使用这个项目缺陷预防 Skill：${origin}${downloadPath}`
    : ''

  const copyCommand = async () => {
    if (!installCommand) return
    try {
      await navigator.clipboard.writeText(installCommand)
    } catch {
      const input = document.createElement('textarea')
      input.value = installCommand
      input.setAttribute('readonly', 'true')
      input.style.position = 'fixed'
      input.style.opacity = '0'
      document.body.appendChild(input)
      input.select()
      document.execCommand('copy')
      document.body.removeChild(input)
    }
    setCopied(true)
    setTimeout(() => setCopied(false), 1200)
  }

  return (
    <section className="panel">
      <div className="section-head">
        <div>
          <h2>Skill</h2>
          <p>按项目把常见缺陷沉淀成可下载、可演进的经验 Skill。</p>
        </div>
        <div className="toolbar">
          <IconButton icon={RefreshCw} onClick={loadProjects} disabled={generating}>刷新</IconButton>
          <IconButton icon={generating ? LoaderCircle : Sparkles} className={generating ? 'busy' : ''} onClick={generate} disabled={!selected || generating}>
            {generating ? '生成中' : '生成/进化'}
          </IconButton>
        </div>
      </div>
      <Message message={message} />

      <div className="skills-layout">
        <aside className="skill-projects">
          <h3>项目</h3>
          <div className="skill-project-list">
            {projects.length ? projects.map((project) => {
              const active = selected?.owner === project.owner && selected?.repo === project.repo
              return (
                <button className={active ? 'skill-project active' : 'skill-project'} type="button" key={`${project.owner}/${project.repo}`} onClick={() => setSelected(project)} disabled={generating}>
                  <strong>{project.owner}/{project.repo}</strong>
                  <span>{project.findings} findings · {project.open_findings} open</span>
                  {project.skill_version ? <small>v{project.skill_version}</small> : <small>未生成</small>}
                </button>
              )
            }) : <div className="empty-state compact">暂无可沉淀的项目缺陷</div>}
          </div>
        </aside>

        <div className="skill-workspace" aria-busy={busy ? 'true' : 'false'}>
          {selected ? (
            <>
              <div className="skill-hero">
                <div>
                  <span>{selected.owner}/{selected.repo}</span>
                  <h3>{skill.title || '尚未生成 Skill'}</h3>
                  <p>{ctx.findings || selected.findings || 0} 个历史缺陷，{ctx.high_critical_open || selected.high_critical_open || 0} 个严重 Open。</p>
                </div>
                <div className="skill-actions">
                  <a className={generating || !skill.content ? 'button disabled-link' : 'button'} href={generating || !skill.content ? undefined : downloadPath} target="_blank" rel="noreferrer" aria-disabled={generating || !skill.content ? 'true' : 'false'}><Download size={17} />下载</a>
                  <button className="button" type="button" onClick={copyCommand} disabled={!skill.content || generating}><Copy size={17} />{copied ? '已复制' : '复制 Skill 指令'}</button>
                </div>
              </div>

              {generating ? (
                <div className="skill-generating-banner" role="status">
                  <LoaderCircle size={18} />
                  <div>
                    <strong>正在生成/进化项目 Skill</strong>
                    <span>任务 {generationTask?.id || '-'} 正在后台运行，完成后自动刷新版本与下载链接。</span>
                  </div>
                </div>
              ) : null}

              <div className="skill-metrics">
                <StatCard label="PR" value={ctx.pull_requests ?? selected.pull_requests ?? '-'} hint="project PRs" />
                <StatCard label="Review Runs" value={ctx.review_runs ?? selected.review_runs ?? '-'} hint="agent runs" />
                <StatCard label="Findings" value={ctx.findings ?? selected.findings ?? '-'} hint="source defects" />
                <StatCard label="版本" value={skill.version ? `v${skill.version}` : '-'} hint={skill.updated_at ? prettyTime(skill.updated_at) : 'not generated'} />
              </div>

              <section className="subsection">
                <div className="subsection-title">
                  <div>
                    <h3>Skill 指令</h3>
                    <span>按项目区分，链接不需要控制台鉴权</span>
                  </div>
                  <button className="button compact-button" type="button" onClick={copyCommand} disabled={!skill.content || generating}><Copy size={15} />{copied ? '已复制' : '复制指令'}</button>
                </div>
                <pre className="command-box">{installCommand}</pre>
              </section>

              {evidenceSignals.length ? (
                <section className="subsection">
                  <div className="subsection-title">
                    <div>
                      <h3>经验信号</h3>
                      <span>按标签、严重度和状态抽象，生成时用于沉淀经验</span>
                    </div>
                  </div>
                  <div className="pattern-list">
                    {evidenceSignals.slice(0, 6).map((signal) => (
                      <article className="pattern-card" key={`${signal.label}-${signal.severity}-${signal.status}`}>
                        <strong>{signal.label}</strong>
                        <span>{signal.severity} · {signal.status} · {signal.count} 次</span>
                        <small>{signal.openCount} open · general signal</small>
                      </article>
                    ))}
                  </div>
                </section>
              ) : null}

              <section className="subsection">
                <div className="subsection-title">
                  <div>
                    <h3>SKILL.md</h3>
                    <span>{generating ? '正在生成...' : loading ? '加载中...' : skill.content ? '可直接下载使用' : '点击生成后出现内容'}</span>
                  </div>
                </div>
                <pre className={generating ? 'skill-preview generating' : 'skill-preview'}>{generating ? '正在基于项目历史缺陷和已有 Skill 生成，请稍候...' : skill.content || '还没有生成 Skill。'}</pre>
              </section>
            </>
          ) : (
            <div className="empty-state">暂无项目数据。</div>
          )}
        </div>
      </div>
    </section>
  )
}

function buildSkillSignals(patterns) {
  const byKey = new Map()
  for (const pattern of patterns || []) {
    const tags = [...new Set((pattern.tags || []).map((tag) => String(tag || '').trim().toLowerCase()).filter(Boolean))].sort().slice(0, 3)
    const label = tags.length ? tags.join(' / ') : `${pattern.severity || 'general'} experience`
    const key = `${label}\u0000${pattern.severity || ''}\u0000${pattern.status || ''}`
    const current = byKey.get(key) || {
      label,
      severity: pattern.severity || 'info',
      status: pattern.status || 'open',
      count: 0,
      openCount: 0
    }
    current.count += pattern.count || 0
    current.openCount += pattern.open_count || 0
    byKey.set(key, current)
  }
  return [...byKey.values()].sort((a, b) => b.count - a.count || b.openCount - a.openCount || a.label.localeCompare(b.label))
}

function AnalyticsPanel() {
  const [report, setReport] = useState(null)
  const [trend, setTrend] = useState([])
  const [trendInterval, setTrendInterval] = useState('day')
  const [loading, setLoading] = useState(false)
  const [message, setMessage] = useState(null)

  const trendLimit = trendInterval === 'day' ? 14 : 12

  const loadLatest = useCallback(async () => {
    try {
      const [latestPayload, trendPayload] = await Promise.all([
        fetchJSON('/admin/api/analytics/reports/latest', {}, 10000),
        fetchJSON(`/admin/api/analytics/trend?interval=${encodeURIComponent(trendInterval)}&limit=${trendLimit}`, {}, 10000)
      ])
      setReport(latestPayload.report || null)
      setTrend(trendPayload.points || [])
      setMessage(null)
    } catch (error) {
      setMessage({ ok: false, text: `加载失败：${error.message}` })
    }
  }, [trendInterval, trendLimit])

  useEffect(() => {
    loadLatest()
  }, [loadLatest])

  const createReport = async () => {
    setLoading(true)
    setMessage({ ok: true, text: '正在生成全量历史分析...' })
    try {
      const payload = await fetchJSON('/admin/api/analytics/reports', { method: 'POST' }, 30000)
      setReport(payload.report || null)
      const trendPayload = await fetchJSON(`/admin/api/analytics/trend?interval=${encodeURIComponent(trendInterval)}&limit=${trendLimit}`, {}, 10000)
      setTrend(trendPayload.points || [])
      setMessage({ ok: true, text: '分析报告已生成' })
    } catch (error) {
      setMessage({ ok: false, text: `生成失败：${error.message}` })
    } finally {
      setLoading(false)
    }
  }

  return (
    <section className="panel">
      <div className="section-head">
        <div>
          <h2>分析</h2>
          <p>{report ? `报告 #${report.id} · ${prettyTime(report.created_at)}` : '暂无报告'}</p>
        </div>
        <div className="toolbar">
          <IconButton icon={RefreshCw} onClick={loadLatest}>刷新</IconButton>
          <IconButton icon={BarChart3} onClick={createReport} disabled={loading}>生成分析报告</IconButton>
        </div>
      </div>
      <Message message={message} />
      <AnalysisReport report={report} trend={trend} trendInterval={trendInterval} setTrendInterval={setTrendInterval} />
    </section>
  )
}

function AnalysisReport({ report, trend = [], trendInterval = 'day', setTrendInterval }) {
  if (!report) return <div className="empty-state">点击“生成分析报告”后查看全量历史聚合。</div>
  const summary = report.summary || {}
  const completed = (summary.successful_review_runs || 0) + (summary.failed_review_runs || 0)
  const agents = Object.entries(summary.by_agent || {}).sort(([a], [b]) => a.localeCompare(b))
  const severities = Object.entries(summary.by_severity || {}).sort(byPreferredOrder(['critical', 'high', 'medium', 'low', 'info']))
  const statuses = Object.entries(summary.by_status || {}).sort(byPreferredOrder(['open', 'fixed', 'dismissed']))
  const developers = summary.by_developer || []
  const severityChart = severities.map(([label, value]) => ({ label, value }))
  const statusChart = statuses.map(([label, value]) => ({ label, value }))
  const agentChart = agents
    .map(([label, agent]) => ({ label, value: agent.findings || 0, meta: `${agent.open || 0} open` }))
    .sort((a, b) => b.value - a.value || a.label.localeCompare(b.label))
  const developerChart = developers
    .map((developer) => ({ label: developer.developer, value: developer.findings || 0, meta: `${developer.open_findings || 0} open · ${developer.pull_requests || 0} PR` }))
    .sort((a, b) => b.value - a.value || a.label.localeCompare(b.label))

  return (
    <>
      <div className="stats-grid">
        <StatCard label="Review 成功率" value={completed ? percent(summary.success_rate) : '--'} hint={completed ? `${summary.successful_review_runs || 0} / ${completed}` : '暂无运行'} />
        <StatCard label="问题总数" value={summary.total_findings || 0} hint="findings" />
        <StatCard label="Open 问题" value={summary.open_findings || 0} hint="仍需关注" />
        <StatCard label="严重 Open" value={summary.high_critical_open || 0} hint="high + critical" />
        <StatCard label="已修复" value={summary.fixed_findings || 0} hint="fixed" />
      </div>

      <TrendOverview points={trend} interval={trendInterval} onIntervalChange={setTrendInterval} />

      <div className="chart-grid">
        <BarChartBlock title="按严重度" label="Risk" items={severityChart} empty="暂无严重度数据" tone="risk" />
        <BarChartBlock title="按状态" label="Lifecycle" items={statusChart} empty="暂无状态数据" tone="status" />
        <BarChartBlock title="按 Reviewer 发现量" label="Agent" items={agentChart} empty="暂无 reviewer 数据" tone="agent" />
        <BarChartBlock title="按研发发现量" label="Owner" items={developerChart} empty="暂无研发数据" tone="developer" />
      </div>

      <div className="two-column">
        <TableBlock title="Top Tags" headers={['Tag', '数量']} empty="暂无 tags">
          {(summary.top_tags || []).map((tag) => <tr key={tag.tag}><td>{tag.tag}</td><td>{tag.count}</td></tr>)}
        </TableBlock>
        <TableBlock title="严重度分布" headers={['严重度', '数量']} empty="暂无 findings">
          {severities.map(([severity, count]) => <tr key={severity}><td>{severity}</td><td>{count}</td></tr>)}
        </TableBlock>
      </div>

      <TableBlock title="近期严重问题" headers={['Reviewer', '严重度', '位置', '标题', '状态']} empty="暂无 high/critical 问题">
        {(summary.recent_severe || []).map((finding, index) => (
          <tr key={`${finding.path}-${finding.line}-${index}`}>
            <td>{finding.agent}</td>
            <td><StatusBadge status={finding.severity} /></td>
            <td><SourceLink baseURL={report.gitea_url} finding={finding} /></td>
            <td>{finding.title}</td>
            <td>{finding.status}</td>
          </tr>
        ))}
      </TableBlock>

      <TableBlock title="Reviewer 对比" headers={['Reviewer', '运行', '成功', '失败', '发现', 'Open']} empty="暂无 reviewer run">
        {agents.map(([name, agent]) => (
          <tr key={name}>
            <td>{name}</td>
            <td>{agent.review_runs || 0}</td>
            <td>{agent.succeeded || 0}</td>
            <td>{agent.failed || 0}</td>
            <td>{agent.findings || 0}</td>
            <td>{agent.open || 0}</td>
          </tr>
        ))}
      </TableBlock>

      <TableBlock title="研发统计" headers={['研发', 'PR', '运行', '成功', '失败', '发现', 'Open', '严重 Open']} empty="暂无研发数据">
        {developers.map((developer) => (
          <tr key={developer.developer}>
            <td>{developer.developer}</td>
            <td>{developer.pull_requests || 0}</td>
            <td>{developer.review_runs || 0}</td>
            <td>{developer.successful_review_runs || 0}</td>
            <td>{developer.failed_review_runs || 0}</td>
            <td>{developer.findings || 0}</td>
            <td>{developer.open_findings || 0}</td>
            <td>{developer.high_critical_open || 0}</td>
          </tr>
        ))}
      </TableBlock>

      <div className="two-column">
        <ListBlock title="重复问题标题" items={(summary.repeated_titles || []).map((item) => `${item.title} (${item.count})`)} empty="暂无重复标题" />
        <MultiAgentOverlap report={report} items={summary.agent_overlap || []} />
      </div>
    </>
  )
}

function TrendOverview({ points, interval = 'day', onIntervalChange }) {
  const history = [...(points || [])].filter((item) => item?.day || item?.finished_at)
  const intervalLabel = { day: '天', week: '周', month: '月' }[interval] || '天'
  const intervalText = { day: '按天聚合', week: '按周聚合', month: '按月聚合' }[interval] || '按天聚合'
  if (history.length < 1) {
    return (
      <section className="subsection trend-section">
        <div className="subsection-title">
          <h3>趋势</h3>
          <span>{intervalText}</span>
        </div>
        <TrendIntervalControl value={interval} onChange={onIntervalChange} />
        <div className="empty-state compact">产生 review 数据后，这里会展示{intervalText}趋势。</div>
      </section>
    )
  }

  const chartPoints = history.map((item, index) => {
    const bucket = item.bucket || item.day || prettyTime(item.finished_at).slice(0, 10)
    const label = formatTrendBucket(bucket, item.interval || interval)
    return {
      id: `${bucket}-${index}`,
      label,
      total: item.total_findings || 0,
      open: item.open_findings || 0,
      severe: item.high_critical_open || 0,
      success: Math.round((item.success_rate || 0) * 1000) / 10
    }
  })

  return (
    <div className="trend-wrap">
      <TrendIntervalControl value={interval} onChange={onIntervalChange} />
      <div className="trend-grid">
        <LineChart
          title="问题趋势"
          subtitle={`${chartPoints[0].label} -> ${chartPoints[chartPoints.length - 1].label}`}
          points={chartPoints}
          series={[
            { key: 'total', label: '问题总数', color: '#315f7d' },
            { key: 'open', label: 'Open', color: '#9b5b2a' },
            { key: 'severe', label: '严重 Open', color: '#9a3333' }
          ]}
        />
        <LineChart
          title="Review 成功率趋势"
          subtitle={`最近 ${chartPoints.length} ${intervalLabel}`}
          points={chartPoints}
          valueSuffix="%"
          series={[
            { key: 'success', label: '成功率', color: '#2f6f55' }
          ]}
        />
      </div>
    </div>
  )
}

function TrendIntervalControl({ value, onChange }) {
  const options = [
    { value: 'day', label: '日' },
    { value: 'week', label: '周' },
    { value: 'month', label: '月' }
  ]
  return (
    <div className="segmented-control" aria-label="趋势维度">
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          className={value === option.value ? 'active' : ''}
          onClick={() => onChange?.(option.value)}
        >
          {option.label}
        </button>
      ))}
    </div>
  )
}

function formatTrendBucket(bucket, interval) {
  if (!bucket) return ''
  if (interval === 'month') {
    const [year, month] = bucket.split('-')
    return year && month ? `${year}-${month}` : bucket
  }
  if (bucket.length >= 10) {
    return bucket.slice(5, 10)
  }
  return bucket
}

function LineChart({ title, subtitle, points, series, valueSuffix = '' }) {
  const values = series.flatMap((line) => points.map((point) => point[line.key] || 0))
  const maxValue = Math.max(1, ...values)
  const latest = points[points.length - 1]
  const option = {
    color: series.map((line) => line.color),
    tooltip: {
      trigger: 'axis',
      confine: true,
      backgroundColor: 'rgba(28, 28, 26, 0.92)',
      borderWidth: 0,
      textStyle: { color: '#fff', fontWeight: 700 },
      valueFormatter: (value) => `${value}${valueSuffix}`
    },
    legend: {
      bottom: 0,
      left: 0,
      itemWidth: 18,
      itemHeight: 6,
      textStyle: { color: '#555', fontSize: 12, fontWeight: 700 }
    },
    grid: { left: 34, right: 16, top: 18, bottom: 38, containLabel: true },
    xAxis: {
      type: 'category',
      boundaryGap: false,
      data: points.map((point) => point.label),
      axisLine: { lineStyle: { color: '#deded8' } },
      axisTick: { show: false },
      axisLabel: { color: '#777', fontWeight: 700 }
    },
    yAxis: {
      type: 'value',
      min: 0,
      max: Math.ceil(maxValue * 1.12),
      splitLine: { lineStyle: { color: '#ecece7', type: 'dashed' } },
      axisLabel: { color: '#777', fontWeight: 700, formatter: `{value}${valueSuffix}` }
    },
    series: series.map((line, index) => ({
      name: line.label,
      type: 'line',
      smooth: true,
      symbol: 'circle',
      symbolSize: index === 0 ? 7 : 6,
      lineStyle: { width: index === 0 ? 3.5 : 2.5 },
      itemStyle: { borderColor: '#fff', borderWidth: 2 },
      areaStyle: index === 0 ? { opacity: 0.18 } : undefined,
      emphasis: { focus: 'series' },
      data: points.map((point) => point[line.key] || 0)
    }))
  }

  return (
    <section className="subsection trend-card">
      <div className="subsection-title">
        <div>
          <h3>{title}</h3>
          <span>{subtitle}</span>
        </div>
        <div className="trend-latest">
          {series.map((line) => (
            <strong key={line.key} style={{ color: line.color }}>{latest[line.key] || 0}{valueSuffix}</strong>
          ))}
        </div>
      </div>
      <ReactEChartsCore echarts={echarts} className="echart line-echart" style={{ height: 226 }} option={option} notMerge lazyUpdate />
    </section>
  )
}

function BarChartBlock({ title, label, items, empty, tone = 'default' }) {
  const visibleItems = items.slice(0, 4)
  const hiddenCount = Math.max(0, items.length - visibleItems.length)
  const maxValue = Math.max(1, ...visibleItems.map((item) => item.value || 0))
  const total = items.reduce((sum, item) => sum + (item.value || 0), 0)
  const leader = visibleItems[0]
  const metaItems = visibleItems.filter((item) => item.meta).slice(0, 2)
  const colors = {
    risk: ['#a33333', '#c98243', '#587f9c', '#7aa17a', '#777'],
    status: ['#9b5b2a', '#467c60', '#80745f'],
    agent: ['#315f7d', '#4f8b79', '#9b6f3d', '#777'],
    developer: ['#5c7f4f', '#7f8650', '#925f49', '#4f778a']
  }[tone] || ['#4f7d62']
  const option = {
    color: colors,
    tooltip: {
      trigger: 'axis',
      axisPointer: { type: 'shadow' },
      confine: true,
      backgroundColor: 'rgba(28, 28, 26, 0.92)',
      borderWidth: 0,
      textStyle: { color: '#fff', fontWeight: 700 }
    },
    grid: { left: 8, right: 10, top: 8, bottom: 0, containLabel: true },
    xAxis: { type: 'value', show: false, max: Math.ceil(maxValue * 1.12) },
    yAxis: {
      type: 'category',
      inverse: true,
      data: visibleItems.map((item) => item.label),
      axisLine: { show: false },
      axisTick: { show: false },
      axisLabel: { color: '#2c2c2a', fontSize: 11, fontWeight: 850, width: 68, overflow: 'truncate' }
    },
    series: [{
      type: 'bar',
      data: visibleItems.map((item, index) => ({ value: item.value || 0, itemStyle: { color: colors[index % colors.length] } })),
      barWidth: 8,
      barGap: '40%',
      showBackground: true,
      backgroundStyle: { color: '#eeeee9', borderRadius: 999 },
      itemStyle: { borderRadius: 999 },
      label: {
        show: true,
        position: 'right',
        color: '#202020',
        fontWeight: 900,
        formatter: '{c}'
      }
    }]
  }
  return (
    <section className={`subsection chart-card chart-card-${tone}`}>
      <div className="chart-card-head">
        <div>
          <span>{label}</span>
          <h3>{title}</h3>
        </div>
        <strong><b>{total || '-'}</b><small>findings</small></strong>
      </div>
      {leader ? <p className="chart-card-note">{leader.label} 占比最高</p> : null}
      {visibleItems.length ? (
        <>
          <ReactEChartsCore echarts={echarts} className="echart bar-echart" style={{ height: Math.max(66, visibleItems.length * 24 + 16) }} option={option} notMerge lazyUpdate />
          {(metaItems.length || hiddenCount) ? (
            <div className="bar-meta-list">
              {metaItems.map((item) => <span key={item.label}>{item.label}: {item.meta}</span>)}
              {hiddenCount ? <span>+{hiddenCount} more</span> : null}
            </div>
          ) : null}
        </>
      ) : (
        <EmptyChart label={empty} />
      )}
    </section>
  )
}

function EmptyChart({ label }) {
  return (
    <div className="empty-chart" aria-label={label}>
      <div className="empty-chart-bars" aria-hidden="true">
        <span style={{ '--bar-width': '68%' }} />
        <span style={{ '--bar-width': '46%' }} />
        <span style={{ '--bar-width': '28%' }} />
      </div>
      <strong>{label}</strong>
    </div>
  )
}

function TableBlock({ title, headers, empty, children }) {
  const rows = Array.isArray(children) ? children.filter(Boolean) : children ? [children] : []
  return (
    <section className="subsection">
      <h3>{title}</h3>
      <div className="table-shell compact">
        <table className="data-table">
          <thead><tr>{headers.map((header) => <th key={header}>{header}</th>)}</tr></thead>
          <tbody>{rows.length ? rows : <tr><td colSpan={headers.length} className="empty-cell">{empty}</td></tr>}</tbody>
        </table>
      </div>
    </section>
  )
}

function ListBlock({ title, items, empty }) {
  return (
    <section className="subsection">
      <h3>{title}</h3>
      <ul className="plain-list">
        {items.length ? items.map((item, index) => (
          <li key={item.key || item || index}>{item.node || item}</li>
        )) : <li className="muted">{empty}</li>}
      </ul>
    </section>
  )
}

function MultiAgentOverlap({ report, items }) {
  return (
    <section className="subsection overlap-section">
      <div className="overlap-head">
        <h3>多 Agent 同时发现</h3>
        <span>{items.length} 组</span>
      </div>
      {items.length ? (
        <div className="overlap-list">
          {items.map((item, index) => (
            <article className="overlap-card" key={item.fingerprint || `${item.path}-${item.line}-${index}`}>
              <div className="overlap-card-main">
                <div className="overlap-title">{item.title || '(无标题)'}</div>
                <div className="overlap-meta">
                  <span>{item.owner}/{item.repo}#{item.pull_number}</span>
                  <span>{item.agent_count || item.agents?.length || 0} agents</span>
                </div>
                <div className="agent-pills">
                  {(item.agents || []).map((agent) => (
                    <span className="agent-pill" key={agent}><Users size={13} />{agent}</span>
                  ))}
                </div>
              </div>
              <SourceLink baseURL={report.gitea_url} finding={item} actionLabel="跳到行" prominent />
            </article>
          ))}
        </div>
      ) : (
        <div className="empty-overlap">暂无重叠问题</div>
      )}
    </section>
  )
}

function SourceLink({ baseURL, finding, actionLabel = '打开', prominent = false }) {
  const label = finding?.path ? `${finding.path}${finding.line ? `:${finding.line}` : ''}` : '无位置'
  const url = sourceURL(baseURL, finding)
  if (!url) return <code>{label}</code>
  return (
    <span className={prominent ? 'source-link source-link-prominent' : 'source-link'}>
      <code>{label}</code>
      <a className="line-link" href={url} target="_blank" rel="noreferrer">
        <ExternalLink size={14} />
        <span>{actionLabel}</span>
      </a>
    </span>
  )
}

function reviewerDefaults(settings, reviewer) {
  switch (reviewer) {
    case 'claude':
      return {
        model: settings.claude_model || '',
        base_url: settings.claude_base_url || '',
        provider_id: settings.cc_switch_provider_id || '',
        reasoning_effort: ''
      }
    case 'minimax':
      return {
        model: settings.minimax_model || '',
        base_url: settings.minimax_base_url || '',
        provider_id: settings.minimax_provider_id || '',
        reasoning_effort: ''
      }
    case 'codex':
    default:
      return {
        model: settings.model || '',
        base_url: settings.codex_base_url || '',
        provider_id: settings.codex_cc_switch_provider_id || '',
        reasoning_effort: settings.codex_reasoning_effort || ''
      }
  }
}

function ChatProbePanel() {
  const [settings, setSettings] = useState(() => ({ ...DEFAULT_SETTINGS }))
  const [ccSwitchOptions, setCCSwitchOptions] = useState(null)
  const [form, setForm] = useState(() => ({
    reviewer: 'codex',
    model: '',
    base_url: '',
    provider_id: '',
    reasoning_effort: 'high',
    prompt: 'Return exactly: OK'
  }))
  const [message, setMessage] = useState(null)
  const [reply, setReply] = useState(null)
  const [loading, setLoading] = useState(false)

  const loadProbeDefaults = useCallback(async () => {
    try {
      const [settingsPayload, optionsPayload] = await Promise.all([
        fetchJSON('/admin/api/settings', {}, 10000),
        fetchJSON('/admin/api/cc-switch/codex-options', {}, 10000)
      ])
      const nextSettings = { ...DEFAULT_SETTINGS, ...settingsPayload }
      setSettings(nextSettings)
      setCCSwitchOptions(optionsPayload)
      setForm((current) => ({ ...current, ...reviewerDefaults(nextSettings, current.reviewer) }))
      setMessage(null)
    } catch (error) {
      setMessage({ ok: false, text: `加载对话配置失败：${error.message}` })
    }
  }, [])

  useEffect(() => {
    loadProbeDefaults()
  }, [loadProbeDefaults])

  const modelOptions = useMemo(() => {
    if (form.reviewer === 'codex') {
      return uniqueValues([form.model, settings.model, ...(ccSwitchOptions?.models || []).map((item) => item.id)])
    }
    if (form.reviewer === 'claude') return uniqueValues([form.model, settings.claude_model])
    return uniqueValues([form.model, settings.minimax_model])
  }, [form.reviewer, form.model, settings, ccSwitchOptions])

  const providerOptions = useMemo(() => {
    if (form.reviewer === 'codex') {
      return uniqueValues([form.provider_id, settings.codex_cc_switch_provider_id, ...(ccSwitchOptions?.providers || []).map((item) => item.id)])
    }
    if (form.reviewer === 'claude') return uniqueValues([form.provider_id, settings.cc_switch_provider_id])
    return uniqueValues([form.provider_id, settings.minimax_provider_id])
  }, [form.reviewer, form.provider_id, settings, ccSwitchOptions])

  const baseURLOptions = useMemo(() => {
    if (form.reviewer === 'codex') {
      return uniqueValues([form.base_url, settings.codex_base_url, ...(ccSwitchOptions?.providers || []).map((item) => item.base_url)])
    }
    if (form.reviewer === 'claude') return uniqueValues([form.base_url, settings.claude_base_url])
    return uniqueValues([form.base_url, settings.minimax_base_url])
  }, [form.reviewer, form.base_url, settings, ccSwitchOptions])

  const setProbeField = (key, value) => {
    if (key === 'reviewer') {
      setForm((current) => ({ ...current, reviewer: value, ...reviewerDefaults(settings, value) }))
      setReply(null)
      return
    }
    setForm((current) => ({ ...current, [key]: value }))
  }

  const sendProbe = async () => {
    if (!form.prompt.trim()) {
      setMessage({ ok: false, text: '请输入要发送的内容' })
      return
    }
    setLoading(true)
    setReply(null)
    try {
      const payload = await fetchJSON('/admin/api/chat-probe', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(form)
      }, 120000)
      setReply(payload)
      setMessage(payload.ok ? { ok: true, text: `调用完成（${payload.elapsed_ms || 0}ms）` } : { ok: false, text: `调用失败：${payload.error || 'unknown error'}` })
    } catch (error) {
      setMessage({ ok: false, text: `调用失败：${error.message}` })
    } finally {
      setLoading(false)
    }
  }

  return (
    <section className="panel">
      <div className="section-head">
        <div>
          <h2>对话</h2>
          <p>临时选择 reviewer 和模型参数，直接验证当前通道能否调用。</p>
        </div>
        <div className="toolbar">
          <IconButton icon={RefreshCw} onClick={loadProbeDefaults} disabled={loading}>刷新配置</IconButton>
          <IconButton icon={loading ? LoaderCircle : Send} className={loading ? 'busy' : ''} onClick={sendProbe} disabled={loading}>{loading ? '发送中' : '发送'}</IconButton>
        </div>
      </div>

      <Message message={message} />

      <section className="config-group chat-probe">
        <div className="form-grid chat-controls">
          <label className="field">
            <span>Reviewer</span>
            <select value={form.reviewer} onChange={(event) => setProbeField('reviewer', event.target.value)}>
              <option value="codex">codex</option>
              <option value="claude">claude</option>
              <option value="minimax">minimax</option>
            </select>
          </label>
          <label className="field">
            <span>Model</span>
            <input list="chat-model-options" value={form.model} onChange={(event) => setProbeField('model', event.target.value)} placeholder="model id" />
            <datalist id="chat-model-options">{modelOptions.map((item) => <option key={item} value={item} />)}</datalist>
          </label>
          <label className="field">
            <span>Base URL</span>
            <input list="chat-base-url-options" value={form.base_url} onChange={(event) => setProbeField('base_url', event.target.value)} placeholder="https://relay.example.com/v1" />
            <datalist id="chat-base-url-options">{baseURLOptions.map((item) => <option key={item} value={item} />)}</datalist>
          </label>
          <label className="field">
            <span>Provider</span>
            <input list="chat-provider-options" value={form.provider_id} onChange={(event) => setProbeField('provider_id', event.target.value)} placeholder="cc-switch provider id" />
            <datalist id="chat-provider-options">{providerOptions.map((item) => <option key={item} value={item} />)}</datalist>
          </label>
          {form.reviewer === 'codex' ? (
            <label className="field">
              <span>思考强度</span>
              <select value={form.reasoning_effort} onChange={(event) => setProbeField('reasoning_effort', event.target.value)}>
                <option value="">cc-switch/provider 默认</option>
                {DEFAULT_REASONING_EFFORTS.map((item) => <option key={item} value={item}>{item}</option>)}
              </select>
            </label>
          ) : null}
        </div>

        <label className="field chat-prompt">
          <span>消息</span>
          <textarea value={form.prompt} onChange={(event) => setProbeField('prompt', event.target.value)} />
        </label>

        {reply ? (
          <div className="chat-result">
            <div className="group-head">
              <div>
                <h3>{reply.ok ? '模型响应' : '错误'}</h3>
                <p>{reply.reviewer || form.reviewer} · {reply.elapsed_ms || 0}ms</p>
              </div>
            </div>
            <pre className="status-box">{reply.ok ? (reply.output || '(empty)') : (reply.error || JSON.stringify(reply, null, 2))}</pre>
            {reply.debug ? <pre className="status-box">{JSON.stringify(reply.debug, null, 2)}</pre> : null}
          </div>
        ) : null}
      </section>
    </section>
  )
}

function ConfigPanel() {
  const [settings, setSettings] = useState(() => ({ ...DEFAULT_SETTINGS }))
  const [effectiveConfig, setEffectiveConfig] = useState(null)
  const [ccSwitchOptions, setCCSwitchOptions] = useState(null)
  const [status, setStatus] = useState(null)
  const [fetchingModels, setFetchingModels] = useState(false)
  const [settingsMessage, setSettingsMessage] = useState(null)

  const loadSettings = useCallback(async () => {
    try {
      const payload = await fetchJSON('/admin/api/settings', {}, 10000)
      setSettings({ ...DEFAULT_SETTINGS, ...payload })
      setSettingsMessage({ ok: true, text: '已加载配置' })
    } catch (error) {
      setSettingsMessage({ ok: false, text: `加载失败：${error.message}` })
    }
  }, [])

  const loadEffectiveConfig = useCallback(async () => {
    try {
      const payload = await fetchJSON('/admin/api/effective-config', {}, 10000)
      setEffectiveConfig(payload)
    } catch (error) {
      setSettingsMessage({ ok: false, text: `读取 effective config 失败：${error.message}` })
    }
  }, [])

  const loadCCSwitchOptions = useCallback(async () => {
    try {
      const payload = await fetchJSON('/admin/api/cc-switch/codex-options', {}, 10000)
      setCCSwitchOptions(payload)
    } catch (error) {
      setSettingsMessage({ ok: false, text: `读取 cc-switch 选项失败：${error.message}` })
    }
  }, [])

  const checkStatus = useCallback(async () => {
    try {
      const payload = await fetchJSON('/admin/api/status', {}, 20000)
      setStatus(payload)
    } catch (error) {
      setStatus({ ok: false, status: `检查失败：${error.message}` })
    }
  }, [])

  const fetchCodexModels = useCallback(async () => {
    setFetchingModels(true)
    try {
      const payload = await fetchJSON('/admin/api/cc-switch/codex-options/fetch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          provider_id: settings.codex_cc_switch_provider_id || '',
          base_url: settings.codex_base_url || ''
        })
      }, 70000)
      setCCSwitchOptions(payload)
      setSettingsMessage({ ok: true, text: '已从 cc-switch 获取 Codex 模型列表' })
    } catch (error) {
      setSettingsMessage({ ok: false, text: `获取 Codex 模型失败：${error.message}` })
    } finally {
      setFetchingModels(false)
    }
  }, [settings.codex_base_url, settings.codex_cc_switch_provider_id])

  useEffect(() => {
    loadSettings()
    loadEffectiveConfig()
    loadCCSwitchOptions()
  }, [loadSettings, loadEffectiveConfig, loadCCSwitchOptions])

  const fieldMeta = useMemo(() => {
    const modelOptions = uniqueValues([
      settings.model,
      ...(ccSwitchOptions?.models || []).map((item) => item.id)
    ])
    const providerOptions = uniqueValues([
      settings.codex_cc_switch_provider_id,
      ...(ccSwitchOptions?.providers || []).map((item) => item.id)
    ])
    const baseURLOptions = uniqueValues([
      settings.codex_base_url,
      ...(ccSwitchOptions?.providers || []).map((item) => item.base_url)
    ])
    const reasoningOptions = ['', ...uniqueValues([
      settings.codex_reasoning_effort,
      ...(ccSwitchOptions?.reasoning_efforts || []),
      ...DEFAULT_REASONING_EFFORTS
    ])]
    return {
      ...SETTING_META,
      model: { ...SETTING_META.model, type: 'datalist', options: modelOptions },
      codex_base_url: { ...SETTING_META.codex_base_url, type: 'datalist', options: baseURLOptions },
      codex_cc_switch_provider_id: { ...SETTING_META.codex_cc_switch_provider_id, type: 'datalist', options: providerOptions },
      codex_reasoning_effort: { ...SETTING_META.codex_reasoning_effort, options: reasoningOptions }
    }
  }, [settings.model, settings.codex_base_url, settings.codex_cc_switch_provider_id, settings.codex_reasoning_effort, ccSwitchOptions])

  const setField = (key, value) => {
    setSettings((current) => ({ ...current, [key]: value }))
  }

  const saveGroup = async (keys, label) => {
    const payload = {}
    keys.forEach((key) => {
      const value = settings[key] ?? ''
      if (SECRET_FIELDS.has(key) && value === REDACTED) return
      payload[key] = value
    })
    if (!Object.keys(payload).length) {
      setSettingsMessage({ ok: true, text: '没有需要保存的字段' })
      return
    }
    try {
      const result = await fetchJSON('/admin/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ settings: payload })
      }, 12000)
      setSettingsMessage({ ok: true, text: `已保存 ${label}（${result.updated || 0} 项）` })
      await Promise.all([loadSettings(), loadEffectiveConfig(), loadCCSwitchOptions(), checkStatus()])
    } catch (error) {
      setSettingsMessage({ ok: false, text: `保存失败：${error.message}` })
      if (error.status === 401) {
        window.location.href = '/admin/login'
      }
    }
  }

  return (
    <section className="panel">
      <div className="section-head">
        <div>
          <h2>配置</h2>
          <p>保存后写入数据库，后续 review 读取最新设置。</p>
        </div>
        <div className="toolbar">
          <IconButton icon={RefreshCw} onClick={() => { loadSettings(); loadEffectiveConfig(); loadCCSwitchOptions(); }}>刷新</IconButton>
          <IconButton icon={Download} onClick={fetchCodexModels} disabled={fetchingModels}>{fetchingModels ? '获取中' : '获取 Codex 模型'}</IconButton>
          <IconButton icon={Activity} onClick={checkStatus}>检测 Reviewer</IconButton>
        </div>
      </div>

      <Message message={settingsMessage} />

      <ConfigGroup title="通用" keys={FIELD_GROUPS.common} settings={settings} fieldMeta={fieldMeta} setField={setField} onSave={() => saveGroup(FIELD_GROUPS.common, '通用设置')} />
      <ConfigGroup title="Codex" keys={FIELD_GROUPS.codex} settings={settings} fieldMeta={fieldMeta} setField={setField} onSave={() => saveGroup(FIELD_GROUPS.codex, 'Codex 设置')} />
      <ConfigGroup title="Claude" keys={FIELD_GROUPS.claude} settings={settings} fieldMeta={fieldMeta} setField={setField} onSave={() => saveGroup(FIELD_GROUPS.claude, 'Claude 设置')} />
      <ConfigGroup title="MiniMax" keys={FIELD_GROUPS.minimax} settings={settings} fieldMeta={fieldMeta} setField={setField} onSave={() => saveGroup(FIELD_GROUPS.minimax, 'MiniMax 设置')} />
      <ConfigGroup title="告警 Git 缓存" keys={FIELD_GROUPS.analysisCache} settings={settings} fieldMeta={fieldMeta} setField={setField} onSave={() => saveGroup(FIELD_GROUPS.analysisCache, '告警 Git 缓存设置')} />

      <section className="config-group">
        <div className="group-head">
          <div>
            <h3>Reviewer 状态</h3>
            <p>{status ? (status.ok ? '可用' : '不可用') : '未检测'}</p>
          </div>
        </div>
        {status ? <pre className="status-box">{status.status || JSON.stringify(status, null, 2)}</pre> : null}
      </section>

      <EffectiveConfig data={effectiveConfig} />
    </section>
  )
}

function ConfigGroup({ title, keys, settings, fieldMeta, setField, onSave }) {
  return (
    <section className="config-group">
      <div className="group-head">
        <h3>{title}</h3>
        <IconButton icon={Save} onClick={onSave}>保存</IconButton>
      </div>
      <div className="form-grid">
        {keys.map((key) => <SettingField key={key} name={key} value={settings[key] ?? ''} meta={fieldMeta[key]} onChange={(value) => setField(key, value)} />)}
      </div>
    </section>
  )
}

function SettingField({ name, value, meta: fieldMeta, onChange }) {
  const meta = fieldMeta || SETTING_META[name] || { label: name }
  const id = `setting-${name}`
  return (
    <label className="field" htmlFor={id}>
      <span>{meta.label}</span>
      {meta.type === 'select' ? (
        <select id={id} value={value} onChange={(event) => onChange(event.target.value)}>
          {meta.options.map((option) => <option key={option || '__empty'} value={option}>{option || 'cc-switch/provider 默认'}</option>)}
        </select>
      ) : meta.type === 'datalist' ? (
        <>
          <input
            id={id}
            list={`${id}-options`}
            type="text"
            value={value}
            placeholder={meta.placeholder || ''}
            autoComplete="off"
            onChange={(event) => onChange(event.target.value)}
          />
          <datalist id={`${id}-options`}>
            {(meta.options || []).map((option) => <option key={option} value={option} />)}
          </datalist>
        </>
      ) : (
        <input
          id={id}
          type={meta.secret ? 'password' : 'text'}
          value={value}
          placeholder={meta.placeholder || ''}
          autoComplete="off"
          onChange={(event) => onChange(event.target.value)}
        />
      )}
    </label>
  )
}

function EffectiveConfig({ data }) {
  const rows = useMemo(() => {
    if (!data) return []
    return FIELDS.filter((key) => key in data || `${key}_set` in data).map((key) => {
      const value = key in data ? data[key] : data[`${key}_set`] ? 'set' : 'empty'
      return [key, String(value)]
    })
  }, [data])

  return (
    <section className="config-group">
      <div className="group-head">
        <div>
          <h3>Effective Config</h3>
          <p>{data?.runtime_reload_note || '读取当前运行时配置快照。'}</p>
        </div>
      </div>
      <div className="table-shell compact">
        <table className="data-table">
          <tbody>
            {rows.length ? rows.map(([key, value]) => <tr key={key}><td><code>{key}</code></td><td>{value}</td></tr>) : <tr><td className="empty-cell">暂无配置快照</td></tr>}
          </tbody>
        </table>
      </div>
    </section>
  )
}

export default App
