import type { components } from './schema';

export type Problem = components['schemas']['Problem'];
export interface FieldError {
  field: string;
  message: string;
}

/** Erro HTTP interpretado a partir de application/problem+json (RFC 9457). */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly title?: string;
  readonly detail?: string;
  readonly requestId?: string;
  readonly fieldErrors: FieldError[];

  constructor(init: {
    status: number;
    code: string;
    title?: string;
    detail?: string;
    requestId?: string;
    fieldErrors?: FieldError[];
  }) {
    super(init.detail || init.title || `Erro HTTP ${init.status}`);
    this.name = 'ApiError';
    this.status = init.status;
    this.code = init.code;
    this.title = init.title;
    this.detail = init.detail;
    this.requestId = init.requestId;
    this.fieldErrors = init.fieldErrors ?? [];
  }

  get isServerError(): boolean {
    return this.status >= 500;
  }
}

/** Falha de rede (sem resposta do servidor). Reenvios reutilizam a chave de idempotência. */
export class NetworkError extends Error {
  constructor(cause?: unknown) {
    super('Não foi possível falar com o servidor. Verifique sua conexão e tente novamente.');
    this.name = 'NetworkError';
    this.cause = cause;
  }
}

export function isApiError(error: unknown, status?: number): error is ApiError {
  return error instanceof ApiError && (status === undefined || error.status === status);
}

export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
export type QueryValue = string | number | boolean | null | undefined;

export interface RequestOptions {
  method?: HttpMethod;
  query?: Record<string, QueryValue>;
  body?: unknown;
  /** Versão do recurso para concorrência otimista: enviada como If-Match: "<versão>". */
  ifMatch?: number;
  idempotencyKey?: string;
  signal?: AbortSignal;
  responseType?: 'json' | 'text' | 'blob';
  /** false omite X-Recorvia-Tenant (endpoints da própria sessão). */
  tenant?: boolean;
}

export interface HttpClientConfig {
  /** Origem das chamadas. Padrão: window.location.origin. */
  baseUrl?: string;
  getCsrfToken: () => string | undefined;
  getTenantId?: () => string | undefined;
  /** Sessão ausente ou expirada. */
  onUnauthorized?: (error: ApiError) => void;
  /** 409 tenant_mismatch: a sessão mudou de organização em outra aba. */
  onTenantMismatch?: (error: ApiError) => void;
  fetchImpl?: typeof fetch;
}

export interface HttpClient {
  request<T>(path: string, options?: RequestOptions): Promise<T>;
  get<T>(path: string, options?: Omit<RequestOptions, 'method' | 'body'>): Promise<T>;
  post<T>(path: string, body?: unknown, options?: Omit<RequestOptions, 'method' | 'body'>): Promise<T>;
  put<T>(path: string, body?: unknown, options?: Omit<RequestOptions, 'method' | 'body'>): Promise<T>;
  patch<T>(path: string, body?: unknown, options?: Omit<RequestOptions, 'method' | 'body'>): Promise<T>;
  delete<T = void>(path: string, options?: Omit<RequestOptions, 'method' | 'body'>): Promise<T>;
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError';
}

async function toApiError(response: Response): Promise<ApiError> {
  const contentType = response.headers.get('Content-Type') ?? '';
  const requestIdHeader = response.headers.get('X-Request-ID') ?? undefined;
  if (contentType.includes('json')) {
    try {
      const problem = (await response.json()) as Partial<Problem>;
      return new ApiError({
        status: typeof problem.status === 'number' ? problem.status : response.status,
        code: problem.code || `http_${response.status}`,
        title: problem.title,
        detail: problem.detail,
        requestId: problem.request_id || requestIdHeader,
        fieldErrors: Array.isArray(problem.errors)
          ? problem.errors.filter(
              (e): e is FieldError => !!e && typeof e.field === 'string' && typeof e.message === 'string',
            )
          : [],
      });
    } catch {
      // corpo inválido: segue para o erro genérico
    }
  }
  return new ApiError({
    status: response.status,
    code: `http_${response.status}`,
    title: response.statusText || undefined,
    requestId: requestIdHeader,
  });
}

export function createHttpClient(config: HttpClientConfig): HttpClient {
  const fetchImpl = config.fetchImpl ?? ((input: RequestInfo | URL, init?: RequestInit) => fetch(input, init));

  async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
    const method = options.method ?? 'GET';
    const url = new URL(path, config.baseUrl ?? window.location.origin);
    for (const [key, value] of Object.entries(options.query ?? {})) {
      if (value !== undefined && value !== null && value !== '') url.searchParams.set(key, String(value));
    }

    const responseType = options.responseType ?? 'json';
    const headers = new Headers();
    headers.set(
      'Accept',
      responseType === 'json' ? 'application/json, application/problem+json' : '*/*',
    );
    if (options.body !== undefined) headers.set('Content-Type', 'application/json');
    if (method !== 'GET') {
      const csrf = config.getCsrfToken();
      if (csrf) headers.set('X-CSRF-Token', csrf);
    }
    if (options.tenant !== false) {
      const tenantId = config.getTenantId?.();
      if (tenantId) headers.set('X-Recorvia-Tenant', tenantId);
    }
    if (options.ifMatch !== undefined) headers.set('If-Match', `"${options.ifMatch}"`);
    if (options.idempotencyKey) headers.set('Idempotency-Key', options.idempotencyKey);

    let response: Response;
    try {
      response = await fetchImpl(url, {
        method,
        headers,
        body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
        credentials: 'same-origin',
        signal: options.signal,
      });
    } catch (error) {
      if (isAbortError(error)) throw error;
      throw new NetworkError(error);
    }

    if (!response.ok) {
      const error = await toApiError(response);
      if (error.status === 401) config.onUnauthorized?.(error);
      if (error.status === 409 && error.code === 'tenant_mismatch') config.onTenantMismatch?.(error);
      throw error;
    }

    if (response.status === 204) return undefined as T;
    if (responseType === 'text') return (await response.text()) as T;
    if (responseType === 'blob') return (await response.blob()) as T;
    const text = await response.text();
    return (text ? JSON.parse(text) : undefined) as T;
  }

  return {
    request,
    get: (path, options) => request(path, { ...options, method: 'GET' }),
    post: (path, body, options) => request(path, { ...options, method: 'POST', body }),
    put: (path, body, options) => request(path, { ...options, method: 'PUT', body }),
    patch: (path, body, options) => request(path, { ...options, method: 'PATCH', body }),
    delete: (path, options) => request(path, { ...options, method: 'DELETE' }),
  };
}

/** URL de entrada com retorno para a rota atual. */
export function loginUrl(returnTo: string): string {
  const safe = returnTo.startsWith('/') && !returnTo.startsWith('//') ? returnTo : '/';
  const limited = safe.length > 512 ? safe.split('?')[0].slice(0, 512) : safe;
  return `/auth/login?return_to=${encodeURIComponent(limited)}`;
}

type LocationLike = Pick<Location, 'pathname' | 'search' | 'hash' | 'assign'>;
let redirecting = false;

/** Redireciona o navegador inteiro para o login (não é uma chamada fetch). */
export function redirectToLogin(location: LocationLike = window.location): void {
  if (redirecting) return;
  redirecting = true;
  location.assign(loginUrl(`${location.pathname}${location.search}${location.hash}`));
}

/** Apenas para testes. */
export function resetRedirectGuard(): void {
  redirecting = false;
}
