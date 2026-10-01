/**
 * useDirtyGuard — shared "unsaved changes" protection.
 *
 * Two capabilities:
 *  1. confirmDiscard() — opens the app-wide ConfirmDialog asking whether to
 *     leave with unsaved changes; resolves true when leaving is allowed.
 *     Used by every form modal's close path (✕ / 取消 / ESC).
 *  2. registerDirtySource(fn) — registers a dirty-check for page-leave
 *     protection. ProjectPipeline's router/beforeunload guards poll all
 *     registered sources via anyDirty(); sources auto-unregister on unmount.
 */
import { onScopeDispose } from 'vue'
import { useI18n } from 'vue-i18n'
import { useConfirm } from './useConfirm'

const dirtySources = new Set<() => boolean>()

/** True when ANY mounted component reports unsaved changes. */
export function anyDirty(): boolean {
  for (const isDirty of dirtySources) {
    try {
      if (isDirty()) return true
    } catch {
      /* a broken source must never block navigation */
    }
  }
  return false
}

export function useDirtyGuard() {
  const { t } = useI18n()
  const confirm = useConfirm()

  /** Ask whether to discard unsaved changes. Resolves true = leave/discard. */
  async function confirmDiscard(): Promise<boolean> {
    return confirm.open({
      title: t('misc.unsaved.title'),
      body: t('misc.unsaved.body'),
      confirmLabel: t('misc.unsaved.discard'),
      variant: 'danger',
    })
  }

  /**
   * Register a dirty source for page-leave protection.
   * Auto-unregisters when the calling component unmounts.
   */
  function registerDirtySource(isDirty: () => boolean): void {
    dirtySources.add(isDirty)
    onScopeDispose(() => { dirtySources.delete(isDirty) })
  }

  return { confirmDiscard, registerDirtySource }
}
