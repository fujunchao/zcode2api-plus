import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { useConfirm } from '@/components/confirm'
import { useDraftGroup } from '@/hooks/use-draft-group'
import { api, errMsg } from '@/lib/api'
import { defaults } from '@/components/health/labels'
import { HealthSummaryCard } from '@/components/health/summary-card'
import { HealthDetailSheet } from '@/components/health/detail-sheet'
import { HealthSettingsSheet } from '@/components/health/settings-form'
import { EgressGroupsSheet } from '@/components/health/egress-groups'
import type { ModelProbeSettings, ModelProbeSummary, ProxyProfile } from '@/lib/types'

/**
 * 健康巡检入口：摘要行常驻，详情与配置各自走抽屉。
 *
 * 草稿（useDraftGroup）刻意留在本容器层——放进抽屉内的子组件会在关闭时丢失未保存的编辑，
 * 也会让保存期间的并发输入被旧响应覆盖（见 hooks/use-draft-group.ts）。
 */
export function HealthPanel({ profiles }: { profiles: ProxyProfile[] }) {
  const qc = useQueryClient()
  const { confirm, element } = useConfirm()
  const [detail, setDetail] = useState(false)
  const [settings, setSettings] = useState(false)
  const [groups, setGroups] = useState(false)
  const [saving, setSaving] = useState(false)
  const [triggering, setTriggering] = useState('')
  const [historyTarget, setHistoryTarget] = useState('')
  const { data, error } = useQuery({ queryKey: ['model-probe'], queryFn: () => api<ModelProbeSummary>('GET', '/proxies/model-probe'), refetchInterval: 10_000 })
  const draft = useDraftGroup(data?.settings, defaults)
  const value = draft.value

  async function save() {
    if (!data?.settings) return
    const version = draft.beginSave()
    setSaving(true)
    try {
      const latest = qc.getQueryData<ModelProbeSummary>(['model-probe'])?.settings ?? data.settings
      const saved = await api<ModelProbeSettings>('PUT', '/proxies/model-probe/settings', { ...value, target_mode: 'auto', account_mode: 'auto', target_proxy_ids: [], account_ids: [], reference_proxy_ids: [], revision: latest.revision })
      qc.setQueryData<ModelProbeSummary>(['model-probe'], (old) => old ? { ...old, settings: saved } : old)
      draft.accept(version)
      void qc.invalidateQueries({ queryKey: ['accounts'] })
      void qc.invalidateQueries({ queryKey: ['model-probe'] })
      void qc.invalidateQueries({ queryKey: ['network-health'] })
      toast.success('健康巡检配置已保存，后续轮次使用新间隔')
      setSettings(false)
      setGroups(false)
    } catch (e) { toast.error('保存失败：' + errMsg(e)) }
    finally { setSaving(false) }
  }

  function confirmSave() {
    if (!data?.settings) return
    const releasesRoles = data.settings.account_ids.length > 0
    const destructive = (value.enabled || value.network_enabled) && value.mode === 'auto_retire' && ((!data.settings.enabled && !data.settings.network_enabled) || data.settings.mode !== 'auto_retire')
    if (releasesRoles || destructive || (value.allow_direct_control && !data.settings.allow_direct_control)) {
      const description = releasesRoles
        ? '切换为自动择优会解除现有探测专用角色，原专用账号回到普通业务选号；之后每次诊断按近期成功率从合格账号中自动挑选。自动模式会处理经过二次复核的网络故障，或严格对照达标的模型 405；固定出口账号仍等待，不擅自换线。'
        : '全部启用目标将自动巡检，账号按近期可靠性自动选择，继续留在业务池。自动处置只处理经网络复核或严格模型对照达标的问题；直连参考仅在你明确允许时使用，固定出口账号不被借用。'
      confirm({ title: '确认健康巡检策略', description, confirmText: '确认并保存', danger: !!destructive, onConfirm: save })
    } else void save()
  }

  async function check(id: string) {
    setTriggering(id)
    try {
      await api('POST', '/proxies/' + encodeURIComponent(id) + '/model-probe')
      void qc.invalidateQueries({ queryKey: ['model-probe'] })
      void qc.invalidateQueries({ queryKey: ['model-probe-history', id] })
      toast.success('已加入模型检查队列，重复任务会合并')
    } catch (e) { toast.error('无法开始检查：' + errMsg(e)) }
    finally { setTriggering('') }
  }

  function openHistory(id: string) {
    setHistoryTarget(id)
    if (id) setDetail(true)
  }

  return <>
    <HealthSummaryCard data={data} error={error} loading={!data && !error} onDetail={() => setDetail(true)} onSettings={() => setSettings(true)} />
    {data && <HealthDetailSheet open={detail} onOpenChange={setDetail} data={data} profiles={profiles} triggering={triggering} onCheck={check} historyTarget={historyTarget} onHistory={openHistory} />}
    {data && <HealthSettingsSheet open={settings} onOpenChange={setSettings} data={data} profiles={profiles} value={value} edit={draft.edit} saving={saving} onSubmit={confirmSave} onTopology={() => { setSettings(false); setGroups(true) }} />}
    <EgressGroupsSheet open={groups} onOpenChange={setGroups} value={value} edit={draft.edit} profiles={profiles} saving={saving} onSave={confirmSave} />
    {element}
  </>
}
