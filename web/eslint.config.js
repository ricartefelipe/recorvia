import js from '@eslint/js';
import { defineConfig, globalIgnores } from 'eslint/config';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';
import tseslint from 'typescript-eslint';

// ESLint 10 (configuração plana). Diretivas de desativação sem efeito e
// configurações inline desnecessárias são erros, para que exceções antigas não
// permaneçam esquecidas no código.
export default defineConfig([
  globalIgnores(['dist', 'playwright-report', 'test-results', 'blob-report', 'src/api/schema.d.ts']),
  {
    linterOptions: {
      reportUnusedDisableDirectives: 'error',
      reportUnusedInlineConfigs: 'error',
    },
  },
  {
    files: ['**/*.{ts,tsx}'],
    extends: [js.configs.recommended, tseslint.configs.recommended],
    languageOptions: {
      ecmaVersion: 2023,
      globals: { ...globals.browser },
    },
    plugins: { 'react-hooks': reactHooks },
    rules: {
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }],
      '@typescript-eslint/consistent-type-imports': 'error',
    },
  },
  {
    files: ['e2e/**/*.ts', 'playwright.config.ts', 'vite.config.ts'],
    languageOptions: { globals: { ...globals.node } },
  },
]);
