import '@primer/css/dist/primer.css'
import '@primer/primitives/dist/css/functional/themes/light.css'
import '@primer/primitives/dist/css/functional/themes/dark.css'
import '@primer/primitives/dist/css/primitives.css'

import './theme/globals.css'
import './theme/markdown.css'
import './theme/toast.css'
import './theme/syntax.css'

import ReactDOM from 'react-dom/client'

import('./app').then(({ default: App }) => {
  ReactDOM.createRoot(document.getElementById('root')!, {
    // A crash no boundary claims still lands in the console with its
    // component stack.
    onUncaughtError: (error, info) => console.error('uncaught render error', error, info?.componentStack),
  }).render(<App />)
}).catch(() => {
  // A stale chunk after a server restart 404s here; without this the page
  // stays blank with no message.
  document.getElementById('root')!.textContent =
    'Failed to load the app — reload the page.'
})
