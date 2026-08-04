import { useEffect, useState, type ReactNode } from 'react';
import {
  Activity,
	AlertTriangle,
  Blocks,
	BrainCircuit,
  ChevronLeft,
  ChevronRight,
  CircleDollarSign,
  Gauge,
	GitCompareArrows,
  KeyRound,
  Landmark,
  Menu,
  Moon,
  PackagePlus,
  RefreshCw,
	Replace,
  Sun,
  Target,
	Workflow,
  X,
} from 'lucide-react';
import type { ApiContractMismatchDetail } from '../api/client';
import { IconButton } from './ui';

export type PageId = 'overview' | 'wallboard' | 'suppliers' | 'watchlist' | 'pricing' | 'mirror' | 'mapping-drafts' | 'balances' | 'discoveries' | 'intelligence' | 'changes' | 'connectors' | 'config-portability';

const navItems: Array<{ id: PageId; label: string; icon: typeof Gauge }> = [
  { id: 'overview', label: '总览', icon: Gauge },
  { id: 'suppliers', label: '上游站点', icon: Blocks },
  { id: 'watchlist', label: '重点关注', icon: Target },
  { id: 'pricing', label: '价格映射', icon: CircleDollarSign },
	{ id: 'mirror', label: '分组镜像', icon: Workflow },
	{ id: 'mapping-drafts', label: '映射草案', icon: GitCompareArrows },
  { id: 'balances', label: '余额续航', icon: Landmark },
  { id: 'discoveries', label: '新品发现', icon: PackagePlus },
	{ id: 'intelligence', label: '经营智能', icon: BrainCircuit },
  { id: 'changes', label: '变更中心', icon: Activity },
  { id: 'connectors', label: '站点接入', icon: KeyRound },
	{ id: 'config-portability', label: '配置迁移', icon: Replace },
];

export function Shell({ page, onPageChange, syncing, syncProgress, onSync, pendingChanges, contractMismatch, onReloadVersion, children }: { page: PageId; onPageChange: (page: PageId) => void; syncing: boolean; syncProgress?: string; onSync: () => void; pendingChanges: number; contractMismatch?: ApiContractMismatchDetail | null; onReloadVersion: () => void; children: ReactNode }) {
  const [collapsed, setCollapsed] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [dark, setDark] = useState(() => localStorage.getItem('theme') === 'dark');

  useEffect(() => {
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
    localStorage.setItem('theme', dark ? 'dark' : 'light');
  }, [dark]);
	useEffect(() => {
		const syncTheme = () => setDark(localStorage.getItem('theme') === 'dark');
		window.addEventListener('relayops-theme-change', syncTheme);
		return () => window.removeEventListener('relayops-theme-change', syncTheme);
	}, []);

  const changePage = (next: PageId) => {
    onPageChange(next);
    setMobileOpen(false);
    window.scrollTo({ top: 0, behavior: 'smooth' });
  };

	if (page === 'wallboard') {
		return <div className="wallboard-shell">{children}</div>;
	}

  return (
    <div className={`app-shell ${collapsed ? 'sidebar-collapsed' : ''}`}>
      {mobileOpen && <button className="mobile-scrim" aria-label="关闭导航" onClick={() => setMobileOpen(false)} />}
      <aside className={`sidebar ${mobileOpen ? 'mobile-open' : ''}`}>
        <div className="brand">
          <span className="brand-mark"><CircleDollarSign size={22} /></span>
          <div className="brand-text"><strong>Procura</strong><small>智能采购控制台</small></div>
          <IconButton label="关闭导航" className="mobile-close" onClick={() => setMobileOpen(false)}><X size={19} /></IconButton>
        </div>
        <nav className="nav-list" aria-label="主导航">
          {navItems.map((item) => {
            const Icon = item.icon;
            return (
              <button key={item.id} className={`nav-item ${page === item.id ? 'active' : ''}`} onClick={() => changePage(item.id)} title={collapsed ? item.label : undefined}>
                <Icon size={19} />
                <span>{item.label}</span>
                {item.id === 'changes' && pendingChanges > 0 && <b>{pendingChanges}</b>}
              </button>
            );
          })}
        </nav>
        <div className="sidebar-footer">
          <button className="collapse-button" onClick={() => setCollapsed(!collapsed)} aria-label={collapsed ? '展开侧栏' : '收起侧栏'}>
            {collapsed ? <ChevronRight size={18} /> : <><ChevronLeft size={18} /><span>收起导航</span></>}
          </button>
        </div>
      </aside>
      <div className={`content-shell ${contractMismatch ? 'has-contract-mismatch' : ''}`}>
        <header className="topbar">
          <div className="topbar-left">
            <IconButton label="打开导航" className="mobile-menu" onClick={() => setMobileOpen(true)}><Menu size={20} /></IconButton>
          </div>
          <div className="topbar-actions">
            <button className="sync-button" onClick={onSync} disabled={syncing || !!contractMismatch} title="立即刷新全部已启用上游；不会改变无人值守自动改价开关"><RefreshCw size={16} className={syncing ? 'spin' : ''} /><span>{syncing ? `全局刷新 ${syncProgress || ''}`.trim() : '全局刷新'}</span></button>
            <IconButton label={dark ? '切换浅色模式' : '切换深色模式'} onClick={() => setDark(!dark)}>{dark ? <Sun size={18} /> : <Moon size={18} />}</IconButton>
            <div className="avatar" title="管理员">银</div>
          </div>
        </header>
		{contractMismatch && <section className="version-mismatch" role="alert"><AlertTriangle size={20} /><div><strong>控制台已更新，当前页面已停止操作</strong><span>页面 {contractMismatch.expected}，服务端 {contractMismatch.actual}。重新加载后会回到当前功能页，管理令牌仍保留在本标签页。</span></div><button onClick={onReloadVersion}><RefreshCw size={16} />重新加载新版本</button></section>}
        <main className="main-content">{children}</main>
      </div>
    </div>
  );
}
