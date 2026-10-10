import '@primer/css/dist/base.css'
import '@primer/css/dist/markdown.css'
import '@primer/primitives/dist/css/functional/themes/light.css'
import '@primer/primitives/dist/css/functional/themes/dark.css'
import '@primer/primitives/dist/css/primitives.css'

import './theme/globals.css'
import './theme/markdown.css'
import './theme/toast.css'
import './theme/syntax.css'

import ReactDOM from 'react-dom/client'
import { ensureSchemeCss, readScheme } from './theme/themes'

// The stored scheme's theme files land with the app, so the first paint is
// the right theme and not the default one.
Promise.all([import('./app'), ensureSchemeCss(readScheme())]).then(([{ default: App }]) => {
  ReactDOM.createRoot(document.getElementById('root')!, {
    // A crash no boundary claims still lands in the console with its
    // component stack.
    onUncaughtError: (error, info) => console.error('uncaught render error', error, info?.componentStack),
  }).render(<App />)
}).catch(() => {
  // A stale chunk after a server restart 404s here: say so, not a blank page. An
  // element, not a text node: index.html hides #root until it has a child element.
  const p = document.createElement('p')
  p.textContent = 'Failed to load the app — reload the page.'
  document.getElementById('root')!.replaceChildren(p)
})
