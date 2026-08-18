import type { Config } from 'tailwindcss'

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        ink: '#13201b',
        paper: '#f7f5ee',
        sage: '#dce7dc',
        pine: '#214f3b',
        coral: '#dc6b4b',
      },
      boxShadow: { panel: '0 20px 70px rgba(33,79,59,.12)' },
      fontFamily: { sans: ['Inter', 'ui-sans-serif', 'system-ui'], serif: ['Newsreader', 'Georgia', 'serif'] },
    },
  },
  plugins: [],
} satisfies Config

