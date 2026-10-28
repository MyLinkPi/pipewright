// bootstrap 必须作为 main.ts 的第一个 import:Vue Router 的 createWebHashHistory
// 会在模块求值时读取并重写 hash,若不先取走 #t=...,界面令牌会丢失(表现为「未授权」)。
import { initToken } from './api/http'

initToken()
