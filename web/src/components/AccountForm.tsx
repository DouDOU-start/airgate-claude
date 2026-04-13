import { useState, useCallback } from 'react';
import { cssVar } from '@airgate/theme';

/** 账号表单 Props（由核心 AccountsPage 注入） */
export interface AccountFormProps {
  credentials: Record<string, string>;
  onChange: (credentials: Record<string, string>) => void;
  mode: 'create' | 'edit';
  accountType?: string;
  onAccountTypeChange?: (type: string) => void;
  onSuggestedName?: (name: string) => void;
  oauth?: {
    start: () => Promise<{ authorizeURL: string; state: string }>;
    exchange: (callbackURL: string) => Promise<{
      accountType: string;
      accountName: string;
      credentials: Record<string, string>;
    }>;
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
type AccountType = 'apikey' | 'oauth' | 'setup_token' | 'session_key';

/** Claude Code 内部的获取方式 */
type AcquireMethod = 'session_key' | 'browser_oauth';

/** Claude Code 内部的 scope 类型 */
type ScopeType = 'full' | 'inference';

function detectCategory(accountType?: string, credentials?: Record<string, string>): UICategory {
  if (accountType === 'apikey') return 'claude_console';
  if (accountType === 'oauth' || accountType === 'setup_token' || accountType === 'session_key') return 'claude_code';
  if (credentials?.api_key) return 'claude_console';
  if (credentials?.session_key || credentials?.access_token) return 'claude_code';
  return 'claude_code';
}

function detectScopeType(accountType?: string): ScopeType {
  return accountType === 'setup_token' ? 'inference' : 'full';
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
  mode,
  accountType: propType,
  onAccountTypeChange,
  onSuggestedName,
  oauth,
}: AccountFormProps) {
  const [category, setCategory] = useState<UICategory>(
    detectCategory(propType, credentials),
  );
  const [scopeType, setScopeType] = useState<ScopeType>(detectScopeType(propType));
  const [acquireMethod, setAcquireMethod] = useState<AcquireMethod>(
    detectAcquireMethod(propType, credentials),
  );

  // OAuth 浏览器授权流程状态
  const [authorizeURL, setAuthorizeURL] = useState('');
  const [callbackURL, setCallbackURL] = useState('');
  const [oauthLoading, setOauthLoading] = useState(false);
  const [oauthStatus, setOauthStatus] = useState<{ type: 'info' | 'success' | 'error'; text: string } | null>(null);

  const updateField = useCallback(
    (key: string, value: string) => {
      onChange({ ...credentials, [key]: value });
    },
    [credentials, onChange],
  );

  // 根据 category + scope 推导后端 account type
  const resolveAccountType = useCallback(
    (cat: UICategory, scope: ScopeType, method: AcquireMethod): AccountType => {
      if (cat === 'claude_console') return 'apikey';
      if (scope === 'inference') return 'setup_token';
      if (method === 'session_key') return 'session_key';
      return 'oauth';
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
      if (cat === 'claude_console') {
        onAccountTypeChange?.('apikey');
        onChange({ api_key: '', base_url: '' });
      } else {
        const type = resolveAccountType(cat, scopeType, acquireMethod);
        onAccountTypeChange?.(type);
        onChange({ session_key: '', access_token: '', refresh_token: '', expires_at: '', base_url: '' });
      }
    },
    [onChange, onAccountTypeChange, resolveAccountType, scopeType, acquireMethod],
  );

  // 切换 scope（full / inference）
  const handleScopeChange = useCallback(
    (scope: ScopeType) => {
      setScopeType(scope);
      const type = resolveAccountType('claude_code', scope, acquireMethod);
      onAccountTypeChange?.(type);
    },
    [onAccountTypeChange, resolveAccountType, acquireMethod],
  );

  // 切换获取方式
  const handleAcquireMethodChange = useCallback(
    (method: AcquireMethod) => {
      setAcquireMethod(method);
      setAuthorizeURL('');
      setCallbackURL('');
      setOauthStatus(null);
      const type = resolveAccountType('claude_code', scopeType, method);
      onAccountTypeChange?.(type);
    },
    [onAccountTypeChange, resolveAccountType, scopeType],
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
      if (scopeType === 'inference') payload.scope = 'inference';
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
  }, [oauth, credentials, onChange, onSuggestedName, onAccountTypeChange, scopeType]);

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
            <div style={descStyle}>OAuth / Setup Token</div>
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
          {/* ── Token 类型选择 ── */}
          <div>
            <span style={labelStyle}>Token 类型</span>
            <div style={{ display: 'flex', gap: '0.5rem' }}>
              <span
                style={scopeType === 'full' ? pillActiveStyle : pillStyle}
                onClick={() => handleScopeChange('full')}
              >
                OAuth 令牌
              </span>
              <span
                style={scopeType === 'inference' ? pillActiveStyle : pillStyle}
                onClick={() => handleScopeChange('inference')}
              >
                Setup Token
              </span>
            </div>
            <div style={{ ...descStyle, marginTop: '0.375rem' }}>
              {scopeType === 'full'
                ? '完整 scope，支持 session/mcp 等全部功能'
                : '仅推理 scope（user:inference），有效期 1 年'}
            </div>
          </div>

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

          {/* ── Token 字段（自动填充或手动输入） ── */}
          {credentials.access_token ? (
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
            mode === 'edit' && (
              <>
                <div>
                  <label style={labelStyle}>Access Token</label>
                  <input
                    type="password"
                    style={inputStyle}
                    placeholder="手动输入或通过上方获取"
                    value={credentials.access_token ?? ''}
                    onChange={(e) => updateField('access_token', e.target.value)}
                  />
                </div>
                <div>
                  <label style={labelStyle}>Refresh Token</label>
                  <input
                    type="password"
                    style={inputStyle}
                    placeholder="手动输入"
                    value={credentials.refresh_token ?? ''}
                    onChange={(e) => updateField('refresh_token', e.target.value)}
                  />
                </div>
              </>
            )
          )}

          {/* ── Base URL ── */}
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
    </div>
  );
}
