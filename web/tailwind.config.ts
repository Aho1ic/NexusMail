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
      // One radius scale for the whole app, largest first: shell wraps the app and
      // the login card, panel is every pane and dialog, card is the repeated
      // surfaces inside them (rows, chips, attachments).
      borderRadius: { shell: '2.75rem', panel: '2rem', card: '1.5rem' },
      // Elevation scale. Every step pairs a tight contact shadow with a wider
      // ambient one: a single large blur only greys a surface out, it is the
      // contact layer that reads as "this sits above that". The -Npx spreads pull
      // the ambient layer back in so stacked panes do not smear into each other.
      boxShadow: {
        panel: '0 20px 70px rgba(33,79,59,.12)',
        'lift-1': '0 1px 2px rgba(33,79,59,.05), 0 4px 12px -3px rgba(33,79,59,.08)',
        'lift-2': '0 2px 4px rgba(33,79,59,.05), 0 12px 28px -8px rgba(33,79,59,.12)',
        'lift-3': '0 3px 6px rgba(33,79,59,.06), 0 18px 40px -10px rgba(33,79,59,.15)',
        'lift-4': '0 5px 12px rgba(33,79,59,.07), 0 28px 62px -14px rgba(33,79,59,.19)',
        // The inset hairline is what gives a light surface a visible top edge, so
        // it is baked into these tokens rather than applied as a second class —
        // two box-shadow utilities on one element would just overwrite each other.
        glass: 'inset 0 1px 0 rgba(255,255,255,.6), 0 2px 4px rgba(33,79,59,.05), 0 12px 28px -8px rgba(33,79,59,.12)',
        'glass-high': 'inset 0 1px 0 rgba(255,255,255,.85), 0 5px 12px rgba(33,79,59,.07), 0 28px 62px -14px rgba(33,79,59,.19)',
        // On pine the highlight has to be a fraction of the light one to read as
        // an edge instead of a white line.
        'glass-dark': 'inset 0 1px 0 rgba(255,255,255,.12), 0 2px 4px rgba(19,32,27,.12), 0 14px 32px -10px rgba(19,32,27,.34)',
        // The shell is one thick object floating over the stage: three ambient
        // layers so the falloff stays smooth at that size.
        stage: 'inset 0 1px 0 rgba(255,255,255,.7), 0 2px 6px rgba(33,79,59,.05), 0 20px 44px -14px rgba(33,79,59,.13), 0 54px 116px -34px rgba(33,79,59,.2)',
      },
      fontFamily: { sans: ['Inter', 'ui-sans-serif', 'system-ui'], serif: ['Newsreader', 'Georgia', 'serif'] },
    },
  },
  plugins: [],
} satisfies Config
