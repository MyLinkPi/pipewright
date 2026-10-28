// ESLint flat config(ESLint v9)—— Vue 3 + TypeScript。
// 与平台 web/ 同款组合;仅保留真正有价值的检查,warning 不阻断 CI。
import js from '@eslint/js'
import tseslint from 'typescript-eslint'
import pluginVue from 'eslint-plugin-vue'
import globals from 'globals'

export default tseslint.config(
  {
    ignores: ['dist/**', 'node_modules/**', 'coverage/**'],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  ...pluginVue.configs['flat/essential'],
  {
    files: ['**/*.vue'],
    languageOptions: {
      parserOptions: { parser: tseslint.parser },
    },
    rules: {
      'no-undef': 'off',
    },
  },
  {
    languageOptions: {
      ecmaVersion: 'latest',
      sourceType: 'module',
      globals: { ...globals.browser, ...globals.node },
    },
    rules: {
      '@typescript-eslint/no-explicit-any': 'off',
      '@typescript-eslint/no-unused-vars': [
        'warn',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_', caughtErrors: 'none' },
      ],
      '@typescript-eslint/no-empty-object-type': 'off',
      'vue/multi-word-component-names': 'off',
      'vue/require-default-prop': 'off',
      'no-empty': ['warn', { allowEmptyCatch: true }],
    },
  },
  {
    files: ['**/*.test.ts', 'e2e/**', '*.config.{ts,js,mjs}'],
    languageOptions: { globals: { ...globals.node } },
  },
)
