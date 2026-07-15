import '@primer/css/dist/primer.css'
import '@primer/primitives/dist/css/functional/themes/light.css'
import '@primer/primitives/dist/css/functional/themes/dark.css'
import '@primer/primitives/dist/css/primitives.css'

import 'katex/dist/katex.min.css'
import './theme/globals.css'
import './theme/markdown.css'
import './theme/toast.css'
import './theme/syntax.css'

import ReactDOM from 'react-dom/client'

import('./app').then(({ default: App }) => {
  ReactDOM.createRoot(document.getElementById('root')!).render(<App />)
})
