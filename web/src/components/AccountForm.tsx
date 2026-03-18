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

type AccountType = 'apikey' | 'oauth' | 'session_key';

function detectType(credentials: Record<string, string>): AccountType | '' {
  if (credentials.api_key) return 'apikey';
  if (credentials.session_key) return 'session_key';
  if (credentials.access_token) return 'oauth';
  return '';
}

export function AccountForm({
  credentials,
  onChange,
  mode,
  accountType: propType,
  onAccountTypeChange,
  onSuggestedName,
  oauth,
}: AccountFormProps) {
  const [localType, setLocalType] = useState<AccountType | ''>(
    (propType as AccountType) || (mode === 'edit' ? detectType(credentials) : ''),
  );
  const accountType = (propType as AccountType | undefined) ?? localType;

  // OAuth 浏览器授权流程状态
  const [authorizeURL, setAuthorizeURL] = useState('');
  const [callbackURL, setCallbackURL] = useState('');
  const [oauthLoading, setOauthLoading] = useState(false);
  const [oauthStatus, setOauthStatus] = useState<{ type: 'info' | 'success' | 'error'; text: string } | null>(null);

  // Session Key 交换状态
  const [exchangeLoading, setExchangeLoading] = useState(false);
  const [exchangeStatus, setExchangeStatus] = useState<{ type: 'info' | 'success' | 'error'; text: string } | null>(null);

  const updateField = useCallback(
    (key: string, value: string) => {
      onChange({ ...credentials, [key]: value });
    },
    [credentials, onChange],
  );

  const handleTypeChange = useCallback(
    (type: AccountType) => {
      setLocalType(type);
      onAccountTypeChange?.(type);
      setAuthorizeURL('');
      setCallbackURL('');
      setOauthStatus(null);
      setExchangeStatus(null);
      if (type === 'apikey') {
        onChange({ api_key: '', base_url: '' });
      } else if (type === 'oauth') {
        onChange({ access_token: '', refresh_token: '', expires_at: '' });
      } else if (type === 'session_key') {
        onChange({ session_key: '', access_token: '', refresh_token: '', expires_at: '' });
      }
    },
    [onChange, onAccountTypeChange],
  );

  // ── OAuth 浏览器流程：生成授权链接 ──
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
      setOauthStatus({
        type: 'error',
        text: error instanceof Error ? error.message : '生成授权链接失败',
      });
    } finally {
      setOauthLoading(false);
    }
  }, [oauth]);

  // ── OAuth 浏览器流程：提交回调 URL 完成交换 ──
  const submitOAuthCallback = useCallback(async () => {
    if (!oauth || !callbackURL.trim()) return;
    setOauthLoading(true);
    setOauthStatus({ type: 'info', text: '正在完成授权交换...' });
    try {
      const result = await oauth.exchange(callbackURL.trim());
      onAccountTypeChange?.(result.accountType || 'oauth');
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) {
        onSuggestedName?.(result.accountName);
      }
      setOauthStatus({ type: 'success', text: '授权成功，凭证已自动填充。' });
    } catch (error) {
      setOauthStatus({
        type: 'error',
        text: error instanceof Error ? error.message : '授权交换失败',
      });
    } finally {
      setOauthLoading(false);
    }
  }, [oauth, callbackURL, onAccountTypeChange, onChange, credentials, onSuggestedName]);

  // ── OAuth 浏览器流程：复制授权链接 ──
  const copyAuthorizeURL = useCallback(async () => {
    if (!authorizeURL) return;
    try {
      await navigator.clipboard.writeText(authorizeURL);
      setOauthStatus({ type: 'success', text: '授权链接已复制到剪贴板。' });
    } catch {
      setOauthStatus({ type: 'error', text: '复制失败，请手动复制授权链接。' });
    }
  }, [authorizeURL]);

  // ── Session Key 自动换 Token ──
  const exchangeSessionKey = useCallback(async () => {
    if (!oauth || !credentials.session_key?.trim()) return;
    setExchangeLoading(true);
    setExchangeStatus({ type: 'info', text: '正在通过 Session Key 获取 OAuth Token...' });
    try {
      const result = await oauth.exchange(JSON.stringify({ session_key: credentials.session_key }));
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) {
        onSuggestedName?.(result.accountName);
      }
      setExchangeStatus({ type: 'success', text: 'OAuth Token 获取成功，凭证已自动填充。' });
    } catch (error) {
      setExchangeStatus({
        type: 'error',
        text: error instanceof Error ? error.message : '获取 Token 失败',
      });
    } finally {
      setExchangeLoading(false);
    }
  }, [oauth, credentials, onChange, onSuggestedName]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <div>
        <span style={labelStyle}>账号类型 *</span>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: '0.75rem' }}>
          <div
            style={accountType === 'apikey' ? cardActiveStyle : cardStyle}
            onClick={() => handleTypeChange('apikey')}
          >
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: cssVar('text') }}>API Key</div>
            <div style={descStyle}>直接使用 Anthropic API Key</div>
          </div>
          <div
            style={accountType === 'oauth' ? cardActiveStyle : cardStyle}
            onClick={() => handleTypeChange('oauth')}
          >
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: cssVar('text') }}>OAuth 登录</div>
            <div style={descStyle}>通过浏览器授权登录</div>
          </div>
          <div
            style={accountType === 'session_key' ? cardActiveStyle : cardStyle}
            onClick={() => handleTypeChange('session_key')}
          >
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: cssVar('text') }}>Session Key</div>
            <div style={descStyle}>通过 claude.ai 自动获取 Token</div>
          </div>
        </div>
      </div>

      {/* ── API Key 模式 ── */}
      {accountType === 'apikey' && (
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
            <label style={labelStyle}>API 地址</label>
            <input
              type="text"
              style={inputStyle}
              placeholder="https://api.anthropic.com"
              value={credentials.base_url ?? ''}
              onChange={(e) => updateField('base_url', e.target.value)}
            />
            <div style={{ ...descStyle, marginTop: '0.375rem' }}>
              留空使用默认地址，支持自定义反向代理
            </div>
          </div>
        </>
      )}

      {/* ── OAuth 浏览器授权模式 ── */}
      {accountType === 'oauth' && (
        <>
          {oauth && (
            <div
              style={{
                border: `1px solid ${cssVar('glassBorder')}`,
                borderRadius: cssVar('radiusLg'),
                padding: '1rem',
                backgroundColor: cssVar('bgSurface'),
              }}
            >
              <div style={{ fontSize: '0.875rem', fontWeight: 600, color: cssVar('text'), marginBottom: '0.25rem' }}>
                OAuth 授权辅助
              </div>
              <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                先生成授权链接，在浏览器完成授权后，把完整回调 URL 粘贴回来完成交换。
              </div>
              <div style={{ display: 'flex', gap: '0.75rem', marginBottom: '0.75rem', flexWrap: 'wrap' }}>
                <button
                  type="button"
                  onClick={startOAuth}
                  disabled={oauthLoading}
                  style={{
                    ...inputStyle,
                    cursor: oauthLoading ? 'not-allowed' : 'pointer',
                    backgroundColor: cssVar('primary'),
                    color: 'white',
                    border: 'none',
                    fontWeight: 500,
                    width: 'auto',
                    opacity: oauthLoading ? 0.6 : 1,
                  }}
                >
                  生成授权链接
                </button>
                <button
                  type="button"
                  onClick={copyAuthorizeURL}
                  disabled={!authorizeURL || oauthLoading}
                  style={{
                    ...inputStyle,
                    cursor: !authorizeURL || oauthLoading ? 'not-allowed' : 'pointer',
                    backgroundColor: 'transparent',
                    color: cssVar('text'),
                    width: 'auto',
                    opacity: !authorizeURL || oauthLoading ? 0.6 : 1,
                  }}
                >
                  复制授权链接
                </button>
              </div>
              <div style={{ marginBottom: '0.75rem' }}>
                <label style={labelStyle}>授权链接</label>
                <textarea
                  style={{ ...inputStyle, minHeight: '76px', resize: 'vertical' }}
                  readOnly
                  placeholder='点击"生成授权链接"后，这里会显示完整授权地址'
                  value={authorizeURL}
                />
              </div>
              <div style={{ marginBottom: '0.75rem' }}>
                <label style={labelStyle}>回调 URL</label>
                <textarea
                  style={{ ...inputStyle, minHeight: '76px', resize: 'vertical' }}
                  placeholder="粘贴完整回调 URL，例如 https://platform.claude.com/oauth/code/callback?code=...&state=..."
                  value={callbackURL}
                  onChange={(e) => setCallbackURL(e.target.value)}
                />
              </div>
              <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                <button
                  type="button"
                  onClick={submitOAuthCallback}
                  disabled={!callbackURL.trim() || oauthLoading}
                  style={{
                    ...inputStyle,
                    cursor: !callbackURL.trim() || oauthLoading ? 'not-allowed' : 'pointer',
                    backgroundColor: 'transparent',
                    color: cssVar('primary'),
                    border: `1px solid ${cssVar('primary')}`,
                    width: 'auto',
                    opacity: !callbackURL.trim() || oauthLoading ? 0.6 : 1,
                  }}
                >
                  完成授权交换
                </button>
                {oauthStatus && (
                  <div
                    style={{
                      fontSize: '0.75rem',
                      color:
                        oauthStatus.type === 'error'
                          ? cssVar('danger')
                          : oauthStatus.type === 'success'
                            ? cssVar('success')
                            : cssVar('textSecondary'),
                    }}
                  >
                    {oauthStatus.text}
                  </div>
                )}
              </div>
            </div>
          )}

          <div>
            <label style={labelStyle}>
              Access Token {!oauth && <span style={{ color: cssVar('danger') }}>*</span>}
            </label>
            <input
              type="password"
              style={inputStyle}
              placeholder={oauth ? '授权后自动填充，或手动输入' : 'eyJhbG...'}
              value={credentials.access_token ?? ''}
              onChange={(e) => updateField('access_token', e.target.value)}
            />
          </div>
          <div>
            <label style={labelStyle}>Refresh Token</label>
            <input
              type="password"
              style={inputStyle}
              placeholder="授权后自动填充"
              value={credentials.refresh_token ?? ''}
              onChange={(e) => updateField('refresh_token', e.target.value)}
            />
          </div>
          <div>
            <label style={labelStyle}>过期时间</label>
            <input
              type="text"
              style={inputStyle}
              placeholder="授权后自动填充"
              value={credentials.expires_at ?? ''}
              onChange={(e) => updateField('expires_at', e.target.value)}
            />
          </div>
        </>
      )}

      {/* ── Session Key 模式 ── */}
      {accountType === 'session_key' && (
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
              在 claude.ai 的 Cookie 中获取 sessionKey 值
            </div>
          </div>

          {oauth && (
            <div
              style={{
                border: `1px solid ${cssVar('glassBorder')}`,
                borderRadius: cssVar('radiusLg'),
                padding: '1rem',
                backgroundColor: cssVar('bgSurface'),
              }}
            >
              <div style={{ fontSize: '0.875rem', fontWeight: 600, color: cssVar('text'), marginBottom: '0.25rem' }}>
                Token 获取
              </div>
              <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                填入 Session Key 后点击下方按钮，自动通过 claude.ai 获取 OAuth Token。
              </div>
              <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                <button
                  type="button"
                  onClick={exchangeSessionKey}
                  disabled={!credentials.session_key?.trim() || exchangeLoading}
                  style={{
                    ...inputStyle,
                    cursor: !credentials.session_key?.trim() || exchangeLoading ? 'not-allowed' : 'pointer',
                    backgroundColor: cssVar('primary'),
                    color: 'white',
                    border: 'none',
                    fontWeight: 500,
                    width: 'auto',
                    opacity: !credentials.session_key?.trim() || exchangeLoading ? 0.6 : 1,
                  }}
                >
                  {exchangeLoading ? '获取中...' : '获取 OAuth Token'}
                </button>
                {exchangeStatus && (
                  <div
                    style={{
                      fontSize: '0.75rem',
                      color:
                        exchangeStatus.type === 'error'
                          ? cssVar('danger')
                          : exchangeStatus.type === 'success'
                            ? cssVar('success')
                            : cssVar('textSecondary'),
                    }}
                  >
                    {exchangeStatus.text}
                  </div>
                )}
              </div>
            </div>
          )}

          {/* 自动填充的 Token 字段（只读展示） */}
          {credentials.access_token && (
            <>
              <div>
                <label style={labelStyle}>Access Token</label>
                <input
                  type="password"
                  style={{ ...inputStyle, opacity: 0.7 }}
                  value={credentials.access_token ?? ''}
                  readOnly
                />
              </div>
              <div>
                <label style={labelStyle}>Refresh Token</label>
                <input
                  type="password"
                  style={{ ...inputStyle, opacity: 0.7 }}
                  value={credentials.refresh_token ?? ''}
                  readOnly
                />
              </div>
              <div>
                <label style={labelStyle}>过期时间</label>
                <input
                  type="text"
                  style={{ ...inputStyle, opacity: 0.7 }}
                  value={credentials.expires_at ?? ''}
                  readOnly
                />
              </div>
            </>
          )}
        </>
      )}
    </div>
  );
}
