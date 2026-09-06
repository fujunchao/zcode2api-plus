/* 確認對話框封裝：以 hook 提供 confirm(options)，取代舊版 openConfirm */
import { useCallback, useState, type ReactNode } from 'react'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'

export interface ConfirmOptions {
  title: string
  description: ReactNode
  /* 確認文字，預設「確認」 */
  confirmText?: string
  /* 危險操作時用破壞色 */
  danger?: boolean
  onConfirm: () => Promise<void> | void
}

export function useConfirm() {
  const [options, setOptions] = useState<ConfirmOptions | null>(null)
  const [busy, setBusy] = useState(false)

  const confirm = useCallback((opts: ConfirmOptions) => setOptions(opts), [])

  async function run() {
    if (!options) return
    setBusy(true)
    try {
      await options.onConfirm()
      setOptions(null)
    } finally {
      setBusy(false)
    }
  }

  const element = (
    <AlertDialog
      open={!!options}
      onOpenChange={(open) => {
        if (!open && !busy) setOptions(null)
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{options?.title}</AlertDialogTitle>
          <AlertDialogDescription asChild>
            <div>{options?.description}</div>
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={busy}>取消</AlertDialogCancel>
          <AlertDialogAction
            className={options?.danger ? 'bg-destructive text-white hover:bg-destructive/90' : undefined}
            disabled={busy}
            onClick={(e) => {
              e.preventDefault()
              void run()
            }}
          >
            {options?.confirmText ?? '確認'}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )

  return { confirm, element }
}
