import { useEffect, useState } from 'react'
import {
  AnalysisProvider, AnalysisProvidersResp, AnalysisSettings,
  listAnalysisProviders, createAnalysisProvider, updateAnalysisProvider,
  deleteAnalysisProvider, testAnalysisProvider, getAnalysisSettings, setAnalysisSettings,
  roleLabel,
} from '../api'
import './AnalysisProviders.css'

/**
 * 「分析平台」页：配置日志分析的提供方。
 *
 * 这个页面存在的理由（D1/D1c）：
 *  - LogShare 是公益合作的首选，但它会维护、会限流 —— 只绑一家，它一停整条链路就不可用；
 *  - 用户应该能接**自己的**平台（免费额度、自建网关、商业 API），用自己的额度与隐私边界；
 *  - mclo.gs 这类"不做 AI、但能把日志变成可分享链接"的服务也要能挂上同一条链。
 *
 * 两条硬约束在界面上必须写清楚（否则用户以为只是填个表）：
 *  ① **API Key 加密保存、永不回显**：列表里只显示末 4 位；
 *  ② 平台地址默认**禁止内网**（base_url 是用户可控 URL，而面板会带着 key 去请求它），
 *     自建网关确实在内网时由管理员显式打开开关。
 */

const KIND_LABEL: Record<string, string> = {
  logshare: 'LogShare（AI 分析）',
  mclogs: 'mclo.gs（分享链接）',
  openai: '自配平台（OpenAI 兼容）',
  'builtin-rules': '内置规则（离线）',
}

function emptyForm() {
  return {
    id: 0,
    name: '',
    base_url: '',
    model: '',
    api_key: '',
    prompt: '',
    global: false,
    enabled: true,
    timeout_sec: 300,
  }
}

export default function AnalysisProviders({ role }: { role?: string }) {
  const [data, setData] = useState<AnalysisProvidersResp | null>(null)
  const [settings, setSettings] = useState<AnalysisSettings | null>(null)
  const [form, setForm] = useState(emptyForm())
  const [showForm, setShowForm] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  // 每个提供方的自检结果（键是 id）：填完就点一下，比"分析失败再回来猜"省事得多
  const [tests, setTests] = useState<Record<number, { ok: boolean; text: string }>>({})
  const [rateForm, setRateForm] = useState({ per_min: 6, per_day: 200, allow_private: false })
  // 求助模板编辑框：**独立于 rateForm**，因为它是长文本、保存也是单独一个动作
  //（混在一起的话，改一行模板会连带把速率设置也提交一遍，容易误伤）
  const [templateForm, setTemplateForm] = useState('')
  const isAdmin = role === 'admin'

  const load = async () => {
    try {
      const [p, s] = await Promise.all([listAnalysisProviders(), getAnalysisSettings()])
      setData(p)
      setSettings(s)
      setRateForm({ per_min: s.rate_per_min, per_day: s.rate_per_day, allow_private: s.allow_private })
      setTemplateForm(s.help_template || s.help_template_default || '')
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => { load() }, [])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true); setError(''); setMsg('')
    try {
      const payload: any = {
        name: form.name, kind: 'openai', base_url: form.base_url, model: form.model,
        prompt: form.prompt, timeout_sec: Number(form.timeout_sec) || 300, enabled: form.enabled,
      }
      // key 留空 = 不修改（改模型名时不必重填 key）
      if (form.api_key.trim() !== '') payload.api_key = form.api_key.trim()
      if (form.id) {
        await updateAnalysisProvider(form.id, payload)
        setMsg('已保存')
      } else {
        payload.global = form.global
        const r = await createAnalysisProvider(payload)
        setMsg(r.message || '已保存')
      }
      setShowForm(false)
      setForm(emptyForm())
      await load()
    } catch (err: any) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const runTest = async (id: number) => {
    setTests((t) => ({ ...t, [id]: { ok: true, text: '测试中…' } }))
    try {
      const r = await testAnalysisProvider(id)
      setTests((t) => ({
        ...t,
        [id]: r.ok
          ? { ok: true, text: r.message || `连通（${r.took_ms ?? '?'} ms）` }
          : { ok: false, text: r.error || '失败' },
      }))
    } catch (e: any) {
      setTests((t) => ({ ...t, [id]: { ok: false, text: e.message } }))
    }
  }

  const remove = async (p: AnalysisProvider) => {
    if (!confirm(`删除「${p.name}」？\n\n保存的 API Key 会一起删除；已经用它做过的分析记录不受影响。`)) return
    try {
      await deleteAnalysisProvider(p.id)
      setMsg('已删除')
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const saveSettings = async () => {
    setBusy(true); setError(''); setMsg('')
    try {
      await setAnalysisSettings({
        rate_per_min: Number(rateForm.per_min) || 6,
        rate_per_day: Number(rateForm.per_day) || 200,
        allow_private: rateForm.allow_private,
      })
      setMsg('设置已保存并立即生效')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  // 求助模板保存：校验在后端（那里有唯一的真值），这里只把错误原文摆出来 ——
  // 前端复制一份校验规则，迟早会和后端说的不一样。
  const saveTemplate = async () => {
    setBusy(true); setError(''); setMsg('')
    try {
      await setAnalysisSettings({ help_template: templateForm })
      setMsg('求助模板已保存，下次上传（mclo.gs 通道）就按新模板生成')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const custom = data?.custom || []

  return (
    <div className="ap-page">
      {error && <div className="error-banner" onClick={() => setError('')}>{error}</div>}
      {msg && <div className="success-banner" onClick={() => setMsg('')}>{msg}</div>}

      <p className="page-lead">
        日志分析会按下面的顺序依次尝试。<strong>LogShare 是首选</strong>（公益合作，给 AI 结论）；
        它不可用时自动回退到 <strong>mclo.gs</strong>（不做 AI，只把日志变成可分享的链接，
        方便你去社区求助）；你还可以接入<strong>自己的平台</strong>（用自己的额度与隐私边界）。
      </p>

      {/* ---- 当前顺序 ---- */}
      <section className="ap-card">
        <div className="ap-card-title">当前的尝试顺序</div>
        <div className="ap-chain">
          {(data?.chain || []).map((c, i) => (
            <div key={`${c.kind}-${c.id}-${i}`} className="ap-chain-item">
              <span className="ap-chain-idx">{i + 1}</span>
              <span className={`ap-kind kind-${c.kind}`}>{KIND_LABEL[c.kind] || c.kind}</span>
              <span className="ap-chain-name">{c.name}</span>
              <span className="ap-chain-reason">{c.reason}</span>
            </div>
          ))}
          {(!data || data.chain.length === 0) && <div className="muted">（没有可用的提供方）</div>}
        </div>
      </section>

      {/* ---- 内置提供方 ---- */}
      <section className="ap-card">
        <div className="ap-card-title">
          内置提供方
          <span className="ap-card-sub">由面板维护，不需要配置</span>
        </div>
        <div className="ap-list">
          {(data?.builtin || []).map((p) => (
            <div key={p.kind} className="ap-item">
              <div className="ap-item-main">
                <div className="ap-item-name">
                  {p.name}
                  <span className={`ap-kind kind-${p.kind}`}>{KIND_LABEL[p.kind] || p.kind}</span>
                  {!p.enabled && <span className="ap-off">已停用</span>}
                </div>
                <div className="ap-item-desc">
                  {p.kind === 'logshare'
                    ? '第三方 AI 分析（公益合作）。每次上传前仍需手动同意对方条款，「过滤玩家聊天行」默认开启。'
                    : '保底通道：只生成可分享的日志链接并统计 ERROR 行数，**不会给出 AI 结论**，用于去 MC 社区求助。'}
                </div>
              </div>
              {p.kind === 'logshare' && (
                <div className="ap-item-note">
                  {p.enabled
                    ? '当前已启用（总管理员可在「日志分析」页一键关闭）'
                    : '当前已关闭（可在「日志分析」页由总管理员开启）'}
                </div>
              )}
            </div>
          ))}
        </div>
      </section>

      {/* ---- 自配平台 ---- */}
      <section className="ap-card">
        <div className="ap-card-title">
          我配置的平台
          <span className="ap-card-sub">API Key 加密保存、永不回显</span>
          <span className="spacer" />
          {data?.can_manage && (
            <button className="primary" onClick={() => { setForm(emptyForm()); setShowForm(true) }}>
              + 添加平台
            </button>
          )}
        </div>

        {!data?.can_manage && (
          <div className="ap-empty">
            只有总管理员与节点用户可以配置自己的分析平台。你可以直接使用上面两个内置提供方。
          </div>
        )}

        {data?.can_manage && custom.length === 0 && (
          <div className="ap-empty">
            还没有配置。想用自己的额度（例如免费额度的模型、或自建的 OpenAI 兼容网关）时，
            点右上角「添加平台」。
          </div>
        )}

        <div className="ap-list">
          {custom.map((p) => (
            <div key={p.id} className="ap-item">
              <div className="ap-item-main">
                <div className="ap-item-name">
                  {p.name}
                  <span className={`ap-kind kind-${p.kind}`}>{KIND_LABEL[p.kind] || p.kind}</span>
                  {p.owner_id === 0 && <span className="ap-global">全局</span>}
                  {!p.enabled && <span className="ap-off">已停用</span>}
                </div>
                <div className="ap-item-desc mono">
                  {p.base_url}
                  {p.model && ` · ${p.model}`}
                  {p.key_hint && ` · Key ${p.key_hint}`}
                </div>
                {tests[p.id] && (
                  <div className={tests[p.id].ok ? 'ap-test-ok' : 'ap-test-fail'}>
                    {tests[p.id].ok ? '✅ ' : '❌ '}{tests[p.id].text}
                  </div>
                )}
              </div>
              <div className="ap-item-ops">
                {/* 全局平台（owner_id === 0）的「测试连接」会把**解密后的 API Key**
                    发到该平台配置的地址上，所以服务端只允许总管理员触发
                    （2026-10-01 安全审查）。这里同步把按钮收起来 ——
                    否则节点用户点下去只会得到一个 403，看着像功能坏了。 */}
                {p.owner_id === 0 && !isAdmin ? (
                  <span className="ap-readonly">全局平台由总管理员维护</span>
                ) : (
                  <>
                    <button onClick={() => runTest(p.id)}>测试连接</button>
                    <button onClick={() => {
                      setForm({
                        id: p.id, name: p.name, base_url: p.base_url, model: p.model,
                        api_key: '', prompt: p.prompt || '', global: p.owner_id === 0,
                        enabled: p.enabled, timeout_sec: p.timeout_sec || 300,
                      })
                      setShowForm(true)
                    }}>编辑</button>
                    <button className="danger" onClick={() => remove(p)}>删除</button>
                  </>
                )}
              </div>
            </div>
          ))}
        </div>

        {data?.can_manage && (
          <div className="ap-hint">
            <b>建议使用免费平台 API</b>：各家的免费额度模型、或自建的
            OpenAI 兼容网关（vLLM / LiteLLM / One-API 等）都能填在这里。
            地址一般要以 <code>/v1</code> 结尾，模型名照平台文档填；填完点一次「测试连接」，
            错误原文会直接显示出来（Key 错、少了 <code>/v1</code>、模型名不对，三种提示完全不同）。
          </div>
        )}
      </section>

      {/* ---- 管理员设置 ---- */}
      {settings?.can_manage && (
        <section className="ap-card">
          <div className="ap-card-title">
            全局设置
            <span className="ap-card-sub">仅总管理员</span>
          </div>
          <div className="ap-form-row">
            <label>
              每分钟最多调用
              <input type="number" min={1} max={600} value={rateForm.per_min}
                onChange={(e) => setRateForm({ ...rateForm, per_min: Number(e.target.value) })} />
            </label>
            <label>
              每天最多调用
              <input type="number" min={1} max={100000} value={rateForm.per_day}
                onChange={(e) => setRateForm({ ...rateForm, per_day: Number(e.target.value) })} />
            </label>
            <label className="ap-check">
              <input type="checkbox" checked={rateForm.allow_private}
                onChange={(e) => setRateForm({ ...rateForm, allow_private: e.target.checked })} />
              允许平台地址指向内网
            </label>
          </div>
          <div className="ap-hint">
            速率限制按**用户**计数，每一次对外调用都算一次（含回退后的第二次）——
            它保护的是你自己的 key，以及 LogShare 这份公益额度不被单个租户吃光。<br />
            内网开关默认关闭：<code>base_url</code> 是用户可控的地址，而面板会**带着 API Key** 去请求它。
            自建网关确实在内网（甚至就是本机）时才打开；云元数据地址（169.254.169.254）
            无论开关如何都会被拒绝。
          </div>
          <div className="ap-ops">
            <button className="primary" onClick={saveSettings} disabled={busy}>保存设置</button>
          </div>

          {/* ---- 求助模板 ---- */}
          <div className="ap-subtitle">
            求助模板
            <span className="muted">
              走 mclo.gs（保底通道）时，面板替你拼好的那段"可直接贴出去"的求助帖
            </span>
          </div>
          <div className="ap-hint">
            面板只负责把 <code>{'{占位符}'}</code> 换成真实信息，措辞归你改 ——
            不同社区要的东西不一样（有的要 crash-report 全文，有的要 mods 列表，有的群规要求先写"已试过什么"）。<br />
            行内的占位符<b>全部为空时整行会被丢掉</b>（不会留下"· Java："这种悬空标签）；
            <code>{'{url}'}</code> 必须保留，否则求助帖里就没有日志链接了。
            模板清空 = 恢复内置默认。
          </div>
          <div className="ap-placeholders">
            {settings?.help_placeholders?.map((ph) => (
              <span className="ap-ph" key={ph.name}
                title={ph.desc}
                onClick={() => setTemplateForm((t) => `${t}{${ph.name}}`)}>
                {'{' + ph.name + '}'}
              </span>
            ))}
          </div>
          <textarea className="ap-template" rows={14} value={templateForm} spellCheck={false}
            onChange={(e) => setTemplateForm(e.target.value)} />
          <div className="ap-ops">
            <button onClick={() => setTemplateForm(settings?.help_template_default || '')} disabled={busy}>
              恢复内置默认（填入编辑框）
            </button>
            <button className="primary" onClick={saveTemplate} disabled={busy}>
              {busy ? '保存中…' : '保存模板'}
            </button>
            {settings?.help_template_is_default && <span className="muted">当前：内置默认</span>}
          </div>
        </section>
      )}

      {/* ---- 新增/编辑表单 ---- */}
      {showForm && (
        <div className="confirm-mask" onClick={() => setShowForm(false)}>
          <form className="ap-form" onClick={(e) => e.stopPropagation()} onSubmit={submit}>
            <div className="ap-form-head">
              {form.id ? `编辑「${form.name}」` : '添加分析平台'}
            </div>
            <label>
              名称
              <input value={form.name} placeholder="例如：我的 DeepSeek"
                onChange={(e) => setForm({ ...form, name: e.target.value })} required />
            </label>
            <label>
              平台地址（base_url）
              <input value={form.base_url} placeholder="https://api.deepseek.com/v1"
                onChange={(e) => setForm({ ...form, base_url: e.target.value })} required />
              <em>多为 OpenAI 兼容形态；地址一般以 <code>/v1</code> 结尾（少了会 404）。</em>
            </label>
            <label>
              模型名
              <input value={form.model} placeholder="deepseek-chat"
                onChange={(e) => setForm({ ...form, model: e.target.value })} required />
            </label>
            <label>
              API Key
              <input type="password" value={form.api_key} autoComplete="new-password"
                placeholder={form.id ? '留空表示不修改' : 'sk-...'}
                onChange={(e) => setForm({ ...form, api_key: e.target.value })} />
              <em>加密保存（本机密钥文件），接口只回显末 4 位；日志与审计里都不会出现它。</em>
            </label>
            <label>
              自定义提示词（可选）
              <textarea value={form.prompt} rows={3}
                placeholder="留空则用面板内置的排障提示词"
                onChange={(e) => setForm({ ...form, prompt: e.target.value })} />
            </label>
            <div className="ap-form-row">
              <label className="ap-check">
                <input type="checkbox" checked={form.enabled}
                  onChange={(e) => setForm({ ...form, enabled: e.target.checked })} />
                启用
              </label>
              {isAdmin && !form.id && (
                <label className="ap-check">
                  <input type="checkbox" checked={form.global}
                    onChange={(e) => setForm({ ...form, global: e.target.checked })} />
                  全局（所有用户可用）
                </label>
              )}
            </div>
            <div className="ap-form-ops">
              <button type="button" onClick={() => setShowForm(false)}>取消</button>
              <button className="primary" type="submit" disabled={busy}>{busy ? '保存中…' : '保存'}</button>
            </div>
          </form>
        </div>
      )}

      <p className="ap-footnote muted">
        当前登录角色：{roleLabel(role)}。{data?.can_manage
          ? '你可以配置自己的平台（只有你自己能用）。'
          : '如需接入自己的平台，请联系总管理员。'}
      </p>
    </div>
  )
}
