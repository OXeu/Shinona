import { createRouter, createWebHistory } from 'vue-router'

import { useAuthStore } from '../features/auth/authStore'
import { defaultProject } from '../features/sessions/projectScope'
import { useSessionStore } from '../features/sessions/sessionStore'

const agent = () => import('../features/sessions/AgentPage.vue')

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', name: 'login', component: () => import('../features/auth/LoginView.vue') },
    { path: '/', name: 'sessions', component: agent },
    { path: '/sessions/:id', name: 'session', props: true, component: agent },
    {
      path: '/projects/sandbox/:projectId?',
      name: 'project-sandbox',
      props: ({ params }) => {
        const id = params.projectId
        return { project: typeof id === 'string' && id && id !== 'default' ? { kind: 'project', id } : defaultProject }
      },
      component: () => import('../features/sessions/ProjectSandboxView.vue'),
    },
    { path: '/pinned/:id', redirect: (to) => ({ name: 'session', params: { id: to.params.id } }) },
    { path: '/settings', name: 'settings', component: () => import('../features/sessions/SettingsView.vue') },
    { path: '/account', name: 'account', component: () => import('../features/auth/AccountView.vue') },
    { path: '/admin/users', name: 'admin-users', meta: { admin: true }, component: () => import('../features/auth/AdminUsersView.vue') },
    { path: '/:pathMatch(.*)*', redirect: { name: 'sessions' } },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  const sessions = useSessionStore()
  if (to.name === 'login') {
    auth.user = null
    sessions.useAccount(undefined)
    return
  }
  try {
    await auth.refresh()
  } catch {
    auth.user = null
  }
  sessions.useAccount(auth.user?.id)
  if (!auth.user) return { name: 'login' }
  if (to.meta.admin && !auth.user.admin) return { name: 'account' }
})
