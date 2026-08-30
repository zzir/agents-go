type ToastType = 'error' | 'warning' | 'success' | 'info';

interface ToastItem {
  msg: string;
  type: ToastType;
}

type ToastListener = ((item: ToastItem) => void) | null;

let _listener: ToastListener = null;

export function onToast(fn: ToastListener): void { _listener = fn; }

function emit(msg: string, type: ToastType): void {
  // The on-screen toast is clamped to one line; log the full text so nothing is
  // lost when it is truncated.
  if (type === 'error') console.error('[toast]', msg);
  else if (type === 'warning') console.warn('[toast]', msg);
  else console.info('[toast]', msg);
  if (_listener) _listener({ msg, type });
}

export const toast = {
  error:   (msg: string): void => emit(msg, 'error'),
  warn:    (msg: string): void => emit(msg, 'warning'),
  success: (msg: string): void => emit(msg, 'success'),
  info:    (msg: string): void => emit(msg, 'info'),
};
