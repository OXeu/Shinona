import { createPinia } from 'pinia'
import { createApp } from 'vue'

import { initializeLocale } from '../shared/i18n'
import App from './App.vue'
import { router } from './router'
import 'remixicon/fonts/remixicon.css'
import './style.less'

initializeLocale()

createApp(App).use(createPinia()).use(router).mount('#app')
