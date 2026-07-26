import { createRouter, createWebHistory } from 'vue-router'

import { routeRedirect } from './routeAccess.js'
import { session } from './net/session.js'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/login' },
    {
      path: '/login',
      name: 'login',
      component: () => import('./views/LoginPage.vue'),
    },
    {
      path: '/farm',
      name: 'farm',
      component: () => import('./views/FarmPage.vue'),
      meta: { requiresAuth: true },
    },
    {
      path: '/i/:token',
      name: 'invite',
      component: () => import('./views/InviteLanding.vue'),
    },
    { path: '/:pathMatch(.*)*', redirect: '/login' },
  ],
})

router.beforeEach((to) => routeRedirect(to, session.isOnline) || true)

export default router
