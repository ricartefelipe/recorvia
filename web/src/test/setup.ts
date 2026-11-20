import '@testing-library/jest-dom/vitest';
import { notifications } from '@mantine/notifications';
import { cleanup, configure } from '@testing-library/react';
import { afterAll, afterEach, beforeAll } from 'vitest';
import '../lib/validation';
import { server } from './server';

// Sob carga (CI, typecheck e build em paralelo), a primeira renderização de um
// arquivo pode passar de 1 s, o limite padrão das buscas assíncronas.
configure({ asyncUtilTimeout: 3000 });

// jsdom não implementa estas APIs usadas pelo Mantine.
const { getComputedStyle } = window;
window.getComputedStyle = (element: Element) => getComputedStyle(element);
window.HTMLElement.prototype.scrollIntoView = function scrollIntoView() {};
Object.defineProperty(window, 'matchMedia', {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }),
});
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
window.ResizeObserver = ResizeObserverStub as unknown as typeof ResizeObserver;

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => {
  server.resetHandlers();
  cleanup();
  // O estado das notificações do Mantine é global: sem limpar, uma notificação
  // de um teste (que fecha sozinha só depois de alguns segundos) aparece no
  // teste seguinte e duplica textos procurados.
  notifications.clean();
  notifications.cleanQueue();
});
afterAll(() => server.close());
