/* 後台導覽定義：沿用舊版 header.js 的七項導覽與分組（總覽／營運／設定） */
import {
  ChartColumn,
  ChartLine,
  LayoutDashboard,
  Settings,
  ShieldCheck,
  SlidersHorizontal,
  Users,
  type LucideIcon,
} from 'lucide-react'

export interface NavItem {
  href: string
  label: string
  group: '總覽' | '營運' | '設定'
  icon: LucideIcon
}

export const NAV_ITEMS: NavItem[] = [
  { href: '/admin/dashboard', label: '儀表板', group: '總覽', icon: LayoutDashboard },
  { href: '/admin/usage', label: '用量分析', group: '總覽', icon: ChartLine },
  { href: '/admin/monitor', label: '運維監控', group: '營運', icon: ChartColumn },
  { href: '/admin/accounts', label: '帳號池', group: '營運', icon: Users },
  { href: '/admin/proxies', label: '代理設定', group: '營運', icon: SlidersHorizontal },
  { href: '/admin/captcha', label: '驗證中心', group: '營運', icon: ShieldCheck },
  { href: '/admin/settings', label: '系統設定', group: '設定', icon: Settings },
]

export const NAV_GROUPS = ['總覽', '營運', '設定'] as const
