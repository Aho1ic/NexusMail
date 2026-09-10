import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import { relayOAuthResultToOpener } from './lib/oauthbridge'
import './styles.css'

// An OAuth popup loads this same bundle when the provider redirects back. It has
// one job — hand the outcome to the window that opened it — and must not mount a
// second copy of the app to do it.
if (!relayOAuthResultToOpener()) {
  ReactDOM.createRoot(document.getElementById('root')!).render(<React.StrictMode><App /></React.StrictMode>)
}

