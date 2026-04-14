import { useState, useCallback, useEffect } from 'react';
import { cssVar } from '@airgate/theme';

/** 批量 Session Key 换取结果（单条） */
export interface BatchExchangeResult {
  accountType: string;
  accountName: string;
  credentials: Record<string, string>;
  status: 'ok' | 'failed';
  error?: string;
}

/** 批量导入账号项 */
export interface BatchAccountInput {
  name: string;
  type: string;
  credentials: Record<string, string>;
}

/** 账号表单 Props（由核心 AccountsPage 注入） */
export interface AccountFormProps {
  credentials: Record<string, string>;
  onChange: (credentials: Record<string, string>) => void;
  mode: 'create' | 'edit';
  accountType?: string;
  onAccountTypeChange?: (type: string) => void;
  onSuggestedName?: (name: string) => void;
  /** 进入/退出批量模式时通知外层，用于隐藏"下一步/创建"按钮 */
  onBatchModeChange?: (isBatch: boolean) => void;
  /** 批量导入账号，由核心侧调用 accountsApi.import 完成落库 */
  onBatchImport?: (accounts: BatchAccountInput[]) => Promise<{ imported: number; failed: number }>;
  oauth?: {
    start: () => Promise<{ authorizeURL: string; state: string }>;
    exchange: (callbackURL: string) => Promise<{
      accountType: string;
      accountName: string;
      credentials: Record<string, string>;
    }>;
    batchExchange?: (sessionKeys: string[]) => Promise<BatchExchangeResult[]>;
  };
}

const inputStyle: React.CSSProperties = {
  display: 'block',
  width: '100%',
  borderRadius: cssVar('radiusMd'),
  border: `1px solid ${cssVar('glassBorder')}`,
  backgroundColor: cssVar('bgSurface'),
  padding: '0.5rem 0.75rem',
  fontSize: '0.875rem',
  color: cssVar('text'),
  outline: 'none',
  transition: 'border-color 0.2s, box-shadow 0.2s',
};

const labelStyle: React.CSSProperties = {
  display: 'block',
  fontSize: '0.75rem',
  fontWeight: 500,
  color: cssVar('textSecondary'),
  textTransform: 'uppercase',
  letterSpacing: '0.05em',
  marginBottom: '0.375rem',
};

const cardStyle: React.CSSProperties = {
  border: `1px solid ${cssVar('glassBorder')}`,
  borderRadius: cssVar('radiusLg'),
  padding: '1rem',
  cursor: 'pointer',
  transition: 'border-color 0.2s, background-color 0.2s',
};

const cardActiveStyle: React.CSSProperties = {
  ...cardStyle,
  borderColor: cssVar('primary'),
  backgroundColor: cssVar('primarySubtle'),
};

const descStyle: React.CSSProperties = {
  fontSize: '0.75rem',
  color: cssVar('textTertiary'),
  marginTop: '0.25rem',
};

const pillStyle: React.CSSProperties = {
  display: 'inline-block',
  padding: '0.25rem 0.625rem',
  borderRadius: '9999px',
  fontSize: '0.75rem',
  cursor: 'pointer',
  transition: 'all 0.15s',
  border: `1px solid ${cssVar('glassBorder')}`,
  color: cssVar('textSecondary'),
  backgroundColor: 'transparent',
};

const pillActiveStyle: React.CSSProperties = {
  ...pillStyle,
  borderColor: cssVar('primary'),
  color: cssVar('primary'),
  backgroundColor: cssVar('primarySubtle'),
};

const sectionStyle: React.CSSProperties = {
  border: `1px solid ${cssVar('glassBorder')}`,
  borderRadius: cssVar('radiusLg'),
  padding: '1rem',
  backgroundColor: cssVar('bgSurface'),
};

// ── 类型定义 ──

/** UI 分类：Claude Code（OAuth 系列）或 Claude Console（API Key） */
type UICategory = 'claude_code' | 'claude_console';

/** 后端账号类型 */
type AccountType = 'apikey' | 'oauth' | 'session_key';

/** Claude Code 内部的获取方式 */
type AcquireMethod = 'session_key' | 'browser_oauth';

function detectCategory(accountType?: string, credentials?: Record<string, string>): UICategory {
  if (accountType === 'apikey') return 'claude_console';
  if (accountType === 'oauth' || accountType === 'session_key') return 'claude_code';
  if (credentials?.api_key) return 'claude_console';
  if (credentials?.session_key || credentials?.access_token) return 'claude_code';
  return 'claude_code';
}

function detectAcquireMethod(accountType?: string, credentials?: Record<string, string>): AcquireMethod {
  if (accountType === 'session_key' || credentials?.session_key) return 'session_key';
  return 'session_key'; // 默认 session_key，最常用
}

// ── 状态提示组件 ──

function StatusMessage({ status }: { status: { type: 'info' | 'success' | 'error'; text: string } | null }) {
  if (!status) return null;
  return (
    <div
      style={{
        fontSize: '0.75rem',
        color:
          status.type === 'error'
            ? cssVar('danger')
            : status.type === 'success'
              ? cssVar('success')
              : cssVar('textSecondary'),
      }}
    >
      {status.text}
    </div>
  );
}

// ── 主组件 ──

export function AccountForm({
  credentials,
  onChange,
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  mode: _mode,
  accountType: propType,
  onAccountTypeChange,
  onSuggestedName,
  onBatchModeChange,
  onBatchImport,
  oauth,
}: AccountFormProps) {
  const [category, setCategory] = useState<UICategory>(
    detectCategory(propType, credentials),
  );
  const [acquireMethod, setAcquireMethod] = useState<AcquireMethod>(
    detectAcquireMethod(propType, credentials),
  );

  // 单个 / 批量 模式
  const [sessionKeyMode, setSessionKeyMode] = useState<'single' | 'batch'>('single');
  const [batchText, setBatchText] = useState('');
  const [batchPhase, setBatchPhase] = useState<'input' | 'exchanging' | 'result'>('input');
  const [batchResults, setBatchResults] = useState<BatchExchangeResult[]>([]);
  const [batchImportedCount, setBatchImportedCount] = useState(0);

  // OAuth 浏览器授权流程状态
  const [authorizeURL, setAuthorizeURL] = useState('');
  const [callbackURL, setCallbackURL] = useState('');
  const [oauthLoading, setOauthLoading] = useState(false);
  const [oauthStatus, setOauthStatus] = useState<{ type: 'info' | 'success' | 'error'; text: string } | null>(null);

  // 是否处于批量模式（需要隐藏外层"下一步/创建"按钮）
  const isBatchActive =
    category === 'claude_code' && acquireMethod === 'session_key' && sessionKeyMode === 'batch';

  useEffect(() => {
    onBatchModeChange?.(isBatchActive);
  }, [isBatchActive, onBatchModeChange]);

  function parseSessionKeys(text: string): string[] {
    return text
      .split('\n')
      .map((line) => line.trim())
      .filter((line) => line.length > 0 && !line.startsWith('#'));
  }

  const resetBatchState = useCallback(() => {
    setBatchText('');
    setBatchPhase('input');
    setBatchResults([]);
    setBatchImportedCount(0);
  }, []);

  const handleBatchImport = useCallback(async () => {
    if (!oauth?.batchExchange || !onBatchImport) {
      setOauthStatus({ type: 'error', text: '当前环境不支持批量导入' });
      return;
    }
    const keys = parseSessionKeys(batchText);
    if (keys.length === 0) {
      setOauthStatus({ type: 'error', text: '请至少输入一个 Session Key' });
      return;
    }
    setBatchPhase('exchanging');
    setOauthStatus({ type: 'info', text: `正在批量换取 ${keys.length} 个 Token...` });
    try {
      const results = await oauth.batchExchange(keys);
      setBatchResults(results);
      const successItems = results.filter((r) => r.status === 'ok' && r.credentials);
      if (successItems.length > 0) {
        const accounts: BatchAccountInput[] = successItems.map((r) => ({
          name: r.accountName || 'Claude Code',
          type: r.accountType || 'oauth',
          credentials: r.credentials,
        }));
        const importResp = await onBatchImport(accounts);
        setBatchImportedCount(importResp.imported);
      }
      setBatchPhase('result');
      setOauthStatus(null);
    } catch (err) {
      setBatchPhase('input');
      setOauthStatus({ type: 'error', text: err instanceof Error ? err.message : '批量导入失败' });
    }
  }, [batchText, oauth, onBatchImport]);

  const updateField = useCallback(
    (key: string, value: string) => {
      onChange({ ...credentials, [key]: value });
    },
    [credentials, onChange],
  );

  // 根据 category + method 推导后端 account type
  const resolveAccountType = useCallback(
    (cat: UICategory, _method: AcquireMethod): AccountType => {
      if (cat === 'claude_console') return 'apikey';
      return 'oauth'; // Session Key 换取后也是 OAuth 类型
    },
    [],
  );

  // 切换大类
  const handleCategoryChange = useCallback(
    (cat: UICategory) => {
      setCategory(cat);
      setAuthorizeURL('');
      setCallbackURL('');
      setOauthStatus(null);
      resetBatchState();
      setSessionKeyMode('single');
      if (cat === 'claude_console') {
        onAccountTypeChange?.('apikey');
        onChange({ api_key: '', base_url: '' });
      } else {
        const type = resolveAccountType(cat, acquireMethod);
        onAccountTypeChange?.(type);
        onChange({ session_key: '', access_token: '', refresh_token: '', expires_at: '', base_url: '' });
      }
    },
    [onChange, onAccountTypeChange, resolveAccountType, acquireMethod, resetBatchState],
  );

  // 切换获取方式
  const handleAcquireMethodChange = useCallback(
    (method: AcquireMethod) => {
      setAcquireMethod(method);
      setAuthorizeURL('');
      setCallbackURL('');
      setOauthStatus(null);
      resetBatchState();
      setSessionKeyMode('single');
      const type = resolveAccountType('claude_code', method);
      onAccountTypeChange?.(type);
    },
    [onAccountTypeChange, resolveAccountType, resetBatchState],
  );

  // 切换 Session Key 单个/批量
  const handleSessionKeyModeChange = useCallback(
    (m: 'single' | 'batch') => {
      setSessionKeyMode(m);
      resetBatchState();
      setOauthStatus(null);
    },
    [resetBatchState],
  );

  // ── OAuth 浏览器流程 ──
  const startOAuth = useCallback(async () => {
    if (!oauth) return;
    setOauthLoading(true);
    setOauthStatus({ type: 'info', text: '正在生成授权链接...' });
    try {
      const result = await oauth.start();
      setAuthorizeURL(result.authorizeURL);
      setCallbackURL('');
      setOauthStatus({ type: 'success', text: '授权链接已生成，请复制到浏览器完成授权。' });
    } catch (error) {
      setOauthStatus({ type: 'error', text: error instanceof Error ? error.message : '生成授权链接失败' });
    } finally {
      setOauthLoading(false);
    }
  }, [oauth]);

  const submitOAuthCallback = useCallback(async () => {
    if (!oauth || !callbackURL.trim()) return;
    setOauthLoading(true);
    setOauthStatus({ type: 'info', text: '正在完成授权交换...' });
    try {
      const result = await oauth.exchange(callbackURL.trim());
      onAccountTypeChange?.(result.accountType || 'oauth');
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) onSuggestedName?.(result.accountName);
      setOauthStatus({ type: 'success', text: '授权成功，凭证已自动填充。' });
    } catch (error) {
      setOauthStatus({ type: 'error', text: error instanceof Error ? error.message : '授权交换失败' });
    } finally {
      setOauthLoading(false);
    }
  }, [oauth, callbackURL, onAccountTypeChange, onChange, credentials, onSuggestedName]);

  const copyAuthorizeURL = useCallback(async () => {
    if (!authorizeURL) return;
    try {
      await navigator.clipboard.writeText(authorizeURL);
      setOauthStatus({ type: 'success', text: '授权链接已复制到剪贴板。' });
    } catch {
      setOauthStatus({ type: 'error', text: '复制失败，请手动复制。' });
    }
  }, [authorizeURL]);

  // ── Session Key 自动换 Token ──
  const exchangeSessionKey = useCallback(async () => {
    if (!oauth || !credentials.session_key?.trim()) return;
    setOauthLoading(true);
    setOauthStatus({ type: 'info', text: '正在通过 Session Key 获取 OAuth Token...' });
    try {
      const payload: Record<string, string> = { session_key: credentials.session_key };
      const result = await oauth.exchange(JSON.stringify(payload));
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) onSuggestedName?.(result.accountName);
      if (result.accountType) onAccountTypeChange?.(result.accountType);
      setOauthStatus({ type: 'success', text: 'OAuth Token 获取成功。' });
    } catch (error) {
      setOauthStatus({ type: 'error', text: error instanceof Error ? error.message : '获取 Token 失败' });
    } finally {
      setOauthLoading(false);
    }
  }, [oauth, credentials, onChange, onSuggestedName, onAccountTypeChange]);

  // ── 按钮样式 ──
  const primaryBtn = (disabled: boolean): React.CSSProperties => ({
    ...inputStyle,
    cursor: disabled ? 'not-allowed' : 'pointer',
    backgroundColor: cssVar('primary'),
    color: 'white',
    border: 'none',
    fontWeight: 500,
    width: 'auto',
    opacity: disabled ? 0.6 : 1,
  });

  const outlineBtn = (disabled: boolean): React.CSSProperties => ({
    ...inputStyle,
    cursor: disabled ? 'not-allowed' : 'pointer',
    backgroundColor: 'transparent',
    color: cssVar('primary'),
    border: `1px solid ${cssVar('primary')}`,
    width: 'auto',
    opacity: disabled ? 0.6 : 1,
  });

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>

      {/* ── 大类选择：Claude Code / Claude Console ── */}
      <div>
        <span style={labelStyle}>账号类型 *</span>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '0.75rem' }}>
          <div
            style={category === 'claude_code' ? cardActiveStyle : cardStyle}
            onClick={() => handleCategoryChange('claude_code')}
          >
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: cssVar('text') }}>Claude Code</div>
            <div style={descStyle}>OAuth / Session Key</div>
          </div>
          <div
            style={category === 'claude_console' ? cardActiveStyle : cardStyle}
            onClick={() => handleCategoryChange('claude_console')}
          >
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: cssVar('text') }}>Claude Console</div>
            <div style={descStyle}>API Key</div>
          </div>
        </div>
      </div>

      {/* ══════════════════════════════════════════════ */}
      {/* ── Claude Console（API Key）                 ── */}
      {/* ══════════════════════════════════════════════ */}
      {category === 'claude_console' && (
        <>
          <div>
            <label style={labelStyle}>
              API Key <span style={{ color: cssVar('danger') }}>*</span>
            </label>
            <input
              type="password"
              style={inputStyle}
              placeholder="sk-ant-api03-..."
              value={credentials.api_key ?? ''}
              onChange={(e) => updateField('api_key', e.target.value)}
            />
          </div>
          <div>
            <label style={labelStyle}>Base URL</label>
            <input
              type="text"
              style={inputStyle}
              placeholder="https://api.anthropic.com"
              value={credentials.base_url ?? ''}
              onChange={(e) => updateField('base_url', e.target.value)}
            />
            <div style={{ ...descStyle, marginTop: '0.375rem' }}>留空使用官方 Anthropic API</div>
          </div>
        </>
      )}

      {/* ══════════════════════════════════════════════ */}
      {/* ── Claude Code（OAuth 系列）                 ── */}
      {/* ══════════════════════════════════════════════ */}
      {category === 'claude_code' && (
        <>
          {/* ── 获取方式选择 ── */}
          <div>
            <span style={labelStyle}>获取方式</span>
            <div style={{ display: 'flex', gap: '0.5rem' }}>
              <span
                style={acquireMethod === 'session_key' ? pillActiveStyle : pillStyle}
                onClick={() => handleAcquireMethodChange('session_key')}
              >
                Session Key 自动获取
              </span>
              <span
                style={acquireMethod === 'browser_oauth' ? pillActiveStyle : pillStyle}
                onClick={() => handleAcquireMethodChange('browser_oauth')}
              >
                浏览器授权
              </span>
            </div>
          </div>

          {/* ── Session Key 获取方式 ── */}
          {acquireMethod === 'session_key' && (
            <div style={sectionStyle}>
              {/* 单个 / 批量 模式切换 */}
              <div style={{ display: 'flex', gap: '0.5rem', marginBottom: '0.75rem' }}>
                <span
                  style={sessionKeyMode === 'single' ? pillActiveStyle : pillStyle}
                  onClick={() => handleSessionKeyModeChange('single')}
                >
                  单个
                </span>
                <span
                  style={sessionKeyMode === 'batch' ? pillActiveStyle : pillStyle}
                  onClick={() => handleSessionKeyModeChange('batch')}
                >
                  批量
                </span>
              </div>

              {/* ── 单个模式 ── */}
              {sessionKeyMode === 'single' && (
                <>
                  <div>
                    <label style={labelStyle}>
                      Session Key <span style={{ color: cssVar('danger') }}>*</span>
                    </label>
                    <input
                      type="password"
                      style={inputStyle}
                      placeholder="sk-ant-sid01-..."
                      value={credentials.session_key ?? ''}
                      onChange={(e) => updateField('session_key', e.target.value)}
                    />
                    <div style={{ ...descStyle, marginTop: '0.375rem' }}>
                      在 claude.ai 的浏览器 Cookie 中获取 sessionKey 值
                    </div>
                  </div>

                  {oauth && (
                    <div style={{ marginTop: '0.75rem', display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                      <button
                        type="button"
                        onClick={exchangeSessionKey}
                        disabled={!credentials.session_key?.trim() || oauthLoading}
                        style={primaryBtn(!credentials.session_key?.trim() || oauthLoading)}
                      >
                        {oauthLoading ? '获取中...' : '获取 OAuth Token'}
                      </button>
                      <StatusMessage status={oauthStatus} />
                    </div>
                  )}
                </>
              )}

              {/* ── 批量模式 ── */}
              {sessionKeyMode === 'batch' && (
                <>
                  {batchPhase === 'input' && (
                    <>
                      <label style={labelStyle}>
                        Session Keys（每行一个，# 开头为注释）
                      </label>
                      <textarea
                        style={{
                          ...inputStyle,
                          minHeight: '140px',
                          resize: 'vertical',
                          fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
                        }}
                        placeholder={'sk-ant-sid01-...\nsk-ant-sid01-...'}
                        value={batchText}
                        onChange={(e) => setBatchText(e.target.value)}
                      />
                      <div style={{ ...descStyle, marginTop: '0.375rem' }}>
                        已识别 {parseSessionKeys(batchText).length} 个 Session Key · 成功换取后将自动创建账号
                      </div>
                      <div style={{ marginTop: '0.75rem', display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                        <button
                          type="button"
                          onClick={handleBatchImport}
                          disabled={parseSessionKeys(batchText).length === 0 || !oauth?.batchExchange || !onBatchImport}
                          style={primaryBtn(parseSessionKeys(batchText).length === 0 || !oauth?.batchExchange || !onBatchImport)}
                        >
                          批量导入
                        </button>
                        <StatusMessage status={oauthStatus} />
                      </div>
                    </>
                  )}

                  {batchPhase === 'exchanging' && (
                    <div style={{ padding: '1.5rem 0', textAlign: 'center' }}>
                      <div style={{ fontSize: '0.875rem', color: cssVar('textSecondary') }}>
                        正在批量换取 Token 并创建账号...
                      </div>
                    </div>
                  )}

                  {batchPhase === 'result' && (
                    <div>
                      <div style={{ display: 'flex', gap: '1rem', fontSize: '0.875rem', marginBottom: '0.5rem' }}>
                        <span style={{ color: cssVar('success') }}>
                          成功 {batchResults.filter((r) => r.status === 'ok').length}
                        </span>
                        {batchResults.filter((r) => r.status === 'failed').length > 0 && (
                          <span style={{ color: cssVar('danger') }}>
                            失败 {batchResults.filter((r) => r.status === 'failed').length}
                          </span>
                        )}
                        {batchImportedCount > 0 && (
                          <span style={{ color: cssVar('textSecondary') }}>
                            已导入 {batchImportedCount} 个账号
                          </span>
                        )}
                      </div>
                      <div
                        style={{
                          maxHeight: '200px',
                          overflowY: 'auto',
                          border: `1px solid ${cssVar('glassBorder')}`,
                          borderRadius: cssVar('radiusMd'),
                        }}
                      >
                        {batchResults.map((r, i) => (
                          <div
                            key={i}
                            style={{
                              display: 'flex',
                              alignItems: 'center',
                              gap: '0.5rem',
                              padding: '0.5rem 0.75rem',
                              fontSize: '0.75rem',
                              borderBottom:
                                i < batchResults.length - 1
                                  ? `1px solid ${cssVar('glassBorder')}`
                                  : undefined,
                            }}
                          >
                            <span
                              style={{
                                color: r.status === 'ok' ? cssVar('success') : cssVar('danger'),
                              }}
                            >
                              {r.status === 'ok' ? '✓' : '✗'}
                            </span>
                            <span style={{ color: cssVar('text'), flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                              {r.accountName || `SK #${i + 1}`}
                            </span>
                            {r.error && (
                              <span
                                style={{
                                  color: cssVar('danger'),
                                  maxWidth: '220px',
                                  overflow: 'hidden',
                                  textOverflow: 'ellipsis',
                                  whiteSpace: 'nowrap',
                                }}
                                title={r.error}
                              >
                                {r.error}
                              </span>
                            )}
                          </div>
                        ))}
                      </div>
                      <div style={{ marginTop: '0.75rem' }}>
                        <button
                          type="button"
                          onClick={resetBatchState}
                          style={outlineBtn(false)}
                        >
                          继续批量导入
                        </button>
                      </div>
                    </div>
                  )}
                </>
              )}
            </div>
          )}

          {/* ── 浏览器授权方式 ── */}
          {acquireMethod === 'browser_oauth' && oauth && (
            <div style={sectionStyle}>
              <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                生成授权链接 → 浏览器完成授权 → 粘贴回调 URL 完成交换
              </div>
              <div style={{ display: 'flex', gap: '0.75rem', marginBottom: '0.75rem', flexWrap: 'wrap' }}>
                <button type="button" onClick={startOAuth} disabled={oauthLoading} style={primaryBtn(oauthLoading)}>
                  生成授权链接
                </button>
                <button
                  type="button"
                  onClick={copyAuthorizeURL}
                  disabled={!authorizeURL || oauthLoading}
                  style={outlineBtn(!authorizeURL || oauthLoading)}
                >
                  复制授权链接
                </button>
              </div>
              {authorizeURL && (
                <>
                  <div style={{ marginBottom: '0.75rem' }}>
                    <label style={labelStyle}>授权链接</label>
                    <textarea
                      style={{ ...inputStyle, minHeight: '68px', resize: 'vertical' }}
                      readOnly
                      value={authorizeURL}
                    />
                  </div>
                  <div style={{ marginBottom: '0.75rem' }}>
                    <label style={labelStyle}>回调 URL</label>
                    <textarea
                      style={{ ...inputStyle, minHeight: '68px', resize: 'vertical' }}
                      placeholder="粘贴完整回调 URL"
                      value={callbackURL}
                      onChange={(e) => setCallbackURL(e.target.value)}
                    />
                  </div>
                  <button
                    type="button"
                    onClick={submitOAuthCallback}
                    disabled={!callbackURL.trim() || oauthLoading}
                    style={outlineBtn(!callbackURL.trim() || oauthLoading)}
                  >
                    完成授权交换
                  </button>
                </>
              )}
              <StatusMessage status={oauthStatus} />
            </div>
          )}

          {/* ── Token 字段（批量模式下隐藏） ── */}
          {!isBatchActive && (credentials.access_token ? (
            <>
              <div>
                <label style={labelStyle}>Access Token</label>
                <input type="password" style={{ ...inputStyle, opacity: 0.7 }} value={credentials.access_token ?? ''} readOnly />
              </div>
              <div>
                <label style={labelStyle}>Refresh Token</label>
                <input type="password" style={{ ...inputStyle, opacity: 0.7 }} value={credentials.refresh_token ?? ''} readOnly />
              </div>
              <div>
                <label style={labelStyle}>过期时间</label>
                <input type="text" style={{ ...inputStyle, opacity: 0.7 }} value={credentials.expires_at ?? ''} readOnly />
              </div>
            </>
          ) : (
            <div style={{ ...descStyle, color: cssVar('textSecondary'), padding: '0.5rem 0' }}>
              通过上方 Session Key 或浏览器授权获取 Token
            </div>
          ))}

          {/* ── Base URL（批量模式下隐藏） ── */}
          {!isBatchActive && (
            <div>
              <label style={labelStyle}>Base URL</label>
              <input
                type="text"
                style={inputStyle}
                placeholder="https://api.anthropic.com"
                value={credentials.base_url ?? ''}
                onChange={(e) => updateField('base_url', e.target.value)}
              />
              <div style={{ ...descStyle, marginTop: '0.375rem' }}>留空使用官方 Anthropic API</div>
            </div>
          )}
        </>
      )}
    </div>
  );
}
