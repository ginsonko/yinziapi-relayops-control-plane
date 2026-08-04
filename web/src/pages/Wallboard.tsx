import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
	Activity,
	ArrowDownRight,
	ArrowLeft,
	ArrowUpRight,
	CalendarDays,
	ChevronDown,
	CircleDollarSign,
	Cloud,
	CloudFog,
	CloudLightning,
	CloudRain,
	CloudSnow,
	CloudSun,
	Clock3,
	Droplets,
	Database,
	Landmark,
	MapPin,
	MemoryStick,
	Moon,
	Network,
	RadioTower,
	RefreshCw,
	Search,
	ShieldCheck,
	Sun,
	ThermometerSun,
	TimerReset,
	TriangleAlert,
} from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type { Activity as ActivityRecord, BootstrapData, OverviewIntelligenceSnapshot, UnifiedMoney, UnifiedRatio } from '../api/types';
import type { PageId } from '../components/Shell';
import { Button, formatDateTime, formatMoney, IconButton, StatusBadge } from '../components/ui';

function money(metric?: UnifiedMoney | null) {
	if (!metric?.amount || !Number.isFinite(Number(metric.amount))) return '暂不可用';
	const symbol = metric.currency === 'CNY' ? '¥/$' : metric.currency === 'USD' ? '$' : metric.currency;
	return `${symbol} ${Number(metric.amount).toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

function ratio(metric?: UnifiedRatio | null) {
	if (!metric?.value || !Number.isFinite(Number(metric.value))) return '暂不可用';
	return `${(Number(metric.value) * 100).toFixed(2)}%`;
}

function newestTimestamp(...values: Array<string | undefined>) {
	return values.filter(Boolean).sort((left, right) => new Date(right!).getTime() - new Date(left!).getTime())[0] ?? '';
}

interface WeatherLocation {
	id: string;
	label: string;
	detail: string;
	latitude: number;
	longitude: number;
}

const weatherPresets: WeatherLocation[] = [
	{ id: 'yuci', label: '榆次区', detail: '山西省 晋中市', latitude: 37.617838, longitude: 112.846727 },
	{ id: 'taiyuan', label: '太原市', detail: '山西省', latitude: 37.86944, longitude: 112.56028 },
	{ id: 'beijing', label: '北京市', detail: '北京', latitude: 39.9042, longitude: 116.4074 },
	{ id: 'shanghai', label: '上海市', detail: '上海', latitude: 31.2304, longitude: 121.4737 },
	{ id: 'shenzhen', label: '深圳市', detail: '广东省', latitude: 22.5431, longitude: 114.0579 },
	{ id: 'hong-kong', label: '香港', detail: '中国', latitude: 22.3193, longitude: 114.1694 },
];

const defaultWeatherLocation = weatherPresets[0];
const weatherLocationStorageKey = 'relayops-wallboard-weather-location-v2';
const photonPlaceTypes = new Set(['state', 'city', 'county', 'district', 'borough', 'suburb', 'town', 'village', 'municipality']);

interface PhotonFeature {
	geometry?: { coordinates?: unknown[] };
	properties?: Record<string, unknown>;
}

interface WeatherSnapshot {
	temperature: number;
	apparentTemperature: number;
	humidity: number;
	code: number;
}

function validWeatherLocation(value: unknown): value is WeatherLocation {
	if (!value || typeof value !== 'object') return false;
	const item = value as Partial<WeatherLocation>;
	return typeof item.id === 'string' && typeof item.label === 'string' && item.label.trim().length > 0 && item.label.length <= 80 &&
		typeof item.detail === 'string' && item.detail.length <= 120 && Number.isFinite(item.latitude) && Number.isFinite(item.longitude) &&
		Number(item.latitude) >= -90 && Number(item.latitude) <= 90 && Number(item.longitude) >= -180 && Number(item.longitude) <= 180;
}

function storedWeatherLocation() {
	try {
		const parsed = JSON.parse(localStorage.getItem(weatherLocationStorageKey) ?? 'null') as unknown;
		return validWeatherLocation(parsed) ? parsed : defaultWeatherLocation;
	} catch {
		return defaultWeatherLocation;
	}
}

function photonWeatherLocation(feature: PhotonFeature): WeatherLocation | null {
	const properties = feature.properties ?? {};
	const coordinates = feature.geometry?.coordinates;
	const countryCode = String(properties.countrycode ?? '').toUpperCase();
	const placeType = String(properties.type ?? properties.osm_value ?? '').toLowerCase();
	const name = String(properties.name ?? '').trim();
	const longitude = Number(coordinates?.[0]);
	const latitude = Number(coordinates?.[1]);
	if (countryCode !== 'CN' || !photonPlaceTypes.has(placeType) || !name || !Number.isFinite(latitude) || !Number.isFinite(longitude)) return null;
	const context = [properties.state, properties.city, properties.county, properties.district]
		.map((value) => String(value ?? '').trim()).filter((value, index, values) => value && value !== name && values.indexOf(value) === index);
	return {
		id: `photon-${String(properties.osm_type ?? 'place')}-${String(properties.osm_id ?? `${longitude}-${latitude}`)}`,
		label: name.slice(0, 80), detail: context.join(' ').slice(0, 120), latitude, longitude,
	};
}

function formatBytes(value?: number) {
	if (!Number.isFinite(value) || Number(value) < 0) return '暂不可用';
	const bytes = Number(value);
	if (bytes < 1024) return `${Math.round(bytes)} B`;
	if (bytes < 1024 ** 2) return `${(bytes / 1024).toFixed(1)} KB`;
	if (bytes < 1024 ** 3) return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
	return `${(bytes / 1024 ** 3).toFixed(2)} GB`;
}

function formatUptime(value?: number) {
	if (!Number.isFinite(value) || Number(value) < 0) return '暂不可用';
	const minutes = Math.floor(Number(value) / 60);
	const days = Math.floor(minutes / 1440);
	const hours = Math.floor((minutes % 1440) / 60);
	const restMinutes = minutes % 60;
	return days > 0 ? `${days}天 ${hours}小时` : hours > 0 ? `${hours}小时 ${restMinutes}分` : `${restMinutes}分钟`;
}

function runtimeStatusLabel(status: string) {
	if (status === 'healthy') return '正常';
	if (status === 'warning') return '需关注';
	if (status === 'error') return '异常';
	return '待确认';
}

function weatherLabel(code?: number) {
	if (code === 0) return '晴';
	if (code === 1 || code === 2) return '多云';
	if (code === 3) return '阴';
	if (code === 45 || code === 48) return '有雾';
	if (code !== undefined && ((code >= 51 && code <= 67) || (code >= 80 && code <= 82))) return '有雨';
	if (code !== undefined && ((code >= 71 && code <= 77) || (code >= 85 && code <= 86))) return '有雪';
	if (code !== undefined && code >= 95) return '雷雨';
	return '天气平稳';
}

function WeatherGlyph({ code }: { code?: number }) {
	if (code === 0) return <Sun size={18} />;
	if (code === 1 || code === 2) return <CloudSun size={18} />;
	if (code === 3) return <Cloud size={18} />;
	if (code === 45 || code === 48) return <CloudFog size={18} />;
	if (code !== undefined && ((code >= 51 && code <= 67) || (code >= 80 && code <= 82))) return <CloudRain size={18} />;
	if (code !== undefined && ((code >= 71 && code <= 77) || (code >= 85 && code <= 86))) return <CloudSnow size={18} />;
	if (code !== undefined && code >= 95) return <CloudLightning size={18} />;
	return <CloudSun size={18} />;
}

function numericMoney(metric?: UnifiedMoney | null) {
	if (!metric?.amount || !Number.isFinite(Number(metric.amount))) return null;
	return Number(metric.amount);
}

function profitTone(metric?: UnifiedMoney | null) {
	const value = numericMoney(metric);
	if (value === null) return 'neutral';
	if (metric?.evidence === 'estimated') return 'warning';
	return value >= 0 ? 'positive' : 'negative';
}

function marginTone(metric?: UnifiedRatio | null) {
	if (!metric?.value || !Number.isFinite(Number(metric.value))) return 'neutral';
	const value = Number(metric.value) * 100;
	return value >= 15 ? 'positive' : value >= 5 ? 'warning' : 'negative';
}

export function WallboardPage({ data, onNavigate, onRefresh }: {
	data: BootstrapData;
	onNavigate: (page: PageId) => void;
	onRefresh: () => Promise<void>;
}) {
	const [intelligence, setIntelligence] = useState<OverviewIntelligenceSnapshot | null>(null);
	const [intelligenceError, setIntelligenceError] = useState('');
	const [refreshError, setRefreshError] = useState('');
	const [activities, setActivities] = useState<ActivityRecord[]>(() => data.activities.slice(0, 10));
	const [refreshing, setRefreshing] = useState(false);
	const [dark, setDark] = useState(() => localStorage.getItem('theme') === 'dark');
	const [clock, setClock] = useState(() => new Date());
	const [weatherLocation, setWeatherLocation] = useState<WeatherLocation>(storedWeatherLocation);
	const [weather, setWeather] = useState<WeatherSnapshot | null>(null);
	const [weatherUnavailable, setWeatherUnavailable] = useState(false);
	const [locationPickerOpen, setLocationPickerOpen] = useState(false);
	const [locationQuery, setLocationQuery] = useState('');
	const [locationResults, setLocationResults] = useState<WeatherLocation[]>([]);
	const [locationSearchError, setLocationSearchError] = useState('');
	const [locationSearching, setLocationSearching] = useState(false);
	const intelligenceInFlight = useRef<Promise<void> | null>(null);
	const activitiesInFlight = useRef<Promise<void> | null>(null);
	const refreshCycleInFlight = useRef<Promise<void> | null>(null);
	const locationSearchInFlight = useRef<AbortController | null>(null);

	const refreshIntelligence = useCallback(() => {
		if (intelligenceInFlight.current) return intelligenceInFlight.current;
		const pending = api.overviewIntelligence().then((result) => {
			setIntelligence(result.data);
			setIntelligenceError('');
		}).catch((error) => {
			setIntelligenceError(getErrorMessage(error));
		}).finally(() => {
			intelligenceInFlight.current = null;
		});
		intelligenceInFlight.current = pending;
		return pending;
	}, []);

	const refreshActivities = useCallback(() => {
		if (activitiesInFlight.current) return activitiesInFlight.current;
		const pending = api.listActivities({ page: 1, limit: 10, include_total: false }).then((result) => {
			setActivities(result.data.items.slice(0, 10));
		}).catch(() => undefined).finally(() => {
			activitiesInFlight.current = null;
		});
		activitiesInFlight.current = pending;
		return pending;
	}, []);

	const refreshCycle = useCallback(() => {
		if (refreshCycleInFlight.current) return refreshCycleInFlight.current;
		const pending = Promise.allSettled([onRefresh(), refreshIntelligence(), refreshActivities()]).then((results) => {
			const bootstrapResult = results[0];
			setRefreshError(bootstrapResult.status === 'rejected' ? getErrorMessage(bootstrapResult.reason) : '');
		}).finally(() => {
			refreshCycleInFlight.current = null;
		});
		refreshCycleInFlight.current = pending;
		return pending;
	}, [onRefresh, refreshActivities, refreshIntelligence]);

	useEffect(() => {
		setActivities(data.activities.slice(0, 10));
	}, [data.activities]);

	useEffect(() => {
		const interval = window.setInterval(() => setClock(new Date()), 30_000);
		return () => window.clearInterval(interval);
	}, []);

	useEffect(() => {
		let active = true;
		setWeather(null);
		setWeatherUnavailable(false);
		const refreshWeather = async () => {
			const controller = new AbortController();
			const timeout = window.setTimeout(() => controller.abort(), 6_000);
			try {
				const query = new URLSearchParams({
					latitude: String(weatherLocation.latitude),
					longitude: String(weatherLocation.longitude),
					current: 'temperature_2m,apparent_temperature,relative_humidity_2m,weather_code',
					timezone: 'auto',
				});
				const response = await fetch(`https://api.open-meteo.com/v1/forecast?${query}`, { signal: controller.signal, cache: 'no-store' });
				if (!response.ok) throw new Error(`weather HTTP ${response.status}`);
				const payload = await response.json() as { current?: Record<string, unknown> };
				const temperature = Number(payload.current?.temperature_2m);
				const apparentTemperature = Number(payload.current?.apparent_temperature);
				const humidity = Number(payload.current?.relative_humidity_2m);
				const code = Number(payload.current?.weather_code);
				if (![temperature, apparentTemperature, humidity, code].every(Number.isFinite)) throw new Error('weather response is incomplete');
				if (active) {
					setWeather({ temperature, apparentTemperature, humidity, code });
					setWeatherUnavailable(false);
				}
			} catch {
				if (active) setWeatherUnavailable(true);
			} finally {
				window.clearTimeout(timeout);
			}
		};
		void refreshWeather();
		const interval = window.setInterval(() => void refreshWeather(), 30 * 60_000);
		return () => {
			active = false;
			window.clearInterval(interval);
		};
	}, [weatherLocation]);

	useEffect(() => () => locationSearchInFlight.current?.abort(), []);

	const selectWeatherLocation = (location: WeatherLocation) => {
		if (!validWeatherLocation(location)) return;
		setWeatherLocation(location);
		localStorage.setItem(weatherLocationStorageKey, JSON.stringify(location));
		setLocationPickerOpen(false);
		setLocationSearchError('');
		setLocationResults([]);
		setLocationQuery('');
	};

	const searchWeatherLocations = async () => {
		const query = locationQuery.trim();
		if (query.length < 2) {
			setLocationSearchError('请输入至少两个字');
			return;
		}
		locationSearchInFlight.current?.abort();
		const controller = new AbortController();
		locationSearchInFlight.current = controller;
		setLocationSearching(true);
		setLocationSearchError('');
		try {
			const params = new URLSearchParams({ q: query, limit: '15' });
			const timeout = window.setTimeout(() => controller.abort(), 6_000);
			let response: Response;
			try {
				response = await fetch(`https://photon.komoot.io/api/?${params}`, { signal: controller.signal, cache: 'no-store' });
			} finally {
				window.clearTimeout(timeout);
			}
			if (!response.ok) throw new Error(`geocoding HTTP ${response.status}`);
			const payload = await response.json() as { features?: PhotonFeature[] };
			const unique = new Map<string, WeatherLocation>();
			for (const feature of payload.features ?? []) {
				const location = photonWeatherLocation(feature);
				if (location) {
					const key = `${location.label}|${location.detail}`;
					if (!unique.has(key)) unique.set(key, location);
				}
			}
			const results = [...unique.values()].sort((left, right) => Number(right.label === query) - Number(left.label === query)).slice(0, 8);
			setLocationResults(results);
			setLocationSearchError(results.length === 0 ? '没有找到匹配的国内地区' : '');
		} catch (error) {
			if (!controller.signal.aborted) setLocationSearchError('地区搜索暂不可用');
		} finally {
			if (locationSearchInFlight.current === controller) {
				locationSearchInFlight.current = null;
				setLocationSearching(false);
			}
		}
	};

	useEffect(() => {
		let active = true;
		const refresh = () => {
			if (active && document.visibilityState === 'visible') void refreshCycle();
		};
		refresh();
		const interval = window.setInterval(refresh, 60_000);
		const onFocus = () => refresh();
		const onVisibility = () => { if (document.visibilityState === 'visible') refresh(); };
		window.addEventListener('focus', onFocus);
		document.addEventListener('visibilitychange', onVisibility);
		return () => {
			active = false;
			window.clearInterval(interval);
			window.removeEventListener('focus', onFocus);
			document.removeEventListener('visibilitychange', onVisibility);
		};
	}, [refreshCycle]);

	const toggleTheme = () => {
		const next = !dark;
		setDark(next);
		document.documentElement.dataset.theme = next ? 'dark' : 'light';
		localStorage.setItem('theme', next ? 'dark' : 'light');
		window.dispatchEvent(new Event('relayops-theme-change'));
	};

	const refreshAll = async () => {
		if (refreshing) return;
		setRefreshing(true);
		try {
			await refreshCycle();
		} finally {
			setRefreshing(false);
		}
	};

	const supplierAlerts = data.suppliers.filter((item) => item.status === 'warning' || item.status === 'error');
	const credentialSupplierIds = new Set(data.suppliers.flatMap((supplier) => supplier.credentialId ? [supplier.credentialId] : []));
	const credentialAlerts = data.credentials.filter((item) => credentialSupplierIds.has(item.id) && item.status !== 'valid');
	const urgentBalances = data.balances.filter((item) => item.status === 'warning' || item.status === 'error');
	const operations = intelligence?.operations;
	const visibleWatch = data.watchItems.slice(0, Math.min(data.watchSettings.overview_limit, 8));
	const visibleActivities = activities.slice(0, 8);
	const attentionRows = useMemo(() => [
		...(data.metrics.pendingChanges > 0 ? [{ id: 'changes', title: `${data.metrics.pendingChanges} 项变更等待确认`, detail: '进入变更中心查看并处理', page: 'changes' as PageId, tone: 'warning' }] : []),
		...supplierAlerts.map((item) => ({ id: `supplier-${item.id}`, title: `${item.name} 采集异常`, detail: item.note || '查看最近一次失败分类', page: 'suppliers' as PageId, tone: item.status })),
		...urgentBalances.map((item) => ({ id: `balance-${item.id}`, title: `${item.supplier} 续航偏低`, detail: item.balance === null ? '余额待获取' : formatMoney(item.balance, item.currency), page: 'balances' as PageId, tone: item.status })),
		...credentialAlerts.map((item) => ({ id: `credential-${item.id}`, title: `${item.supplier} 登录态需处理`, detail: item.expiresAt ? `到期 ${formatDateTime(item.expiresAt)}` : '当前无法读取登录后数据', page: 'connectors' as PageId, tone: 'warning' })),
	], [credentialAlerts, data.metrics.pendingChanges, supplierAlerts, urgentBalances]);
	const generatedAt = newestTimestamp(operations?.generated_at, intelligence?.generated_at, data.generatedAt);
	const displayError = intelligenceError || refreshError;
	const timeText = clock.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
	const dateText = clock.toLocaleDateString('zh-CN', { year: 'numeric', month: 'long', day: 'numeric' });
	const weekdayText = clock.toLocaleDateString('zh-CN', { weekday: 'long' });
	const spendTone = numericMoney(operations?.today_user_spend) === null ? 'neutral' : 'negative';
	const userBalanceTone = numericMoney(operations?.user_balance) === null ? 'neutral' : 'blue';
	const upstreamBalanceTone = numericMoney(operations?.upstream_balance) === null ? 'neutral' : 'teal';
	const upstreamSpendTone = numericMoney(operations?.upstream_daily_spend) === null ? 'neutral' : 'warning';
	const runtimeTargets = data.runtime?.targets.filter((target) => target.enabled) ?? [];
	const runtimeTarget = [...runtimeTargets].sort((left, right) => {
		const priority: Record<string, number> = { error: 3, warning: 2, unknown: 1, healthy: 0 };
		return (priority[right.status] ?? 1) - (priority[left.status] ?? 1);
	})[0];
	const runtimeTargetTone = runtimeTarget?.status ?? 'unknown';
	const runtimeTargetText = runtimeTargets.length === 0 ? '未配置' : runtimeTargets.length === 1 ? `${runtimeTarget.name} ${runtimeStatusLabel(runtimeTarget.status)}` : `${runtimeTargets.filter((target) => target.status === 'healthy').length}/${runtimeTargets.length} 正常`;
	const runtimeTargetUpdatedAt = newestTimestamp(...runtimeTargets.map((target) => target.updatedAt));

	return <main className="wallboard">
		<header className="wallboard-topbar">
			<div className="wallboard-brand"><span><CircleDollarSign size={18} /></span><div><strong>RelayOps 副屏</strong><small data-testid="wallboard-updated-at"><Clock3 size={11} />数据更新于 {formatDateTime(generatedAt)}</small></div></div>
			<div className="wallboard-runtime">
				<span className={data.backgroundWorkersEnabled ? 'healthy' : 'warning'}>{data.backgroundWorkersEnabled ? '后台采集运行中' : '后台采集已停止'}</span>
				<span className={data.automationEnabled ? 'healthy' : 'warning'}>{data.automationEnabled ? '无人值守已开启' : '无人值守已关闭'}</span>
				{displayError && <span className="warning" title={displayError}>部分数据沿用上次成功值</span>}
			</div>
			<div className="wallboard-actions">
				<IconButton label="刷新副屏数据" onClick={() => void refreshAll()} disabled={refreshing}><RefreshCw size={17} className={refreshing ? 'spin' : ''} /></IconButton>
				<IconButton label={dark ? '切换浅色模式' : '切换深色模式'} onClick={toggleTheme}>{dark ? <Sun size={17} /> : <Moon size={17} />}</IconButton>
				<Button variant="secondary" onClick={() => onNavigate('overview')}><ArrowLeft size={16} />返回总览</Button>
			</div>
		</header>

		<section className="wallboard-utility-strip" aria-label="日期时间与天气">
			<div className="wallboard-clock"><Clock3 size={18} /><time dateTime={clock.toISOString()}>{timeText}</time></div>
			<div className="wallboard-date"><CalendarDays size={16} /><span>{dateText}</span><strong>{weekdayText}</strong></div>
			<div className={`wallboard-weather ${weatherUnavailable ? 'is-unavailable' : ''}`}>
				<span className="wallboard-weather-icon"><WeatherGlyph code={weather?.code} /></span>
				<div><strong>{weather ? `${Math.round(weather.temperature)}°` : weatherUnavailable ? '暂不可用' : '读取中'}</strong><span>{weather ? weatherLabel(weather.code) : '当前天气'}</span></div>
				{weather && <><span className="wallboard-weather-detail"><ThermometerSun size={13} />体感 {Math.round(weather.apparentTemperature)}°</span><span className="wallboard-weather-detail"><Droplets size={13} />湿度 {Math.round(weather.humidity)}%</span></>}
				<div className="wallboard-location-picker">
					<button type="button" className="wallboard-location-trigger" aria-label="选择天气地区" aria-expanded={locationPickerOpen} onClick={() => setLocationPickerOpen((open) => !open)}><MapPin size={13} /><span>{weatherLocation.label}</span><ChevronDown size={12} /></button>
					{locationPickerOpen && <form className="wallboard-location-popover" onSubmit={(event) => { event.preventDefault(); void searchWeatherLocations(); }}>
						<div className="wallboard-location-search"><Search size={13} /><input autoFocus aria-label="搜索国内天气地区" value={locationQuery} placeholder="搜索国内省、市、区、县" onChange={(event) => { setLocationQuery(event.target.value); setLocationSearchError(''); }} onKeyDown={(event) => { if (event.key === 'Escape') setLocationPickerOpen(false); }} /><IconButton type="submit" label="搜索国内地区" disabled={locationSearching}><Search size={14} className={locationSearching ? 'spin' : ''} /></IconButton></div>
						<div className="wallboard-location-results" role="listbox" aria-label={locationResults.length > 0 ? '地区搜索结果' : '常用地区'}>
							<span>{locationResults.length > 0 ? '搜索结果' : '常用地区'}</span>
							{(locationResults.length > 0 ? locationResults : weatherPresets).map((location) => <button type="button" role="option" aria-selected={location.id === weatherLocation.id} key={location.id} onClick={() => selectWeatherLocation(location)}><strong>{location.label}</strong><small>{location.detail}</small></button>)}
						</div>
						{locationSearchError && <small className="wallboard-location-error" role="status">{locationSearchError}</small>}
					</form>}
				</div>
			</div>
		</section>

		<div className="wallboard-grid">
			<div className="wallboard-side-stack">
				<section className="wall-panel wall-status">
					<header><div><Network size={15} /><strong>上游异常</strong></div><button onClick={() => onNavigate('suppliers')}>{data.metrics.healthySuppliers}/{data.metrics.supplierCount} 正常</button></header>
					<div className="wall-list">
						{supplierAlerts.slice(0, 5).map((supplier) => <button className={`wall-row-${supplier.status}`} key={supplier.id} onClick={() => onNavigate('suppliers')}><span className={`wall-dot ${supplier.status}`} /><div><strong>{supplier.name}</strong><small>{supplier.note || '最近采集异常'}</small></div><StatusBadge status={supplier.status} /></button>)}
						{supplierAlerts.length === 0 && <div className="wall-empty"><ShieldCheck size={18} /><span>全部上游最近采集正常</span></div>}
					</div>
				</section>

				<section className="wall-panel wall-attention">
					<header><div><TriangleAlert size={15} /><strong>需要关注</strong></div><span>{attentionRows.length} 项</span></header>
					<div className="wall-list compact">
						{attentionRows.slice(0, 6).map((item) => <button className={`wall-row-${item.tone}`} key={item.id} onClick={() => onNavigate(item.page)}><span className={`wall-dot ${item.tone}`} /><div><strong>{item.title}</strong><small>{item.detail}</small></div></button>)}
						{attentionRows.length === 0 && <div className="wall-empty"><ShieldCheck size={18} /><span>当前没有需要人工处理的事项</span></div>}
					</div>
				</section>
			</div>

			<section className="wall-panel wall-metrics">
				<header><div><CircleDollarSign size={15} /><strong>核心经营数据</strong></div><button onClick={() => onNavigate('intelligence')}>经营智能</button></header>
				<div className="wall-metric-grid">
					<div className={`wall-metric-primary wall-metric-tone-${spendTone}`} data-testid="wallboard-metric-today-user-spend"><span>今日用户消耗</span><strong>{money(operations?.today_user_spend)}</strong></div>
					<div className={`wall-metric-primary wall-metric-tone-${profitTone(operations?.today_gross_profit)}`}><span>{operations?.today_gross_profit.evidence === 'estimated' ? '今日毛利估算' : '今日现金毛利'}</span><strong>{money(operations?.today_gross_profit)}</strong></div>
					<div className={`wall-metric-tone-${userBalanceTone}`}><span>本站用户余额</span><strong>{money(operations?.user_balance)}</strong></div>
					<div className={`wall-metric-tone-${upstreamBalanceTone}`}><span>上游总余额</span><strong>{money(operations?.upstream_balance)}</strong></div>
					<div className={`wall-metric-tone-${upstreamSpendTone}`}><span>上游近 24h 消耗</span><strong>{money(operations?.upstream_daily_spend)}</strong></div>
					<div className={`wall-metric-tone-${marginTone(operations?.today_gross_margin)}`}><span>加权利润率</span><strong>{ratio(operations?.today_gross_margin)}</strong></div>
				</div>
			</section>

			<section className="wall-panel wall-prices">
				<header><div><Landmark size={15} /><strong>重点价格</strong></div><button onClick={() => onNavigate('watchlist')}>管理关注</button></header>
				<div className="wall-price-table">
					<div className="wall-price-head"><span>对象</span><span>上游</span><span>本站</span><span>24h</span><span>状态</span></div>
					{visibleWatch.map((item) => <button key={item.id} onClick={() => onNavigate('watchlist')}><span><strong>{item.displayName || item.group}</strong><small>{item.supplier}</small></span><code>{item.upstreamRate ?? '—'}</code><code>{item.localRate ?? (item.sourceMirror && !item.sourcePricing ? '仅镜像' : '—')}</code><span className={item.delta24h === null ? '' : item.delta24h > 0 ? 'negative' : item.delta24h < 0 ? 'positive' : ''}>{item.delta24h === null ? '样本不足' : <>{item.delta24h > 0 ? <ArrowUpRight size={11} /> : item.delta24h < 0 ? <ArrowDownRight size={11} /> : null}{Math.abs(item.delta24h).toFixed(2)}%</>}</span><StatusBadge status={item.status} /></button>)}
					{visibleWatch.length === 0 && <div className="wall-empty"><Landmark size={18} /><span>尚无重点关注对象</span></div>}
				</div>
			</section>

			<section className="wall-panel wall-activity">
				<header><div><Activity size={15} /><strong>最近活动</strong></div><button onClick={() => onNavigate('changes')}>查看详情</button></header>
				<div className="wall-timeline">
					{visibleActivities.map((item) => <button className={`wall-row-${item.status}`} key={item.id} onClick={() => onNavigate('changes')}><span className={`wall-dot ${item.status}`} /><div><strong>{item.title}</strong><small>{item.detail}</small></div><time>{new Date(item.at).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })}</time></button>)}
					{visibleActivities.length === 0 && <div className="wall-empty"><Activity size={18} /><span>暂无最近活动</span></div>}
				</div>
			</section>
		</div>
		{data.runtime && <section className="wallboard-footer-strip" aria-label="服务运行状态">
			<div><TimerReset size={15} /><span><small>服务运行</small><strong>{formatUptime(data.runtime.uptimeSeconds)}</strong></span></div>
			<div><Database size={15} /><span><small>数据库占用</small><strong>{formatBytes(data.runtime.databaseBytes)}</strong></span></div>
			<div><MemoryStick size={15} /><span><small>RelayOps 运行时内存</small><strong>{formatBytes(data.runtime.processMemoryBytes)}</strong></span><em>{data.runtime.goroutines} 协程</em></div>
			<div className={`wallboard-target-heartbeat ${runtimeTargetTone}`}><RadioTower size={15} /><span><small>NewAPI 已有心跳</small><strong>{runtimeTargetText}</strong></span>{runtimeTargetUpdatedAt && <em>{formatDateTime(runtimeTargetUpdatedAt)}</em>}</div>
		</section>}
	</main>;
}
