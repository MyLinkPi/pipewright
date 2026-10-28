import { createRouter, createWebHashHistory } from 'vue-router'

// hash 路由:#/overview 等。WebView2 与系统浏览器都不依赖服务端 rewrite。
const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', redirect: '/overview' },
    { path: '/overview', component: () => import('./views/OverviewView.vue') },
    { path: '/peers', component: () => import('./views/PeersView.vue') },
    { path: '/routes', component: () => import('./views/RoutesView.vue') },
    { path: '/proxy', component: () => import('./views/ProxyView.vue') },
    { path: '/config', component: () => import('./views/ConfigOverviewView.vue') },
    { path: '/config/connection', component: () => import('./views/ConnectionConfigView.vue') },
    { path: '/aliyun', component: () => import('./views/AliyunView.vue') },
    { path: '/logs', component: () => import('./views/LogsView.vue') },
    { path: '/settings', component: () => import('./views/SettingsView.vue') },
    { path: '/:pathMatch(.*)*', redirect: '/overview' },
  ],
})

export default router
