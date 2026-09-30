import { useRef, useState } from 'react'

export interface DraftLifecycle {
  beginSave(): number
  accept(version: number): void
}

// 未编辑时直接展示远端值；一旦编辑，整组草稿独立于缓存刷新。
export function useDraftGroup<T extends object>(remote: T | undefined, initial: T) {
  const [draft, setDraft] = useState<T>()
  const revision = useRef(0)
  return {
    value: draft ?? remote ?? initial,
    edit(patch: Partial<T>) {
      revision.current++
      setDraft((previous) => ({ ...(previous ?? remote ?? initial), ...patch }))
    },
    beginSave: () => revision.current,
    accept(version: number) {
      // 保存期间继续输入时，旧响应只能确认旧版本，不能清除新草稿。
      if (revision.current === version) setDraft(undefined)
    },
  }
}
