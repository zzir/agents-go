import tseslint from 'typescript-eslint';
import reactHooks from 'eslint-plugin-react-hooks';

export default tseslint.config(
  { ignores: ['dist/**', 'src/lib/apiTypes.gen.ts'] },
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      ...tseslint.configs.recommended,
      reactHooks.configs.flat['recommended-latest'],
    ],
    // A disable comment that suppresses nothing is a lie waiting to mislead.
    linterOptions: { reportUnusedDisableDirectives: 'error' },
    rules: {
      '@typescript-eslint/no-unused-vars': ['error', {
        argsIgnorePattern: '^_', varsIgnorePattern: '^_', destructuredArrayIgnorePattern: '^_', caughtErrorsIgnorePattern: '^_',
      }],
      'react-hooks/exhaustive-deps': 'error',
      // v7's compiler-derived rules flag patterns this codebase uses on
      // purpose: latest-value refs written during render, cache-hit setState
      // inside effects, useApi's caller-supplied dependency list, and the
      // tests' IS_REACT_ACT_ENVIRONMENT global. Rules-of-hooks and
      // exhaustive-deps carry the actual safety weight.
      'react-hooks/refs': 'off',
      'react-hooks/set-state-in-effect': 'off',
      'react-hooks/use-memo': 'off',
      'react-hooks/immutability': 'off',
      'react-hooks/globals': 'off',
    },
  },
);
