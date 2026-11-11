// 前端静态检查：只盯两件事——变量没声明（会直接报 ReferenceError）、声明了没用。
// 目的很单纯：漏一个 `let uploadingImages` 这种错，不许再带着上 main。
import globals from 'globals';

export default [
  {
    files: ['web/**/*.js'],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'script',
      globals: {
        ...globals.browser,
        ...globals.serviceworker,
      },
    },
    rules: {
      'no-undef': 'error',
      'no-unused-vars': ['warn', { args: 'none', caughtErrors: 'none', varsIgnorePattern: '^_' }],
    },
  },
];
