import { notifications } from '@mantine/notifications';

type Politeness = 'polite' | 'assertive';
type Announcer = (message: string, politeness: Politeness) => void;

let announcer: Announcer | undefined;

/** Registrado pela região aria-live da aplicação. */
export function setAnnouncer(fn: Announcer | undefined): void {
  announcer = fn;
}

export function announce(message: string, politeness: Politeness = 'polite'): void {
  announcer?.(message, politeness);
}

export function notifySuccess(message: string, title = 'Pronto'): void {
  notifications.show({ color: 'green', title, message, autoClose: 5000 });
  announce(`${title}. ${message}`);
}

export function notifyInfo(message: string, title?: string): void {
  notifications.show({ color: 'blue', title, message, autoClose: 6000 });
  announce(title ? `${title}. ${message}` : message);
}

export function notifyError(message: string, options: { title?: string; requestId?: string } = {}): void {
  const title = options.title ?? 'Não foi possível concluir';
  const full = options.requestId ? `${message} Código da requisição: ${options.requestId}` : message;
  notifications.show({ color: 'red', title, message: full, autoClose: options.requestId ? false : 8000 });
  announce(`${title}. ${full}`, 'assertive');
}
