import { setupServer } from 'msw/node';

/** Servidor MSW dos testes; cada teste registra seus handlers com server.use(). */
export const server = setupServer();
