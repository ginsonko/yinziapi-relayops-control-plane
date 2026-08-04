import type { ButtonHTMLAttributes, ReactNode } from 'react';
import { AlertTriangle, CheckCircle2, CircleHelp, LoaderCircle, XCircle } from 'lucide-react';
import type { AuthStatus, HealthStatus } from '../api/types';

export function Button({ className = '', variant = 'primary', ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: 'primary' | 'secondary' | 'danger' | 'ghost' }) {
  return <button className={`button button-${variant} ${className}`} {...props} />;
}

export function IconButton({ label, className = '', ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { label: string }) {
  return <button className={`icon-button ${className}`} aria-label={label} title={label} {...props} />;
}

const healthLabels: Record<HealthStatus, string> = {
  healthy: '正常',
  warning: '需关注',
  error: '异常',
  unknown: '待确认',
};

const authLabels: Record<AuthStatus, string> = {
  valid: '凭据有效',
  expiring: '即将到期',
  missing: '需要登录',
	pending: '待验证',
	invalid: '授权失效',
};

export function StatusBadge({ status, label }: { status: HealthStatus | AuthStatus; label?: string }) {
  const healthStatus = status === 'valid' ? 'healthy' : status === 'expiring' || status === 'pending' ? 'warning' : status === 'missing' || status === 'invalid' ? 'error' : status;
  const Icon = healthStatus === 'healthy' ? CheckCircle2 : healthStatus === 'warning' ? AlertTriangle : healthStatus === 'error' ? XCircle : CircleHelp;
  return (
    <span className={`status-badge status-${healthStatus}`}>
      <Icon size={13} />
      {label ?? (status in healthLabels ? healthLabels[status as HealthStatus] : authLabels[status as AuthStatus])}
    </span>
  );
}

export function Toggle({ checked, onChange, label, disabled = false }: { checked: boolean; onChange?: (value: boolean) => void; label: string; disabled?: boolean }) {
  return (
    <button type="button" className={`toggle ${checked ? 'is-on' : ''}`} role="switch" aria-checked={checked} aria-label={label} title={label} disabled={disabled} onClick={() => onChange?.(!checked)}>
      <span />
    </button>
  );
}

export function PageHeader({ eyebrow, title, description, actions }: { eyebrow?: string; title: string; description: string; actions?: ReactNode }) {
  return (
    <header className="page-header">
      <div>
        {eyebrow && <div className="eyebrow">{eyebrow}</div>}
        <h1>{title}</h1>
        <p>{description}</p>
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </header>
  );
}

export function Section({ title, description, action, children, className = '' }: { title: string; description?: string; action?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={`section ${className}`}>
      <div className="section-heading">
        <div>
          <h2>{title}</h2>
          {description && <p>{description}</p>}
        </div>
        {action}
      </div>
      {children}
    </section>
  );
}

export function EmptyState({ icon, title, description, action }: { icon: ReactNode; title: string; description: string; action?: ReactNode }) {
  return (
    <div className="empty-state">
      <span className="empty-icon">{icon}</span>
      <h3>{title}</h3>
      <p>{description}</p>
      {action}
    </div>
  );
}

export function LoadingState() {
  return (
    <div className="loading-state">
      <LoaderCircle className="spin" size={24} />
      <span>正在读取控制台数据…</span>
    </div>
  );
}

export function ErrorState({ message, onRetry, children }: { message: string; onRetry: () => void; children?: ReactNode }) {
  return (
    <div className="error-state">
      <XCircle size={24} />
      <div><strong>数据读取失败</strong><span>{message}</span></div>
      <Button variant="secondary" onClick={onRetry}>重新加载</Button>
      {children}
    </div>
  );
}

export function Modal({ open, title, description, children, footer, onClose }: { open: boolean; title: string; description?: string; children: ReactNode; footer?: ReactNode; onClose: () => void }) {
  if (!open) return null;
  return (
    <div className="modal-backdrop" role="presentation" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <div className="modal" role="dialog" aria-modal="true" aria-labelledby="modal-title">
        <div className="modal-header">
          <div><h2 id="modal-title">{title}</h2>{description && <p>{description}</p>}</div>
          <IconButton label="关闭" onClick={onClose}><XCircle size={19} /></IconButton>
        </div>
        <div className="modal-body">{children}</div>
        {footer && <div className="modal-footer">{footer}</div>}
      </div>
    </div>
  );
}

export function MiniBars({ values, tone = 'teal' }: { values: number[]; tone?: 'teal' | 'amber' | 'red' }) {
  const max = Math.max(...values, 1);
  return (
    <div className={`mini-bars bars-${tone}`} aria-label={`近七日数据：${values.join('、')}`}>
      {values.map((value, index) => <span key={index} style={{ height: `${Math.max(12, value / max * 100)}%` }} />)}
    </div>
  );
}

export function formatMoney(value: number | null, currency = 'CNY') {
	if (value === null) return '—';
	const normalized = currency.trim().toUpperCase() || 'CNY';
	if (normalized === 'CNY/USD') {
		return `¥/$ ${value.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
	}
	try {
		return new Intl.NumberFormat('zh-CN', { style: 'currency', currency: normalized, minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(value);
	} catch {
		return `${value.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })} ${normalized}`;
	}
}

export function formatDateTime(value?: string | null, fallback = '未记录') {
	if (!value) return fallback;
	const parsed = new Date(value);
	return Number.isNaN(parsed.getTime()) ? fallback : parsed.toLocaleString('zh-CN', { hour12: false });
}

export function formatRunway(hours: number | null | undefined) {
	if (hours === null || hours === undefined || !Number.isFinite(hours)) return '待采样';
	if (hours >= 720) return '30 天以上';
	if (hours >= 48) return `${(hours / 24).toLocaleString('zh-CN', { minimumFractionDigits: 1, maximumFractionDigits: 1 })} 天`;
	if (hours >= 1) return `${hours.toLocaleString('zh-CN', { minimumFractionDigits: 1, maximumFractionDigits: 1 })} 小时`;
	if (hours <= 0) return '不足 1 小时';
	return `${Math.max(1, Math.round(hours * 60)).toLocaleString('zh-CN')} 分钟`;
}

export function modeLabel(mode: string) {
  return ({ add: '固定加值', multiply: '倍率乘算', target_margin: '目标毛利', markup: '成本加成', subsidy_rate: '公益补贴', fixed: '固定价格' } as Record<string, string>)[mode] ?? mode;
}
